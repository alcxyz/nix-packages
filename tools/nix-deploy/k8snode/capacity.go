package k8snode

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
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

// resources are a CPU amount in millicores and a memory amount in bytes.
type resources struct {
	cpu    int64
	memory int64
}

func (r resources) plus(other resources) resources {
	return resources{r.cpu + other.cpu, r.memory + other.memory}
}

func (r resources) minus(other resources) resources {
	return resources{r.cpu - other.cpu, r.memory - other.memory}
}

func (r resources) fits(room resources) bool {
	return r.cpu <= room.cpu && r.memory <= room.memory
}

func (r resources) String() string {
	return fmt.Sprintf("cpu=%dm memory=%dMi", r.cpu, r.memory>>20)
}

var quantityPattern = regexp.MustCompile(`^([0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:([numkMGTPE]|[KMGTPE]i)|[eE]([+-]?[0-9]+))?$`)

var quantitySuffixes = map[string]float64{
	"": 1, "n": 1e-9, "u": 1e-6, "m": 1e-3,
	"k": 1e3, "M": 1e6, "G": 1e9, "T": 1e12, "P": 1e15, "E": 1e18,
	"Ki": 1 << 10, "Mi": 1 << 20, "Gi": 1 << 30, "Ti": 1 << 40, "Pi": 1 << 50, "Ei": 1 << 60,
}

// parseQuantity reads a Kubernetes resource quantity, scaled by scale
// (1000 for millicores) and rounded up as the scheduler rounds requests.
func parseQuantity(text string, scale float64) (int64, error) {
	match := quantityPattern.FindStringSubmatch(text)
	if match == nil {
		return 0, fmt.Errorf("unsupported quantity: %q", text)
	}
	value, err := strconv.ParseFloat(match[1], 64)
	if err != nil {
		return 0, fmt.Errorf("unsupported quantity: %q", text)
	}
	value *= quantitySuffixes[match[2]]
	if match[3] != "" {
		exponent, err := strconv.Atoi(match[3])
		if err != nil {
			return 0, fmt.Errorf("unsupported quantity: %q", text)
		}
		value *= math.Pow10(exponent)
	}
	return int64(math.Ceil(value*scale - 1e-9)), nil
}

// readResources reads the cpu and memory entries of a request or
// allocatable list; a missing entry is zero.
func readResources(list map[string]string) (resources, error) {
	var r resources
	var err error
	if text, ok := list["cpu"]; ok {
		if r.cpu, err = parseQuantity(text, 1000); err != nil {
			return r, err
		}
	}
	if text, ok := list["memory"]; ok {
		if r.memory, err = parseQuantity(text, 1); err != nil {
			return r, err
		}
	}
	return r, nil
}

// podRequests is what the scheduler reserves for a Pod: its containers and
// sidecar init containers, or the largest init container while it runs with
// the sidecars started before it, whichever is more, plus the Pod overhead.
func podRequests(spec podSpec) (resources, error) {
	var app resources
	for _, c := range spec.Containers {
		r, err := readResources(c.Resources.Requests)
		if err != nil {
			return app, err
		}
		app = app.plus(r)
	}
	var sidecars, init resources
	for _, c := range spec.InitContainers {
		r, err := readResources(c.Resources.Requests)
		if err != nil {
			return app, err
		}
		if c.RestartPolicy == "Always" {
			sidecars = sidecars.plus(r)
			continue
		}
		running := r.plus(sidecars)
		init = resources{max(init.cpu, running.cpu), max(init.memory, running.memory)}
	}
	app = app.plus(sidecars)
	total := resources{max(app.cpu, init.cpu), max(app.memory, init.memory)}
	overhead, err := readResources(spec.Overhead)
	if err != nil {
		return total, err
	}
	return total.plus(overhead), nil
}

// roomyNode is a node that can take displaced Pods, with the CPU and
// memory its Pods' requests leave free.
type roomyNode struct {
	node node
	free resources
	// claims are the controller selectors of the Pods with a one-per-node
	// anti-affinity placed on it, by namespace.
	claims []claim
}

type claim struct {
	namespace string
	selector  *labelSelector
}

// displacedPod is a Pod the drain evicts that the availability gate needs
// placed on another node.
type displacedPod struct {
	pod      pod
	requests resources
	// selector is its controller's selector when the Pods allow one per
	// node, else nil.
	selector *labelSelector
}

