package k8snode

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// maxSwitchoverLag is how far, in bytes of WAL, a replica may trail the
// primary's flushed WAL and still take over: one WAL segment.
const maxSwitchoverLag = 16 << 20

// replicationQueryTimeout bounds one question to a primary.
const replicationQueryTimeout = 30 * time.Second

// replicationQuery lists the primary's standbys: name, state and how many
// bytes of WAL each has still to flush. A replica cluster's designated
// primary is itself in recovery, where pg_current_wal_lsn fails; like its
// cascading sender, it measures from the later of the WAL it received and
// the WAL it replayed, since after a fall back to the archive the received
// position stops moving.
const replicationQuery = "SELECT application_name, state, COALESCE(pg_wal_lsn_diff(" +
	"CASE WHEN pg_is_in_recovery() THEN GREATEST(pg_last_wal_receive_lsn(), pg_last_wal_replay_lsn()) " +
	"ELSE pg_current_wal_lsn() END, flush_lsn), -1) FROM pg_stat_replication"

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
	// lagging is set when a replica that streams would take over but
	// trails too far behind; waiting may bring it within reach.
	lagging bool
}

// standby is a replica as its primary sees it in pg_stat_replication.
type standby struct {
	state string
	// lag is the WAL, in bytes, the replica has still to flush; -1 when the
	// primary does not know.
	lag float64
}

// caughtUp reports whether the replica streams and trails the primary by at
// most maxSwitchoverLag.
func (s standby) caughtUp() bool {
	return s.state == "streaming" && s.lag >= 0 && s.lag <= maxSwitchoverLag
}

// parseStandbys reads replicationQuery's unaligned output into standbys by
// namespace/name.
func parseStandbys(namespace, output string) map[string]standby {
	standbys := map[string]standby{}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Split(strings.TrimSpace(line), "|")
		if len(fields) != 3 || fields[0] == "" {
			continue
		}
		lag, err := strconv.ParseFloat(fields[2], 64)
		if err != nil {
			lag = -1
		}
		standbys[namespace+"/"+fields[0]] = standby{state: fields[1], lag: lag}
	}
	return standbys
}

// replication is what the clusters' primaries report: standbys by
// namespace/name, and why a cluster's primary could not be asked, by
// namespace/cluster.
type replication struct {
	standbys   map[string]standby
	unanswered map[string]string
}

// streaming describes a cluster's streaming replicas for a report: the
// count, or why it is unknown.
func (r replication) streaming(c cnpgCluster, count int) string {
	if reason, ok := r.unanswered[c.Metadata.Namespace+"/"+c.Metadata.Name]; ok {
		return "unknown (" + reason + ")"
	}
	return strconv.Itoa(count)
}

// cnpgStandbys asks each cluster's Ready primary which replicas stream from
// it. CloudNativePG reports a replica healthy once PostgreSQL accepts
// connections, even when it can no longer stream because the primary has
// removed the WAL it needs; only the primary knows. A primary that cannot be
// asked reports no standbys, and the reason is kept so a report does not
// mistake it for replicas that stopped streaming.
func (o *Operation) cnpgStandbys(clusters []cnpgCluster, pods []pod) replication {
	index := podIndex(pods)
	r := replication{standbys: map[string]standby{}, unanswered: map[string]string{}}
	for _, c := range clusters {
		if c.desiredInstances() < 2 {
			continue
		}
		key := c.Metadata.Namespace + "/" + c.Metadata.Name
		primary, found := index[c.Metadata.Namespace+"/"+c.Status.CurrentPrimary]
		if !found || !primary.runningAndReady() {
			r.unanswered[key] = "primary " + c.Status.CurrentPrimary + " is not Running and Ready"
			continue
		}
		result := o.kubectl(Command{Argv: []string{"-n", c.Metadata.Namespace, "exec", c.Status.CurrentPrimary, "-c", "postgres",
			"--", "psql", "-XAt", "-c", replicationQuery}, Quiet: true, Timeout: replicationQueryTimeout})
		if result.Status != 0 {
			reason, _, _ := strings.Cut(strings.TrimSpace(result.Stderr), "\n")
			if reason == "" {
				reason = fmt.Sprintf("status %d", result.Status)
			}
			r.unanswered[key] = "could not ask " + c.Status.CurrentPrimary + ": " + reason
			continue
		}
		for name, s := range parseStandbys(c.Metadata.Namespace, result.Stdout) {
			r.standbys[name] = s
		}
	}
	return r
}

