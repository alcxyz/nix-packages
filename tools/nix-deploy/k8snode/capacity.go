package k8snode

import (
	"fmt"
	"maps"
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
// the sidecars started before it, whichever is more, or instead the
// Pod-level request for a resource it names, plus the Pod overhead.
func podRequests(spec podSpec) (resources, error) {
	return heldRequests(spec, nil, false)
}

// heldRequests is podRequests with each container's request raised to what
// the node holds for it, by container name, as during an in-place resize.
// For a resize the node cannot grant, what it holds replaces the request.
func heldRequests(spec podSpec, held map[string]resources, infeasible bool) (resources, error) {
	request := func(c container) (resources, error) {
		r, err := readResources(c.Resources.Requests)
		if h, ok := held[c.Name]; ok {
			if infeasible {
				r = h
			} else {
				r = resources{max(r.cpu, h.cpu), max(r.memory, h.memory)}
			}
		}
		return r, err
	}
	var app resources
	for _, c := range spec.Containers {
		r, err := request(c)
		if err != nil {
			return app, err
		}
		app = app.plus(r)
	}
	var sidecars, init resources
	for _, c := range spec.InitContainers {
		r, err := request(c)
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
	podLevel, err := readResources(spec.Resources.Requests)
	if err != nil {
		return total, err
	}
	if _, ok := spec.Resources.Requests["cpu"]; ok {
		total.cpu = podLevel.cpu
	}
	if _, ok := spec.Resources.Requests["memory"]; ok {
		total.memory = podLevel.memory
	}
	overhead, err := readResources(spec.Overhead)
	if err != nil {
		return total, err
	}
	return total.plus(overhead), nil
}

// roomyNode is a node that can take displaced Pods, with the CPU and
// memory its Pods' requests leave free.
type roomyNode struct {
	node        node
	allocatable resources
	free        resources
	// placed are the Pods the check placed on it.
	placed []pod
}

// displacedPod is a Pod the drain evicts that the availability gate needs
// placed on another node.
type displacedPod struct {
	pod      pod
	requests resources
}

// checkDisplacedPodsFit requires that the Pods the availability gate needs
// after the drain fit, by their controllers' CPU and memory requests, on the
// remaining Ready, schedulable nodes they may run on. It places them largest
// first on the node with the largest free share of CPU and memory, like the
// default scheduler's spreading score, searching for another placement when
// that leaves one out, and models node selectors, required node affinity,
// taints and required hostname anti-affinity both ways, not spread
// constraints or preferences. The drain's other Pods are placed after them
// and only warned about: the scheduler may place them first, so the
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
		room = append(room, &roomyNode{node: n, allocatable: allocatable, free: allocatable})
	}
	for _, p := range pods {
		if !p.active() {
			continue
		}
		for _, r := range room {
			if p.Spec.NodeName == r.node.Metadata.Name {
				requests, err := residentRequests(p)
				if err != nil {
					return fmt.Errorf("%s/%s: %v", p.Metadata.Namespace, p.Metadata.Name, err)
				}
				r.free = r.free.minus(requests)
			}
		}
	}

	needed, others, err := o.displacedPodsToPlace(nodes.Items, pods)
	if err != nil {
		return err
	}
	before := make([]resources, len(room))
	for i, r := range room {
		before[i] = r.free
	}
	unplaced, unsure := placeNeeded(room, needed, pods, packBudget)
	// Another Pod no node may run waits for the target by design; only a
	// shortage of room is worth a warning.
	unplacedOthers, _ := place(room, others, pods)

	fmt.Fprintf(o.stdout, "remaining-node\tfree-before\tfree-after\n")
	for i, r := range room {
		fmt.Fprintf(o.stdout, "%s\t%s\t%s\n", r.node.Metadata.Name, before[i], r.free)
	}
	fmt.Fprintf(o.stdout, "needed-pods\t%d\tunplaced\t%d\tunsettled\t%d\tother-pods\t%d\tunplaced\t%d\n",
		len(needed), len(unplaced), len(unsure), len(others), len(unplacedOthers))
	if len(unplaced) > 0 {
		o.report(unplaced)
		return fmt.Errorf("the remaining nodes cannot hold the Pods drained from %s by their CPU and memory requests; free capacity or lower requests before maintenance", target)
	}
	if len(unsure) > 0 {
		o.log("warning: the check could not settle within its search limit whether %d needed Pods fit; the availability gate after the drain decides", len(unsure))
		o.report(unsure)
	}
	if len(unplacedOthers) > 0 {
		o.log("warning: %d other Pods drained from %s do not fit after the needed ones; if the scheduler places them first, the availability gate fails", len(unplacedOthers), target)
		o.report(unplacedOthers)
	}
	return nil
}

