package k8snode

import (
	"fmt"
	"slices"
	"sort"
)

type cnpgCluster struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		Instances *int `json:"instances"`
	} `json:"spec"`
	Status struct {
		ReadyInstances  int                 `json:"readyInstances"`
		CurrentPrimary  string              `json:"currentPrimary"`
		Phase           string              `json:"phase"`
		InstancesStatus map[string][]string `json:"instancesStatus"`
	} `json:"status"`
}

// desiredInstances is spec.instances, which defaults to one.
func (c cnpgCluster) desiredInstances() int {
	if c.Spec.Instances == nil {
		return 1
	}
	return *c.Spec.Instances
}

// switchover moves a CloudNativePG primary off the node. An empty candidate
// means no healthy replica can take over.
type switchover struct {
	namespace     string
	cluster       string
	primary       string
	candidate     string
	candidateNode string
}

// cnpgInstalled reports whether the cluster serves CloudNativePG clusters.
func (o *Operation) cnpgInstalled() bool {
	r := o.kubectl(Command{Argv: []string{"get", "clusters.postgresql.cnpg.io", "--all-namespaces"}, Quiet: true})
	return r.Status == 0
}

func (o *Operation) listCNPGClusters() ([]cnpgCluster, error) {
	var clusters struct {
		Items []cnpgCluster `json:"items"`
	}
	err := o.getJSON(&clusters, "get", "clusters.postgresql.cnpg.io", "--all-namespaces", "-o", "json")
	return clusters.Items, err
}

// podIndex maps namespace/name to each Pod.
func podIndex(pods []pod) map[string]pod {
	index := map[string]pod{}
	for _, p := range pods {
		index[p.Metadata.Namespace+"/"+p.Metadata.Name] = p
	}
	return index
}

// cnpgSurvivabilityFailures lists clusters that would not keep a Ready
// primary away from the node and enough Ready instances for one node down.
func cnpgSurvivabilityFailures(target string, clusters []cnpgCluster, pods []pod) []string {
	index := podIndex(pods)
	var lines []string
	for _, c := range clusters {
		desired := c.desiredInstances()
		minimumReady := 1
		if desired > 1 {
			minimumReady = desired - 1
		}
		ready := c.Status.ReadyInstances
		primary := c.Status.CurrentPrimary
		primaryPod, found := index[c.Metadata.Namespace+"/"+primary]
		if ready < minimumReady || primary == "" || !found ||
			primaryPod.Spec.NodeName == target || !primaryPod.runningAndReady() {
			primaryNode := primaryPod.Spec.NodeName
			if primaryNode == "" {
				primaryNode = "missing"
			}
			lines = append(lines, fmt.Sprintf("%s/%s\tready=%d/%d\tminimum=%d\tprimary=%s\tprimaryNode=%s",
				c.Metadata.Namespace, c.Metadata.Name, ready, desired, minimumReady, primary, primaryNode))
		}
	}
	return lines
}

func (o *Operation) waitForCNPGSurvivability() error {
	if !o.cnpgInstalled() {
		o.log("CloudNativePG clusters not found; skipping database maintenance gate")
		return nil
	}
	o.log("checking CloudNativePG one-node-down availability")
	deadline := o.now().Add(duration(o.opts.SettleTimeout))
	for {
		clusters, err := o.listCNPGClusters()
		if err != nil {
			return err
		}
		pods, err := o.listAllPods()
		if err != nil {
			return err
		}
		bad := cnpgSurvivabilityFailures(o.opts.Node, clusters, pods)
		if len(bad) == 0 {
			return nil
		}
		o.report(bad)
		if o.expired(deadline) {
			return fmt.Errorf("CloudNativePG maintenance availability gate did not pass")
		}
		if err := o.pause(); err != nil {
			return err
		}
	}
}

func (o *Operation) waitForCNPGHealth() error {
	if !o.cnpgInstalled() {
		return nil
	}
	o.log("waiting for CloudNativePG clusters to become fully healthy")
	deadline := o.now().Add(duration(o.opts.SettleTimeout))
	for {
		clusters, err := o.listCNPGClusters()
		if err != nil {
			return err
		}
		var bad []string
		for _, c := range clusters {
			desired := c.desiredInstances()
			if c.Status.ReadyInstances != desired || c.Status.Phase != "Cluster in healthy state" {
				phase := c.Status.Phase
				if phase == "" {
					phase = "unknown"
				}
				bad = append(bad, fmt.Sprintf("%s/%s\tready=%d/%d\tphase=%s",
					c.Metadata.Namespace, c.Metadata.Name, c.Status.ReadyInstances, desired, phase))
			}
		}
		if len(bad) == 0 {
			return nil
		}
		o.report(bad)
		if o.expired(deadline) {
			return fmt.Errorf("CloudNativePG clusters did not return to full health")
		}
		if err := o.pause(); err != nil {
			return err
		}
	}
}