// streamingReplicas counts the cluster's replicas that stream from its
// primary, leaving out those on the excluded node.
func streamingReplicas(c cnpgCluster, pods []pod, standbys map[string]standby, excluded string) int {
	count := 0
	for _, p := range pods {
		if p.Metadata.Namespace != c.Metadata.Namespace ||
			p.Metadata.Labels["cnpg.io/cluster"] != c.Metadata.Name ||
			p.Metadata.Name == c.Status.CurrentPrimary ||
			(excluded != "" && p.Spec.NodeName == excluded) {
			continue
		}
		if standbys[p.Metadata.Namespace+"/"+p.Metadata.Name].state == "streaming" {
			count++
		}
	}
	return count
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
// primary away from the node and enough Ready instances, with the replicas
// among them streaming, for one node down. A cluster of two instances keeps
// no replica then, so only three or more keep a standby. A cluster whose
// primary could not be asked fails, whatever its size.
func cnpgSurvivabilityFailures(target string, clusters []cnpgCluster, pods []pod, repl replication) []string {
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
		streaming := streamingReplicas(c, pods, repl.standbys, target)
		_, unanswered := repl.unanswered[c.Metadata.Namespace+"/"+c.Metadata.Name]
		if ready < minimumReady || primary == "" || !found || unanswered ||
			primaryPod.Spec.NodeName == target || !primaryPod.runningAndReady() ||
			streaming < minimumReady-1 {
			primaryNode := primaryPod.Spec.NodeName
			if primaryNode == "" {
				primaryNode = "missing"
			}
			lines = append(lines, fmt.Sprintf("%s/%s\tready=%d/%d\tminimum=%d\tstreaming=%s\tprimary=%s\tprimaryNode=%s",
				c.Metadata.Namespace, c.Metadata.Name, ready, desired, minimumReady, repl.streaming(c, streaming), primary, primaryNode))
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
		bad := cnpgSurvivabilityFailures(o.opts.Node, clusters, pods, o.cnpgStandbys(clusters, pods))
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
		pods, err := o.listAllPods()
		if err != nil {
			return err
		}
		repl := o.cnpgStandbys(clusters, pods)
		var bad []string
		// notStreaming is set when a primary answered and too few
		// replicas stream, the case a rebuild may be needed for.
		notStreaming := false
		for _, c := range clusters {
			desired := c.desiredInstances()
			streaming := streamingReplicas(c, pods, repl.standbys, "")
			_, unanswered := repl.unanswered[c.Metadata.Namespace+"/"+c.Metadata.Name]
			short := streaming < desired-1
			if c.Status.ReadyInstances != desired || c.Status.Phase != "Cluster in healthy state" || short {
				phase := c.Status.Phase
				if phase == "" {
					phase = "unknown"
				}
				if short && !unanswered && c.Status.ReadyInstances == desired {
					notStreaming = true
				}
				bad = append(bad, fmt.Sprintf("%s/%s\tready=%d/%d\tstreaming=%s/%d\tphase=%s",
					c.Metadata.Namespace, c.Metadata.Name, c.Status.ReadyInstances, desired, repl.streaming(c, streaming), max(desired-1, 0), phase))
			}
		}
		if len(bad) == 0 {
			return nil
		}
		o.report(bad)
		if o.expired(deadline) {
			if notStreaming {
				return fmt.Errorf("CloudNativePG clusters did not return to full health; a Ready replica that does not stream may need " +
					"WAL its primary has removed, and only a rebuild (kubectl cnpg destroy) brings it back")
			}
			return fmt.Errorf("CloudNativePG clusters did not return to full health")
		}
		if err := o.pause(); err != nil {
			return err
		}
	}
}

// planSwitchovers finds every cluster whose primary runs on the node and
// the healthy replica, on a surviving Ready, schedulable stable node, that
// would take over. A candidate must stream from the primary and be caught up:
// promoting a replica that stopped streaming rolls the database back to where
// it stopped. Candidates on nodes without RebootPendingTaint come first, so a
// fleet reboot moves a primary to a node that already rebooted; otherwise
// they are taken in name order.
func planSwitchovers(target string, clusters []cnpgCluster, pods []pod, nodes []node, standbys map[string]standby) []switchover {
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
		lagging := false
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
			if standby := standbys[p.Metadata.Namespace+"/"+p.Metadata.Name]; !standby.caughtUp() {
				lagging = lagging || standby.state == "streaming"
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
		s := switchover{namespace: c.Metadata.Namespace, cluster: c.Metadata.Name, primary: primary, lagging: lagging}
		if len(candidates) > 0 {
			s.candidate = candidates[0].Metadata.Name
			s.candidateNode = candidates[0].Spec.NodeName
		}
		plan = append(plan, s)
	}
	return plan
}

// planCNPGSwitchovers prints the planned primary switchovers and fails when
// a primary on the node has no healthy replica to take over. A replica may
// trail briefly under load, so it waits up to the settle timeout for one to
// catch up.
func (o *Operation) planCNPGSwitchovers() ([]switchover, error) {
	if !o.cnpgInstalled() {
		return nil, nil
	}
	deadline := o.now().Add(duration(o.opts.SettleTimeout))
	for {
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
		repl := o.cnpgStandbys(clusters, pods)
		plan := planSwitchovers(o.opts.Node, clusters, pods, nodes, repl.standbys)
		if len(plan) == 0 {
			return nil, nil
		}
		var missing []string
		lagging := true
		for _, s := range plan {
			if s.candidate == "" || s.candidateNode == "" {
				line := s.namespace + "/" + s.cluster
				if reason, ok := repl.unanswered[line]; ok {
					line += ": " + reason
				}
				missing = append(missing, line)
				lagging = lagging && s.lagging
			}
		}
		// Only a replica that streams but trails can come within reach;
		// unless every missing cluster has one, the run stops at once.
		if len(missing) > 0 && lagging && !o.expired(deadline) {
			o.log("waiting for a streaming, caught-up replica of %s", strings.Join(missing, ", "))
			if err := o.pause(); err != nil {
				return nil, err
			}
			continue
		}

		fmt.Fprintf(o.stdout, "namespace\tcluster\tcurrent-primary\ttarget-primary\ttarget-node\n")
		for _, s := range plan {
			candidate, candidateNode := s.candidate, s.candidateNode
			if candidate == "" || candidateNode == "" {
				candidate, candidateNode = "unavailable", "unavailable"
			}
			fmt.Fprintf(o.stdout, "%s\t%s\t%s\t%s\t%s\n", s.namespace, s.cluster, s.primary, candidate, candidateNode)
		}
		if len(missing) > 0 {
			return nil, fmt.Errorf("CloudNativePG primary on %s has no healthy, streaming and caught-up replica on a surviving stable node (%s)",
				o.opts.Node, strings.Join(missing, "; "))
		}
		return plan, nil
	}
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