// packBudget bounds the search for a placement, in Pods placed and taken
// back.
const packBudget = 200000

// placeNeeded places the Pods the gate needs as the scheduler spreads them.
// When spreading leaves one out, it searches for any placement of those
// some node may run. It returns a line for each Pod that does not fit, and
// in unsure those spreading left out when the search ran out of steps
// without settling whether they fit.
func placeNeeded(room []*roomyNode, needed []displacedPod, pods []pod, budget int) (unplaced, unsure []string) {
	free := make([]resources, len(room))
	for i, r := range room {
		free[i] = r.free
	}
	reset := func() {
		for i, r := range room {
			r.free, r.placed = free[i], nil
		}
	}
	noRoom, barred := place(room, needed, pods)
	if len(noRoom) == 0 && len(barred) == 0 {
		return nil, nil
	}
	// A Pod no node may run even with nothing placed stays out of the
	// search; the spreading may have barred others only by where it put
	// their neighbours.
	reset()
	var placeable, excluded []displacedPod
	for _, d := range needed {
		if slices.ContainsFunc(room, func(r *roomyNode) bool { return admits(r, d, pods) }) {
			placeable = append(placeable, d)
		} else {
			excluded = append(excluded, d)
		}
	}
	_, barredLines := place(nil, excluded, pods)
	found, settled := pack(room, placeable, pods, budget)
	if found {
		return barredLines, nil
	}
	reset()
	noRoom, barred = place(room, needed, pods)
	if !settled {
		// The spreading's misses, those barred only by where it put a
		// neighbour included, are unsettled; the excluded Pods are not.
		for _, line := range barred {
			if !slices.Contains(barredLines, line) {
				noRoom = append(noRoom, line)
			}
		}
		return barredLines, noRoom
	}
	return append(noRoom, barred...), nil
}

// pack searches depth first for a placement of every Pod, the Pods with
// the fewest nodes that may run them first, then the largest, each on the
// tightest node first, and leaves the room as placed when it finds one.
// It settles at once that they do not fit when their total CPU or memory
// exceeds the room's, and tries each arrangement of interchangeable Pods
// (same namespace, labels, requests and nodes, no anti-affinity of their
// own) once. settled is false
// when it gave up after budget steps.
func pack(room []*roomyNode, displaced []displacedPod, pods []pod, budget int) (found, settled bool) {
	var demand, total resources
	for _, d := range displaced {
		demand = demand.plus(d.requests)
	}
	for _, r := range room {
		total = total.plus(resources{max(r.free.cpu, 0), max(r.free.memory, 0)})
	}
	if !demand.fits(total) {
		return false, true
	}

	displaced = largestFirst(displaced)
	// eligible is, per Pod, which nodes may run it with nothing placed.
	eligible := map[string][]bool{}
	key := func(d displacedPod) string { return d.pod.Metadata.Namespace + "/" + d.pod.Metadata.Name }
	options := func(d displacedPod) int {
		n := 0
		for _, ok := range eligible[key(d)] {
			if ok {
				n++
			}
		}
		return n
	}
	for _, d := range displaced {
		mask := make([]bool, len(room))
		for i, r := range room {
			mask[i] = admits(r, d, pods)
		}
		eligible[key(d)] = mask
	}
	sort.SliceStable(displaced, func(i, j int) bool { return options(displaced[i]) < options(displaced[j]) })
	interchangeable := func(a, b displacedPod) bool {
		return a.pod.Metadata.Namespace == b.pod.Metadata.Namespace && maps.Equal(a.pod.Metadata.Labels, b.pod.Metadata.Labels) &&
			len(hostnameAntiAffinity(a.pod.Spec)) == 0 && len(hostnameAntiAffinity(b.pod.Spec)) == 0 && a.requests == b.requests &&
			slices.Equal(eligible[key(a)], eligible[key(b)])
	}

	// chosen is the room index each Pod was placed on; a Pod
	// interchangeable with the one before goes on the same or a later node.
	chosen := make([]int, len(displaced))
	steps := 0
	var try func(i int) bool
	try = func(i int) bool {
		if i == len(displaced) {
			return true
		}
		d := displaced[i]
		lowest := 0
		if i > 0 && interchangeable(displaced[i-1], d) {
			lowest = chosen[i-1]
		}
		var candidates []int
		for j := lowest; j < len(room); j++ {
			if d.requests.fits(room[j].free) && admits(room[j], d, pods) {
				candidates = append(candidates, j)
			}
		}
		sort.SliceStable(candidates, func(a, b int) bool {
			return room[candidates[a]].freeShare() < room[candidates[b]].freeShare()
		})
		for _, j := range candidates {
			if steps++; steps > budget {
				return false
			}
			r := room[j]
			chosen[i] = j
			r.free = r.free.minus(d.requests)
			r.placed = append(r.placed, d.pod)
			if try(i + 1) {
				return true
			}
			r.free = r.free.plus(d.requests)
			r.placed = r.placed[:len(r.placed)-1]
		}
		return false
	}
	found = try(0)
	return found, found || steps <= budget
}

