package k8snode

import (
	"slices"
	"strconv"
)

// The placement checks below decide whether a controller can recover while
// the node is out. They model the parts of the Kubernetes scheduler the Bash
// tool modelled in jq, with the same deliberate simplifications.

const (
	hostnameLabel    = "kubernetes.io/hostname"
	unschedulableKey = "node.kubernetes.io/unschedulable"
)

// requirementMatches applies a node selector requirement to a label value;
// present is false when the node lacks the label.
func requirementMatches(value string, present bool, r requirement) bool {
	switch r.Operator {
	case "In":
		return present && slices.Contains(r.Values, value)
	case "NotIn":
		// As in the scheduler, a node without the label matches.
		return !present || !slices.Contains(r.Values, value)
	case "Exists":
		return present
	case "DoesNotExist":
		return !present
	case "Gt", "Lt":
		if !present {
			return false
		}
		actual, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return false
		}
		expectedText := ""
		if len(r.Values) > 0 {
			expectedText = r.Values[0]
		}
		expected, err := strconv.ParseFloat(expectedText, 64)
		if err != nil {
			return false
		}
		if r.Operator == "Gt" {
			return actual > expected
		}
		return actual < expected
	}
	return false
}

func nodeSelectorMatches(n node, selector map[string]string) bool {
	for key, value := range selector {
		actual, ok := n.Metadata.Labels[key]
		if !ok || actual != value {
			return false
		}
	}
	return true
}

func nodeAffinityTermMatches(n node, term nodeSelectorTerm) bool {
	for _, r := range term.MatchExpressions {
		value, present := n.Metadata.Labels[r.Key]
		if !requirementMatches(value, present, r) {
			return false
		}
	}
	for _, r := range term.MatchFields {
		// metadata.name is the only field a node selector can match.
		if !requirementMatches(n.Metadata.Name, r.Key == "metadata.name", r) {
			return false
		}
	}
	return true
}

func requiredNodeAffinityMatches(n node, a *affinity) bool {
	if a == nil || a.NodeAffinity == nil || a.NodeAffinity.Required == nil {
		return true
	}
	terms := a.NodeAffinity.Required.NodeSelectorTerms
	if len(terms) == 0 {
		return true
	}
	for _, term := range terms {
		if nodeAffinityTermMatches(n, term) {
			return true
		}
	}
	return false
}

// podMatchesNode reports whether the Pod's node selector and required node
// affinity admit the node.
func podMatchesNode(spec podSpec, n node) bool {
	return nodeSelectorMatches(n, spec.NodeSelector) && requiredNodeAffinityMatches(n, spec.Affinity)
}

// requiresTargetNode reports whether a Pod template can run only on the
// target: it matches the target and no other Ready, schedulable node.
func requiresTargetNode(target string, spec podSpec, nodes []node) bool {
	found := false
	for _, n := range nodes {
		if n.Metadata.Name == target {
			found = true
			if !podMatchesNode(spec, n) {
				return false
			}
			break
		}
	}
	if !found {
		return false
	}
	for _, n := range nodes {
		if n.Metadata.Name != target && !n.Spec.Unschedulable && nodeIsReady(n) && podMatchesNode(spec, n) {
			return false
		}
	}
	return true
}

// workloadRequiresTargetNode reports whether the controller's Pods can run
// only on the target node. A controller or node list kubectl cannot read
// counts as portable, as in the Bash tool.
func (o *Operation) workloadRequiresTargetNode(w workload) (bool, error) {
	var c controller
	found, err := o.readQuiet(&c, "-n", w.Namespace, "get", w.ref(), "-o", "json")
	if err != nil || !found {
		return false, err
	}
	var nodes nodeList
	found, err = o.readQuiet(&nodes, "get", "nodes", "-o", "json")
	if err != nil || !found {
		return false, err
	}
	return requiresTargetNode(o.opts.Node, c.Spec.Template.Spec, nodes.Items), nil
}

// selectorMatches applies a label selector as Kubernetes does: an empty
// one selects every Pod, and NotIn and DoesNotExist select a Pod without
// the label. A nil selector selects none.
func selectorMatches(labels map[string]string, s *labelSelector) bool {
	if s == nil || !labelsMatch(labels, s.MatchLabels) {
		return false
	}
	for _, r := range s.MatchExpressions {
		value, present := labels[r.Key]
		if !requirementMatches(value, present, r) {
			return false
		}
	}
	return true
}