// planSwitchovers finds every cluster whose primary runs on the node and
// the healthy replica, on a surviving Ready, schedulable stable node, that
// would take over. Candidates on nodes without RebootPendingTaint come
// first, so a fleet reboot moves a primary to a node that already rebooted;
// otherwise they are taken in name order.
func planSwitchovers(target string, clusters []cnpgCluster, pods []pod, nodes []node) []switchover {
	index := podIndex(pods)
	nodeIndex := map[string]node{}
	for _, n := range nodes {
		nodeIndex[n.Metadata.Name] = n
	}
	var plan []switchover
	for _, c := range clusters {
		primary := c.Status.CurrentPrimary
		primaryPod := index[c.Metadata.Namespace+"/"+primary]
		if primaryPod.Spec.NodeName != target {
			continue
		}
		healthy := c.Status.InstancesStatus["healthy"]
		var candidates []pod
		for _, p := range pods {
			labels := p.Metadata.Labels
			if p.Metadata.Namespace != c.Metadata.Namespace ||
				labels["cnpg.io/cluster"] != c.Metadata.Name ||
				labels["cnpg.io/podRole"] != "instance" ||
				labels["cnpg.io/instanceRole"] != "replica" ||
				p.Metadata.Name == primary ||
				p.Spec.NodeName == target ||
				p.Metadata.DeletionTimestamp != nil ||
				!p.runningAndReady() ||
				!slices.Contains(healthy, p.Metadata.Name) {
				continue
			}
			n, ok := nodeIndex[p.Spec.NodeName]
			if !ok || n.Spec.Unschedulable || n.Metadata.Labels["workload-class"] != "stable" || !nodeIsReady(n) {
				continue
			}
			candidates = append(candidates, p)
		}
		sort.Slice(candidates, func(i, j int) bool {
			pendingI := rebootPending(nodeIndex[candidates[i].Spec.NodeName])
			pendingJ := rebootPending(nodeIndex[candidates[j].Spec.NodeName])
			if pendingI != pendingJ {
				return pendingJ
			}
			return candidates[i].Metadata.Name < candidates[j].Metadata.Name
		})
		s := switchover{namespace: c.Metadata.Namespace, cluster: c.Metadata.Name, primary: primary}
		if len(candidates) > 0 {
			s.candidate = candidates[0].Metadata.Name
			s.candidateNode = candidates[0].Spec.NodeName
		}
		plan = append(plan, s)
	}
	return plan
}

// planCNPGSwitchovers prints the planned primary switchovers and fails when
// a primary on the node has no healthy replica to take over.
func (o *Operation) planCNPGSwitchovers() ([]switchover, error) {
	if !o.cnpgInstalled() {
		return nil, nil
	}
	clusters, err := o.listCNPGClusters()
	if err != nil {
		return nil, err
	}
	pods, err := o.listAllPods()
	if err != nil {
		return nil, err
	}
	nodes, err := o.listNodes()
	if err != nil {
		return nil, err
	}
	plan := planSwitchovers(o.opts.Node, clusters, pods, nodes)
	if len(plan) == 0 {
		return nil, nil
	}

	fmt.Fprintf(o.stdout, "namespace\tcluster\tcurrent-primary\ttarget-primary\ttarget-node\n")
	missing := false
	for _, s := range plan {
		candidate, candidateNode := s.candidate, s.candidateNode
		if candidate == "" || candidateNode == "" {
			missing = true
			candidate, candidateNode = "unavailable", "unavailable"
		}
		fmt.Fprintf(o.stdout, "%s\t%s\t%s\t%s\t%s\n", s.namespace, s.cluster, s.primary, candidate, candidateNode)
	}
	if missing {
		return nil, fmt.Errorf("CloudNativePG primary on %s has no healthy replica on a surviving stable node", o.opts.Node)
	}
	return plan, nil
}

// switchCNPGPrimariesOffNode promotes the planned replicas through the
// operator's supported interface and waits for full health.
func (o *Operation) switchCNPGPrimariesOffNode() error {
	node := o.opts.Node
	o.log("planning CloudNativePG primary switchovers away from %s", node)
	plan, err := o.planCNPGSwitchovers()
	if err != nil {
		return err
	}
	if len(plan) == 0 {
		o.log("no CloudNativePG primaries need to move")
		return nil
	}
	if !o.runner.Has("kubectl-cnpg") {
		return fmt.Errorf("missing required command: kubectl-cnpg")
	}
	for _, s := range plan {
		o.log("switching %s/%s primary from %s to %s on %s", s.namespace, s.cluster, s.primary, s.candidate, s.candidateNode)
		r := o.kubectl(Command{Argv: []string{"cnpg", "promote", "-n", s.namespace, s.cluster, s.candidate}, Passthrough: true})
		if r.Status != 0 {
			return fmt.Errorf("could not promote %s on %s/%s", s.candidate, s.namespace, s.cluster)
		}
	}
	if err := o.waitForCNPGHealth(); err != nil {
		return err
	}
	return o.waitForCNPGSurvivability()
}