// largestFirst sorts a copy of the Pods by CPU, then memory, largest first.
func largestFirst(displaced []displacedPod) []displacedPod {
	displaced = slices.Clone(displaced)
	sort.SliceStable(displaced, func(i, j int) bool {
		a, b := displaced[i].requests, displaced[j].requests
		if a.cpu != b.cpu {
			return a.cpu > b.cpu
		}
		return a.memory > b.memory
	})
	return displaced
}

// place puts the Pods, largest first, on the node that admits them and has
// room with the largest share of its CPU and memory free, as the default
// scheduler's spreading score does. It returns a line for each that some
// node admits but none has room for, and one for each no node admits.
func place(room []*roomyNode, displaced []displacedPod, pods []pod) (noRoom, barred []string) {
	for _, d := range largestFirst(displaced) {
		var pick *roomyNode
		admitted := false
		for _, r := range room {
			if !admits(r, d, pods) {
				continue
			}
			admitted = true
			if !d.requests.fits(r.free) {
				continue
			}
			if pick == nil || r.freeShare() > pick.freeShare() {
				pick = r
			}
		}
		name := d.pod.Metadata.Namespace + "/" + d.pod.Metadata.Name
		switch {
		case !admitted:
			barred = append(barred, fmt.Sprintf("%s\t%s\tno remaining node may run it", name, d.requests))
		case pick == nil:
			noRoom = append(noRoom, fmt.Sprintf("%s\t%s\tno remaining node has room", name, d.requests))
		default:
			pick.free = pick.free.minus(d.requests)
			pick.placed = append(pick.placed, d.pod)
		}
	}
	return noRoom, barred
}

// freeShare is the node's free share of CPU and memory, from 0 to 2.
func (r *roomyNode) freeShare() float64 {
	share := 0.0
	if r.allocatable.cpu > 0 {
		share += float64(r.free.cpu) / float64(r.allocatable.cpu)
	}
	if r.allocatable.memory > 0 {
		share += float64(r.free.memory) / float64(r.allocatable.memory)
	}
	return share
}