// labelsMatch reports whether labels carry every key and value of match.
func labelsMatch(labels, match map[string]string) bool {
	for key, value := range match {
		actual, ok := labels[key]
		if !ok || actual != value {
			return false
		}
	}
	return true
}

// simpleSelector reports whether a label selector is a non-empty set of
// matchLabels without expressions, the only form the floor models.
func simpleSelector(s *labelSelector) bool {
	return s != nil && len(s.MatchExpressions) == 0 && len(s.MatchLabels) > 0
}

// onePerNode reports whether a required anti-affinity on the Pods' own
// labels allows at most one of them per node.
func onePerNode(spec podSpec, labels map[string]string, namespace string) bool {
	if spec.Affinity == nil || spec.Affinity.PodAntiAffinity == nil {
		return false
	}
	for _, term := range spec.Affinity.PodAntiAffinity.Required {
		if term.TopologyKey == hostnameLabel &&
			term.NamespaceSelector == nil &&
			(len(term.Namespaces) == 0 || slices.Contains(term.Namespaces, namespace)) &&
			len(term.MatchLabelKeys) == 0 &&
			len(term.MismatchLabelKeys) == 0 &&
			simpleSelector(term.LabelSelector) &&
			labelsMatch(labels, term.LabelSelector.MatchLabels) {
			return true
		}
	}
	return false
}

// hostnameSpread returns the Pods' strict hostname spread on their own
// labels, or nil. Other spread forms keep the full replica count.
func hostnameSpread(spec podSpec, labels map[string]string) *spreadConstraint {
	var strict []spreadConstraint
	for _, c := range spec.TopologySpreadConstraints {
		if c.WhenUnsatisfiable == "DoNotSchedule" {
			strict = append(strict, c)
		}
	}
	if len(strict) != 1 {
		return nil
	}
	c := strict[0]
	if c.TopologyKey == hostnameLabel &&
		c.MaxSkew >= 1 &&
		(c.NodeAffinityPolicy == "" || c.NodeAffinityPolicy == "Honor") &&
		c.MinDomains == nil &&
		len(c.MatchLabelKeys) == 0 &&
		simpleSelector(c.LabelSelector) &&
		labelsMatch(labels, c.LabelSelector.MatchLabels) {
		return &c
	}
	return nil
}

func toleratesTaint(spec podSpec, t taint) bool {
	for _, tol := range spec.Tolerations {
		operator := tol.Operator
		if operator == "" {
			operator = "Equal"
		}
		keyMatches := (tol.Key == "" && operator == "Exists") || tol.Key == t.Key
		valueMatches := operator == "Exists" || tol.Value == t.Value
		effectMatches := tol.Effect == "" || tol.Effect == t.Effect
		if keyMatches && valueMatches && effectMatches {
			return true
		}
	}
	return false
}

// nodeAdmits reports whether the Pods tolerate every taint that blocks
// scheduling on the node, other than the cordon itself.
func nodeAdmits(spec podSpec, n node) bool {
	for _, t := range n.Spec.Taints {
		if t.Effect != "NoSchedule" && t.Effect != "NoExecute" {
			continue
		}
		if t.Key == unschedulableKey {
			continue
		}
		if !toleratesTaint(spec, t) {
			return false
		}
	}
	return true
}

// livePod reports whether a Pod is bound, not finished and not deleting.
func livePod(p pod) bool {
	return p.Metadata.DeletionTimestamp == nil && p.active() && p.Spec.NodeName != ""
}

// toleratesCordon reports whether the Pods may be scheduled back on the
// cordoned node.
func toleratesCordon(spec podSpec) bool {
	for _, tol := range spec.Tolerations {
		operator := tol.Operator
		if operator == "" {
			operator = "Equal"
		}
		if (tol.Key == unschedulableKey || (tol.Key == "" && operator == "Exists")) &&
			(operator == "Exists" || (operator == "Equal" && tol.Value == "")) &&
			(tol.Effect == "" || tol.Effect == "NoSchedule") {
			return true
		}
	}
	return false
}