// checkDisplacedPodsFit requires that the Pods the availability gate needs
// after the drain fit, by their CPU and memory requests, on the remaining
// Ready, schedulable nodes they may run on. It places them largest first on
// the node with the most free CPU, like the default scheduler's spreading
// score, and models node selectors, required node affinity, taints and a
// one-per-node anti-affinity, not spread constraints or preferences. The
// availability gate after the drain still has the final word.
func (o *Operation) checkDisplacedPodsFit() error {
	target := o.opts.Node
	o.log("checking that the Pods drained from %s fit on the remaining nodes by their CPU and memory requests", target)
	var nodes nodeList
	if err := o.getJSON(&nodes, "get", "nodes", "-o", "json"); err != nil {
		return err
	}
	pods, err := o.listAllPods()
	if err != nil {
		return err
	}

	var room []*roomyNode
	for _, n := range nodes.Items {
		if n.Metadata.Name == target || n.Spec.Unschedulable || !nodeIsReady(n) {
			continue
		}
		allocatable, err := readResources(n.Status.Allocatable)
		if err != nil {
			return fmt.Errorf("%s: allocatable resources: %v", n.Metadata.Name, err)
		}
		room = append(room, &roomyNode{node: n, free: allocatable})
	}
	for _, p := range pods {
		if !p.active() {
			continue
		}
		for _, r := range room {
			if p.Spec.NodeName == r.node.Metadata.Name {
				requests, err := podRequests(p.Spec)
				if err != nil {
					return fmt.Errorf("%s/%s: %v", p.Metadata.Namespace, p.Metadata.Name, err)
				}
				r.free = r.free.minus(requests)
			}
		}
	}

	displaced, err := o.displacedPodsToPlace(nodes.Items, pods)
	if err != nil {
		return err
	}
	sort.SliceStable(displaced, func(i, j int) bool {
		a, b := displaced[i].requests, displaced[j].requests
		if a.cpu != b.cpu {
			return a.cpu > b.cpu
		}
		return a.memory > b.memory
	})
	before := make([]resources, len(room))
	for i, r := range room {
		before[i] = r.free
	}
	var unplaced []string
	for _, d := range displaced {
		var pick *roomyNode
		for _, r := range room {
			if !d.requests.fits(r.free) || !podMatchesNode(d.pod.Spec, r.node) || !nodeAdmits(d.pod.Spec, r.node) ||
				claimed(r, d, pods) {
				continue
			}
			if pick == nil || r.free.cpu > pick.free.cpu {
				pick = r
			}
		}
		if pick == nil {
			unplaced = append(unplaced, fmt.Sprintf("%s/%s\t%s\tno remaining node has room", d.pod.Metadata.Namespace, d.pod.Metadata.Name, d.requests))
			continue
		}
		pick.free = pick.free.minus(d.requests)
		if d.selector != nil {
			pick.claims = append(pick.claims, claim{d.pod.Metadata.Namespace, d.selector})
		}
	}

	fmt.Fprintf(o.stdout, "remaining-node\tfree-before\tfree-after\n")
	for i, r := range room {
		fmt.Fprintf(o.stdout, "%s\t%s\t%s\n", r.node.Metadata.Name, before[i], r.free)
	}
	fmt.Fprintf(o.stdout, "displaced-pods\t%d\tunplaced\t%d\n", len(displaced), len(unplaced))
	if len(unplaced) > 0 {
		o.report(unplaced)
		return fmt.Errorf("the remaining nodes cannot hold the Pods drained from %s by their CPU and memory requests; free capacity or lower requests before maintenance", target)
	}
	return nil
}

// claimed reports whether a one-per-node Pod would join a replica of its
// controller already running on the node, or placed there by the check.
func claimed(r *roomyNode, d displacedPod, pods []pod) bool {
	if d.selector == nil {
		return false
	}
	namespace := d.pod.Metadata.Namespace
	for _, c := range r.claims {
		if c.namespace == namespace && c.selector == d.selector {
			return true
		}
	}
	for _, p := range pods {
		if livePod(p) && p.Spec.NodeName == r.node.Metadata.Name && p.Metadata.Namespace == namespace &&
			pdbSelectorMatches(p.Metadata.Labels, d.selector) {
			return true
		}
	}
	return false
}

// displacedPodsToPlace lists the target's Pods that the availability gate
// requires elsewhere: of each displaced controller the gate checks, as many
// as its Ready replicas on other nodes fall short of its minimum. Pods of
// controllers that can run only on the target wait for it instead.
func (o *Operation) displacedPodsToPlace(nodes []node, pods []pod) ([]displacedPod, error) {
	target := o.opts.Node
	byWorkload := map[workload][]pod{}
	var order []workload
	for _, p := range pods {
		if p.Spec.NodeName != target || !p.active() || p.Metadata.hasOwner("DaemonSet") ||
			p.Metadata.Annotations["kubernetes.io/config.mirror"] != "" ||
			p.Metadata.Labels["longhorn.io/component"] == "instance-manager" {
			continue
		}
		w, ok, err := o.workloadForPod(p)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if _, seen := byWorkload[w]; !seen {
			order = append(order, w)
		}
		byWorkload[w] = append(byWorkload[w], p)
	}

	var displaced []displacedPod
	for _, w := range uniqueWorkloads(order) {
		minimum, _, gated, err := o.readyMinimum(w)
		if err != nil {
			return nil, err
		}
		if !gated {
			continue
		}
		var c controller
		found, err := o.readQuiet(&c, "-n", w.Namespace, "get", w.ref(), "-o", "json")
		if err != nil {
			return nil, err
		}
		if found && requiresTargetNode(target, c.Spec.Template.Spec, nodes) {
			continue
		}
		onTarget := byWorkload[w]
		readyOnTarget := 0
		for _, p := range onTarget {
			if p.runningAndReady() {
				readyOnTarget++
			}
		}
		need := minimum - (c.Status.ReadyReplicas - readyOnTarget)
		need = min(max(need, 0), len(onTarget))
		var selector *labelSelector
		if found && onePerNode(c.Spec.Template.Spec, c.Spec.Template.Metadata.Labels, w.Namespace) {
			selector = c.Spec.Selector
		}
		for _, p := range onTarget[:need] {
			requests, err := podRequests(p.Spec)
			if err != nil {
				return nil, fmt.Errorf("%s/%s: %v", p.Metadata.Namespace, p.Metadata.Name, err)
			}
			displaced = append(displaced, displacedPod{pod: p, requests: requests, selector: selector})
		}
	}
	return displaced, nil
}