// residentRequests is what the node holds for a Pod already on it: its
// requests, with each container's raised during an in-place resize to what
// the node allocated to it or it runs with, as the scheduler counts them.
func residentRequests(p pod) (resources, error) {
	requested := map[string]resources{}
	for _, c := range slices.Concat(p.Spec.Containers, p.Spec.InitContainers) {
		r, err := readResources(c.Resources.Requests)
		if err != nil {
			return resources{}, err
		}
		requested[c.Name] = r
	}
	held := map[string]resources{}
	for _, s := range append(slices.Clone(p.Status.ContainerStatuses), p.Status.InitContainerStatuses...) {
		allocated, err := readResources(s.AllocatedResources)
		if err != nil {
			return resources{}, err
		}
		actual, err := readResources(s.Resources.Requests)
		if err != nil {
			return resources{}, err
		}
		if len(s.AllocatedResources) == 0 && len(s.Resources.Requests) == 0 {
			continue
		}
		h := resources{max(allocated.cpu, actual.cpu), max(allocated.memory, actual.memory)}
		// A resource the node does not report holds the request.
		if s.AllocatedResources["cpu"] == "" && s.Resources.Requests["cpu"] == "" {
			h.cpu = requested[s.Name].cpu
		}
		if s.AllocatedResources["memory"] == "" && s.Resources.Requests["memory"] == "" {
			h.memory = requested[s.Name].memory
		}
		held[s.Name] = h
	}
	infeasible := p.Status.Resize == "Infeasible"
	for _, c := range p.Status.Conditions {
		if c.Type == "PodResizePending" && c.Status == "True" && c.Reason == "Infeasible" {
			infeasible = true
		}
	}
	// A Pod-level resize holds its own allocation, which the overhead adds
	// to as it does to the Pod-level request; an infeasible one holds only
	// that, not the request the node refused.
	podLevelHeld := len(p.Status.AllocatedResources) > 0 || len(p.Status.Resources.Requests) > 0
	// The refused request goes only for the resources the node reports.
	spec := p.Spec
	if infeasible && podLevelHeld {
		spec.Resources.Requests = maps.Clone(spec.Resources.Requests)
		for name := range spec.Resources.Requests {
			_, allocated := p.Status.AllocatedResources[name]
			_, actual := p.Status.Resources.Requests[name]
			if allocated || actual {
				delete(spec.Resources.Requests, name)
			}
		}
	}
	requests, err := heldRequests(spec, held, infeasible)
	if err != nil {
		return requests, err
	}
	if !podLevelHeld {
		return requests, nil
	}
	allocated, err := readResources(p.Status.AllocatedResources)
	if err != nil {
		return requests, err
	}
	actual, err := readResources(p.Status.Resources.Requests)
	if err != nil {
		return requests, err
	}
	overhead, err := readResources(p.Spec.Overhead)
	if err != nil {
		return requests, err
	}
	podLevel := resources{max(allocated.cpu, actual.cpu), max(allocated.memory, actual.memory)}.plus(overhead)
	return resources{max(requests.cpu, podLevel.cpu), max(requests.memory, podLevel.memory)}, nil
}

// admits reports whether the node may run the Pod, room aside.
func admits(r *roomyNode, d displacedPod, pods []pod) bool {
	return podMatchesNode(d.pod.Spec, r.node) && nodeAdmits(d.pod.Spec, r.node) && !claimed(r, d, pods)
}

// claimed reports whether a required hostname anti-affinity, of the Pod or
// of one running on the node or placed there by the check, keeps the Pod
// off the node.
func claimed(r *roomyNode, d displacedPod, pods []pod) bool {
	for _, p := range r.placed {
		if repels(p, d.pod) || repels(d.pod, p) {
			return true
		}
	}
	for _, p := range pods {
		if livePod(p) && p.Spec.NodeName == r.node.Metadata.Name && (repels(p, d.pod) || repels(d.pod, p)) {
			return true
		}
	}
	return false
}

// repels reports whether a's required hostname anti-affinity selects b.
// Terms with a namespace selector or label keys, which the check does not
// model, are left out.
func repels(a, b pod) bool {
	for _, term := range hostnameAntiAffinity(a.Spec) {
		namespaces := term.Namespaces
		if len(namespaces) == 0 {
			namespaces = []string{a.Metadata.Namespace}
		}
		if slices.Contains(namespaces, b.Metadata.Namespace) && selectorMatches(b.Metadata.Labels, term.LabelSelector) {
			return true
		}
	}
	return false
}

// hostnameAntiAffinity lists the Pod's required hostname anti-affinity
// terms the check models.
func hostnameAntiAffinity(spec podSpec) []podAffinityTerm {
	if spec.Affinity == nil || spec.Affinity.PodAntiAffinity == nil {
		return nil
	}
	var terms []podAffinityTerm
	for _, term := range spec.Affinity.PodAntiAffinity.Required {
		if term.TopologyKey == hostnameLabel && term.LabelSelector != nil && term.NamespaceSelector == nil &&
			len(term.MatchLabelKeys) == 0 && len(term.MismatchLabelKeys) == 0 {
			terms = append(terms, term)
		}
	}
	return terms
}