// readyFloor returns the Ready-replica count a Deployment can meet while the
// target node is out. A required one-per-node anti-affinity on the Pods' own
// labels caps it at the other Ready nodes the Pods can use: schedulable ones,
// plus nodes still running a live replica. A strict hostname spread instead
// replays the scheduler's skew check to find how many replicas can still
// run. The floor never drops below one. Pods that tolerate the cordon (they
// may land back on the target) and selector forms this check does not model
// keep the full replica count.
func readyFloor(target, namespace string, d controller, nodes []node, pods []pod) int {
	desired := d.desiredReplicas()
	spec := d.Spec.Template.Spec
	labels := d.Spec.Template.Metadata.Labels
	selector := d.Spec.Selector
	spread := hostnameSpread(spec, labels)
	perNode := onePerNode(spec, labels, namespace)
	if desired <= 1 || toleratesCordon(spec) || !simpleSelector(selector) || (!perNode && spread == nil) {
		return desired
	}

	var replicas []pod
	for _, p := range pods {
		if labelsMatch(p.Metadata.Labels, selector.MatchLabels) && livePod(p) {
			replicas = append(replicas, p)
		}
	}

	if perNode {
		var runningNodes []string
		for _, p := range replicas {
			runningNodes = append(runningNodes, p.Spec.NodeName)
		}
		eligible := 0
		for _, n := range nodes {
			if n.Metadata.Name == target || !nodeIsReady(n) {
				continue
			}
			if slices.Contains(runningNodes, n.Metadata.Name) ||
				(podMatchesNode(spec, n) && !n.Spec.Unschedulable && nodeAdmits(spec, n)) {
				eligible++
			}
		}
		return min(desired, max(eligible, 1))
	}

	// Replay the scheduler spread filter: count every live Pod the
	// constraint selects per node domain (DaemonSet Pods included), then
	// place the missing replicas one at a time on schedulable nodes while
	// the skew stays within maxSkew.
	running := 0
	for _, p := range replicas {
		if p.Spec.NodeName != target && p.Metadata.hasOwner("ReplicaSet") {
			running++
		}
	}
	type domain struct {
		name     string
		hostname string
		count    int
		open     bool
	}
	var domains []domain
	for _, n := range nodes {
		name := n.Metadata.Name
		hostname := n.Metadata.Labels[hostnameLabel]
		if !podMatchesNode(spec, n) || hostname == "" {
			continue
		}
		admitted := !n.Spec.Unschedulable && name != target && nodeAdmits(spec, n)
		if spread.NodeTaintsPolicy != "" && spread.NodeTaintsPolicy != "Ignore" && !admitted {
			continue
		}
		count := 0
		for _, p := range pods {
			if !livePod(p) || p.Spec.NodeName != name ||
				!labelsMatch(p.Metadata.Labels, spread.LabelSelector.MatchLabels) {
				continue
			}
			// The drain removes this Deployment from the target; other
			// selected Pods there, such as DaemonSet Pods, remain.
			if name == target && labelsMatch(p.Metadata.Labels, selector.MatchLabels) && p.Metadata.hasOwner("ReplicaSet") {
				continue
			}
			count++
		}
		domains = append(domains, domain{
			name:     name,
			hostname: hostname,
			count:    count,
			open:     admitted && nodeIsReady(n),
		})
	}

	// Shared hostname values would merge domains; keep every replica.
	hostnames := map[string]bool{}
	for _, d := range domains {
		hostnames[d.hostname] = true
	}
	if len(domains) == 0 || len(hostnames) != len(domains) || running >= desired {
		return desired
	}

	placed := 0
	for range desired - running {
		minimum := domains[0].count
		for _, d := range domains {
			minimum = min(minimum, d.count)
		}
		pick := -1
		for i, d := range domains {
			if d.open && d.count+1-minimum <= spread.MaxSkew && (pick < 0 || d.count < domains[pick].count) {
				pick = i
			}
		}
		if pick >= 0 {
			domains[pick].count++
			placed++
		}
	}
	return min(desired, max(running+placed, 1))
}

// deploymentReadyFloor reads the Deployment, the nodes and the namespace's
// Pods and returns readyFloor. ok is false when kubectl cannot read any of
// them, and the caller then requires every replica.
func (o *Operation) deploymentReadyFloor(w workload) (floor int, ok bool, err error) {
	var d controller
	if found, err := o.readQuiet(&d, "-n", w.Namespace, "get", w.ref(), "-o", "json"); err != nil || !found {
		return 0, false, err
	}
	var nodes nodeList
	if found, err := o.readQuiet(&nodes, "get", "nodes", "-o", "json"); err != nil || !found {
		return 0, false, err
	}
	var pods podList
	if found, err := o.readQuiet(&pods, "-n", w.Namespace, "get", "pods", "-o", "json"); err != nil || !found {
		return 0, false, err
	}
	return readyFloor(o.opts.Node, w.Namespace, d, nodes.Items, pods.Items), true, nil
}
