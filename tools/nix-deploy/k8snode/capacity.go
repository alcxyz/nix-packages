package k8snode

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

type podDisruptionBudget struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		Selector *labelSelector `json:"selector"`
	} `json:"spec"`
	Status struct {
		DisruptionsAllowed int `json:"disruptionsAllowed"`
	} `json:"status"`
}

// pdbSelectorMatches applies a PodDisruptionBudget's label selector, with
// the Bash tool's semantics: NotIn requires the label, an empty selector
// matches every Pod.
func pdbSelectorMatches(labels map[string]string, selector *labelSelector) bool {
	if selector == nil {
		return true
	}
	if !labelsMatch(labels, selector.MatchLabels) {
		return false
	}
	for _, e := range selector.MatchExpressions {
		value, present := labels[e.Key]
		var ok bool
		switch e.Operator {
		case "In":
			ok = present && slices.Contains(e.Values, value)
		case "NotIn":
			ok = present && !slices.Contains(e.Values, value)
		case "Exists":
			ok = present
		case "DoesNotExist":
			ok = !present
		}
		if !ok {
			return false
		}
	}
	return true
}

// checkPDBEvictionBlockers fails when a PodDisruptionBudget that allows no
// disruption covers an evictable Pod on the node, instead of letting the
// drain wait out its timeout. Before the switchover, CloudNativePG primaries
// are left to it.
func (o *Operation) checkPDBEvictionBlockers(ignoreCNPGPrimaries bool) error {
	node := o.opts.Node
	o.log("checking PodDisruptionBudgets on %s", node)
	var pdbs struct {
		Items []podDisruptionBudget `json:"items"`
	}
	if err := o.getJSON(&pdbs, "get", "poddisruptionbudgets.policy", "--all-namespaces", "-o", "json"); err != nil {
		return err
	}
	pods, err := o.listAllPods()
	if err != nil {
		return err
	}

	var rows []string
	for _, pdb := range pdbs.Items {
		if pdb.Status.DisruptionsAllowed >= 1 {
			continue
		}
		// As in the Bash tool, the budget's namespace is not compared: a
		// matching Pod anywhere on the node blocks.
		for _, p := range pods {
			if p.Spec.NodeName != node || !p.active() || p.Metadata.hasOwner("DaemonSet") {
				continue
			}
			if p.Metadata.Labels["longhorn.io/component"] == "instance-manager" {
				continue
			}
			if !pdbSelectorMatches(p.Metadata.Labels, pdb.Spec.Selector) {
				continue
			}
			role, hasRole := p.Metadata.Labels["cnpg.io/instanceRole"]
			if ignoreCNPGPrimaries && role == "primary" {
				continue
			}
			if !hasRole {
				role = "-"
			}
			rows = append(rows, strings.Join([]string{p.Metadata.Namespace, pdb.Metadata.Name, p.Metadata.Name, role}, "\t"))
		}
	}
	if len(rows) > 0 {
		o.report(append([]string{"namespace\tpdb\tpod\tinstance-role"}, rows...))
		return fmt.Errorf("PodDisruptionBudgets block eviction; restore replicas or switchover primaries before disrupting %s", node)
	}
	return nil
}

// checkStableNodePodSlots requires enough aggregate Pod slots on the
// remaining Ready, schedulable stable nodes for the node's evictable Pods
// plus the reserve.
func (o *Operation) checkStableNodePodSlots() error {
	target := o.opts.Node
	required := o.opts.MinFreePodSlots
	o.log("checking remaining stable-node aggregate Pod capacity (%d slots reserved)", required)
	var nodes nodeList
	if err := o.getJSON(&nodes, "get", "nodes", "-l", "workload-class=stable", "-o", "json"); err != nil {
		return err
	}
	pods, err := o.listAllPods()
	if err != nil {
		return err
	}

	var remaining []string
	allocatable := 0
	for _, n := range nodes.Items {
		if n.Metadata.Name == target || n.Spec.Unschedulable || !nodeIsReady(n) {
			continue
		}
		slots, err := strconv.Atoi(n.Status.Allocatable["pods"])
		if err != nil {
			return fmt.Errorf("%s has no readable allocatable Pod count", n.Metadata.Name)
		}
		remaining = append(remaining, n.Metadata.Name)
		allocatable += slots
	}
	scheduled := 0
	displaced := 0
	for _, p := range pods {
		if !p.active() {
			continue
		}
		if slices.Contains(remaining, p.Spec.NodeName) {
			scheduled++
		}
		if p.Spec.NodeName == target && !p.Metadata.hasOwner("DaemonSet") && p.Metadata.Annotations["kubernetes.io/config.mirror"] == "" {
			displaced++
		}
	}
	freeAfter := allocatable - scheduled - displaced
	status := "insufficient"
	if len(remaining) > 0 && freeAfter >= required {
		status = "pass"
	}
	fmt.Fprintf(o.stdout, "remaining-nodes\tscheduled\tdisplaced\tallocatable\tfree-after\tstatus\n%s\t%d\t%d\t%d\t%d\t%s\n",
		strings.Join(remaining, ","), scheduled, displaced, allocatable, freeAfter, status)
	if status != "pass" {
		return fmt.Errorf("remaining stable nodes do not have enough aggregate Pod slots")
	}
	return nil
}