// mayRunElsewhere reports whether a Ready, schedulable node other than the
// target may run the Pod, room aside.
func mayRunElsewhere(target string, nodes []node, pods []pod, p pod) bool {
	for _, n := range nodes {
		if n.Metadata.Name == target || n.Spec.Unschedulable || !nodeIsReady(n) {
			continue
		}
		if admits(&roomyNode{node: n}, displacedPod{pod: p}, pods) {
			return true
		}
	}
	return false
}

// replacementRequests estimates the requests of the Pod that replaces p
// from the union of p, which carries what admission added (LimitRange
// defaults, injected sidecars, RuntimeClass overhead), and the template
// that recreates it, which an in-place resize of p does not change: every
// container at the larger of its two requests (a template's limit standing
// in for a missing request, as on admission), each init container beside
// the sidecars that start before it on either side, and the larger
// Pod-level request. It is exact when p matches the
// template and otherwise may count more than the replacement asks, never
// less.
func (o *Operation) replacementRequests(p pod) (resources, error) {
	template, found, err := o.recreationTemplate(p)
	if err != nil || !found {
		requests, specErr := podRequests(p.Spec)
		if err == nil {
			err = specErr
		}
		return requests, err
	}
	// Pod-level defaulting, a Pod-level limit standing in for a request no
	// container makes, comes before LimitRange admission.
	written := template
	template, err = o.withLimitRangeDefaults(p.Metadata.Namespace, template)
	if err != nil {
		return resources{}, err
	}
	// largest is each container's larger request, by name.
	largest := map[string]resources{}
	for _, c := range slices.Concat(p.Spec.Containers, p.Spec.InitContainers, template.Containers, template.InitContainers) {
		r, err := containerRequest(c)
		if err != nil {
			return resources{}, err
		}
		l := largest[c.Name]
		largest[c.Name] = resources{max(l.cpu, r.cpu), max(l.memory, r.memory)}
	}
	sum := func(names map[string]bool) resources {
		var total resources
		for name := range names {
			total = total.plus(largest[name])
		}
		return total
	}
	names := func(list []container, keep func(container) bool) map[string]bool {
		set := map[string]bool{}
		for _, c := range list {
			if keep(c) {
				set[c.Name] = true
			}
		}
		return set
	}
	isSidecar := func(c container) bool { return c.RestartPolicy == "Always" }
	app := names(slices.Concat(p.Spec.Containers, template.Containers), func(container) bool { return true })
	podSidecars := names(p.Spec.InitContainers, isSidecar)
	templateSidecars := names(template.InitContainers, isSidecar)
	sidecars := maps.Clone(podSidecars)
	maps.Copy(sidecars, templateSidecars)
	total := sum(app).plus(sum(sidecars))
	for _, side := range [][]container{p.Spec.InitContainers, template.InitContainers} {
		for _, c := range side {
			if isSidecar(c) {
				continue
			}
			// Beside this init container run the sidecars before it on
			// either side that has it: the template's order, and where
			// admission put the Pod's own. A side without it says nothing
			// of its place, so all that side's sidecars count.
			beside := map[string]bool{}
			for _, list := range [][]container{p.Spec.InitContainers, template.InitContainers} {
				at := slices.IndexFunc(list, func(o container) bool { return o.Name == c.Name })
				if at < 0 {
					at = len(list)
				}
				for _, before := range list[:at] {
					if isSidecar(before) {
						beside[before.Name] = true
					}
				}
			}
			peak := largest[c.Name].plus(sum(beside))
			total = resources{max(total.cpu, peak.cpu), max(total.memory, peak.memory)}
		}
	}
	for _, spec := range []podSpec{p.Spec, written} {
		podLevel, err := podLevelRequest(spec)
		if err != nil {
			return resources{}, err
		}
		total = resources{max(total.cpu, podLevel.cpu), max(total.memory, podLevel.memory)}
	}
	overhead, err := readResources(p.Spec.Overhead)
	if err != nil {
		return resources{}, err
	}
	return total.plus(overhead), nil
}

// limitRange is the part of a LimitRange that defaults containers.
type limitRange struct {
	Spec struct {
		Limits []struct {
			Type           string            `json:"type"`
			Default        map[string]string `json:"default"`
			DefaultRequest map[string]string `json:"defaultRequest"`
		} `json:"limits"`
	} `json:"spec"`
}

// withLimitRangeDefaults gives a template's containers the namespace's
// LimitRange defaults, as admission gives the Pod made from it: a missing
// request first takes the container's own limit, then the largest default
// request, and a missing limit the largest default limit, which a still
// missing request then takes. Kubernetes does not say which of several
// LimitRanges wins, so the largest is the bound. When a container relies
// on a default and the LimitRanges cannot be read, the size is unknown and
// the check fails.
func (o *Operation) withLimitRangeDefaults(namespace string, template podSpec) (podSpec, error) {
	reliesOnDefault := slices.ContainsFunc(slices.Concat(template.Containers, template.InitContainers), func(c container) bool {
		for _, name := range []string{"cpu", "memory"} {
			if c.Resources.Requests[name] == "" && c.Resources.Limits[name] == "" {
				return true
			}
		}
		return false
	})
	if !reliesOnDefault {
		return template, nil
	}
	ranges, ok := o.limitRanges[namespace]
	if !ok {
		var list struct {
			Items []limitRange `json:"items"`
		}
		if err := o.getJSON(&list, "-n", namespace, "get", "limitranges", "-o", "json"); err != nil {
			return template, fmt.Errorf("cannot read the LimitRanges that size a replacement in %s: %v", namespace, err)
		}
		ranges = list.Items
		if o.limitRanges == nil {
			o.limitRanges = map[string][]limitRange{}
		}
		o.limitRanges[namespace] = ranges
	}
	largest := func(name string, pick func(defaultRequest, defaultLimit map[string]string) map[string]string) (string, error) {
		best, value := int64(-1), ""
		for _, r := range ranges {
			for _, l := range r.Spec.Limits {
				text, ok := pick(l.DefaultRequest, l.Default)[name]
				if l.Type != "Container" || !ok {
					continue
				}
				scale := 1.0
				if name == "cpu" {
					scale = 1000
				}
				n, err := parseQuantity(text, scale)
				if err != nil {
					return "", err
				}
				if n > best {
					best, value = n, text
				}
			}
		}
		return value, nil
	}
	apply := func(list []container) ([]container, error) {
		list = slices.Clone(list)
		for i := range list {
			c := &list[i]
			c.Resources.Requests = maps.Clone(c.Resources.Requests)
			c.Resources.Limits = maps.Clone(c.Resources.Limits)
			if c.Resources.Requests == nil {
				c.Resources.Requests = map[string]string{}
			}
			if c.Resources.Limits == nil {
				c.Resources.Limits = map[string]string{}
			}
			for _, name := range []string{"cpu", "memory"} {
				if c.Resources.Requests[name] == "" && c.Resources.Limits[name] != "" {
					c.Resources.Requests[name] = c.Resources.Limits[name]
				}
				if c.Resources.Requests[name] == "" {
					value, err := largest(name, func(request, _ map[string]string) map[string]string { return request })
					if err != nil {
						return nil, err
					}
					if value != "" {
						c.Resources.Requests[name] = value
					}
				}
				if c.Resources.Limits[name] == "" {
					value, err := largest(name, func(_, limit map[string]string) map[string]string { return limit })
					if err != nil {
						return nil, err
					}
					if value != "" {
						c.Resources.Limits[name] = value
					}
				}
			}
		}
		return list, nil
	}
	var err error
	if template.Containers, err = apply(template.Containers); err != nil {
		return template, err
	}
	if template.InitContainers, err = apply(template.InitContainers); err != nil {
		return template, err
	}
	return template, nil
}

// podLevelRequest is a Pod spec's Pod-level request, with its Pod-level
// limit standing in for a resource that neither it nor any container
// requests, as admission defaults it.
func podLevelRequest(spec podSpec) (resources, error) {
	requests := maps.Clone(spec.Resources.Requests)
	if requests == nil {
		requests = map[string]string{}
	}
	for _, name := range []string{"cpu", "memory"} {
		if _, ok := requests[name]; ok {
			continue
		}
		limit, ok := spec.Resources.Limits[name]
		if !ok {
			continue
		}
		requested := slices.ContainsFunc(slices.Concat(spec.Containers, spec.InitContainers), func(c container) bool {
			_, request := c.Resources.Requests[name]
			_, limit := c.Resources.Limits[name]
			return request || limit
		})
		if !requested {
			requests[name] = limit
		}
	}
	return readResources(requests)
}

// containerRequest is a container's request, with its limit standing in
// for a missing one as admission defaults it.
func containerRequest(c container) (resources, error) {
	requests := maps.Clone(c.Resources.Requests)
	if requests == nil {
		requests = map[string]string{}
	}
	for _, name := range []string{"cpu", "memory"} {
		if _, ok := requests[name]; !ok {
			if limit, ok := c.Resources.Limits[name]; ok {
				requests[name] = limit
			}
		}
	}
	return readResources(requests)
}

// statefulSet is the part of a StatefulSet that decides which revision
// recreates a Pod.
type statefulSet struct {
	Spec struct {
		Template struct {
			Spec podSpec `json:"spec"`
		} `json:"template"`
		// PodManagementPolicy Parallel creates Pods in any order; the
		// default, OrderedReady, lowest ordinal first.
		PodManagementPolicy string `json:"podManagementPolicy"`
		// Ordinals.Start is the first ordinal, zero when unset.
		Ordinals *struct {
			Start int `json:"start"`
		} `json:"ordinals"`
		UpdateStrategy struct {
			Type          string `json:"type"`
			RollingUpdate *struct {
				Partition *int `json:"partition"`
			} `json:"rollingUpdate"`
		} `json:"updateStrategy"`
	} `json:"spec"`
	Status struct {
		CurrentRevision string `json:"currentRevision"`
		UpdateRevision  string `json:"updateRevision"`
	} `json:"status"`
}

// podOrdinal is a StatefulSet Pod's ordinal, the number after its name's
// last dash, or -1.
func podOrdinal(p pod) int {
	ordinal, err := strconv.Atoi(p.Metadata.Name[strings.LastIndex(p.Metadata.Name, "-")+1:])
	if err != nil {
		return -1
	}
	return ordinal
}

// controllerRevision holds a StatefulSet revision's Pod template.
type controllerRevision struct {
	Data struct {
		Spec struct {
			Template struct {
				Spec podSpec `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	} `json:"data"`
}

// recreationTemplate returns the Pod template p's controller recreates it
// from: its own ReplicaSet's, which in a paused or unfinished Deployment
// rollout differs from the Deployment's, or for a StatefulSet the revision
// its ordinal gets, the current one below a rolling update's partition.
// found is false when no such controller recreates the Pod; one that does
// but cannot be read leaves the size unknown and fails.
func (o *Operation) recreationTemplate(p pod) (podSpec, bool, error) {
	namespace := p.Metadata.Namespace
	owner := p.Metadata.firstOwner()
	switch owner.Kind {
	case "ReplicaSet":
		var rs controller
		if err := o.getJSON(&rs, "-n", namespace, "get", "replicaset", owner.Name, "-o", "json"); err != nil {
			return podSpec{}, false, fmt.Errorf("cannot read the ReplicaSet that recreates it: %v", err)
		}
		return rs.Spec.Template.Spec, true, nil
	case "StatefulSet":
		var sts statefulSet
		if err := o.getJSON(&sts, "-n", namespace, "get", "statefulset", owner.Name, "-o", "json"); err != nil {
			return podSpec{}, false, fmt.Errorf("cannot read the StatefulSet that recreates it: %v", err)
		}
		// As the controller does, a Pod whose ordinal is below the first
		// ordinal plus the partition comes back at the current revision.
		revision := sts.Status.UpdateRevision
		ordinal := podOrdinal(p)
		start := 0
		if sts.Spec.Ordinals != nil {
			start = sts.Spec.Ordinals.Start
		}
		if strategy := sts.Spec.UpdateStrategy; strategy.Type != "OnDelete" && strategy.RollingUpdate != nil &&
			strategy.RollingUpdate.Partition != nil && ordinal >= 0 && ordinal < start+*strategy.RollingUpdate.Partition {
			revision = sts.Status.CurrentRevision
		}
		if revision == "" || revision == sts.Status.UpdateRevision {
			return sts.Spec.Template.Spec, true, nil
		}
		var cr controllerRevision
		if err := o.getJSON(&cr, "-n", namespace, "get", "controllerrevision", revision, "-o", "json"); err != nil {
			return podSpec{}, false, fmt.Errorf("cannot read the revision %s that recreates it: %v", revision, err)
		}
		return cr.Data.Spec.Template.Spec, true, nil
	}
	return podSpec{}, false, nil
}

// displacedPodsToPlace lists the target's Pods that the availability gate
// requires elsewhere: of each displaced controller the gate checks, as many
// as its Ready replicas on other nodes fall short of its minimum. others are
// the rest of the Pods a controller recreates, which the gate does not
// need. Pods of controllers that can run only on the target wait for it and
// are neither.
func (o *Operation) displacedPodsToPlace(nodes []node, pods []pod) (needed, others []displacedPod, err error) {
	target := o.opts.Node
	byWorkload := map[workload][]pod{}
	var order []workload
	for _, p := range pods {
		if p.Spec.NodeName != target || !p.active() || p.Metadata.hasOwner("DaemonSet") ||
			isMirror(p) ||
			p.Metadata.Labels["longhorn.io/component"] == "instance-manager" {
			continue
		}
		w, ok, err := o.workloadForPod(p)
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			continue
		}
		if _, seen := byWorkload[w]; !seen {
			order = append(order, w)
		}
		byWorkload[w] = append(byWorkload[w], p)
	}

	for _, w := range uniqueWorkloads(order) {
		minimum, _, gated, err := o.readyMinimum(w)
		if err != nil {
			return nil, nil, err
		}
		var c controller
		found := false
		if gated {
			if found, err = o.readQuiet(&c, "-n", w.Namespace, "get", w.ref(), "-o", "json"); err != nil {
				return nil, nil, err
			}
			// Pods that can run only on the target, or that tolerate its
			// cordon and may land back on it, wait for it.
			if found && (requiresTargetNode(target, c.Spec.Template.Spec, nodes) || toleratesCordon(c.Spec.Template.Spec)) {
				continue
			}
		}
		onTarget := byWorkload[w]
		need := 0
		if gated {
			readyOnTarget := 0
			for _, p := range onTarget {
				if p.podReady() {
					readyOnTarget++
				}
			}
			need = minimum - (c.Status.ReadyReplicas - readyOnTarget)
			need = min(max(need, 0), len(onTarget))
		}
		var displaced []displacedPod
		for _, p := range onTarget {
			requests, err := o.replacementRequests(p)
			if err != nil {
				return nil, nil, fmt.Errorf("%s/%s: %v", p.Metadata.Namespace, p.Metadata.Name, err)
			}
			displaced = append(displaced, displacedPod{pod: p, requests: requests})
		}
		// Any of the controller's replicas meets its gate, so the needed
		// ones are those some remaining node may run, smallest first; an
		// ordered StatefulSet recreates its lowest ordinals first, so those
		// are.
		displaced = largestFirst(displaced)
		slices.Reverse(displaced)
		sort.SliceStable(displaced, func(i, j int) bool {
			return mayRunElsewhere(target, nodes, pods, displaced[i].pod) && !mayRunElsewhere(target, nodes, pods, displaced[j].pod)
		})
		if w.Kind == "statefulset" {
			var sts statefulSet
			found, err := o.readQuiet(&sts, "-n", w.Namespace, "get", w.ref(), "-o", "json")
			if err != nil {
				return nil, nil, err
			}
			if !found || sts.Spec.PodManagementPolicy != "Parallel" {
				sort.SliceStable(displaced, func(i, j int) bool {
					return podOrdinal(displaced[i].pod) < podOrdinal(displaced[j].pod)
				})
			}
		}
		needed = append(needed, displaced[:need]...)
		others = append(others, displaced[need:]...)
	}
	return needed, others, nil
}
