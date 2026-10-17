package k8snode

import (
	"regexp"
	"strings"
	"testing"
)

func TestParseQuantity(t *testing.T) {
	cases := []struct {
		text  string
		scale float64
		want  int64
	}{
		{"100m", 1000, 100},
		{"1", 1000, 1000},
		{"0.5", 1000, 500},
		{"2.5", 1000, 2500},
		{"1500u", 1000, 2},
		{"250n", 1000, 1},
		{"512Mi", 1, 512 << 20},
		{"1.5Gi", 1, 3 << 29},
		{"32731792Ki", 1, 32731792 << 10},
		{"1k", 1, 1000},
		{"128M", 1, 128e6},
		{"1e3", 1, 1000},
		{"5E-1", 1000, 500},
		{"128974848", 1, 128974848},
	}
	for _, c := range cases {
		got, err := parseQuantity(c.text, c.scale)
		if err != nil || got != c.want {
			t.Errorf("parseQuantity(%q, %v) = %d, %v; want %d", c.text, c.scale, got, err, c.want)
		}
	}
	for _, text := range []string{"", "abc", "1Gb", "-1", "1 Gi", "Mi"} {
		if _, err := parseQuantity(text, 1); err == nil {
			t.Errorf("parseQuantity(%q) did not fail", text)
		}
	}
}

// The scheduler reserves the app containers plus sidecars, or the largest
// init container with the sidecars started before it, plus the overhead.
func TestPodRequests(t *testing.T) {
	var spec podSpec
	newContainer := func(cpu, memory, restart string) container {
		var c container
		c.Resources.Requests = map[string]string{"cpu": cpu, "memory": memory}
		c.RestartPolicy = restart
		return c
	}
	spec.Containers = []container{newContainer("100m", "64Mi", ""), newContainer("50m", "64Mi", "")}
	spec.InitContainers = []container{
		newContainer("100m", "32Mi", "Always"),
		newContainer("500m", "16Mi", ""),
	}
	spec.Overhead = map[string]string{"cpu": "10m"}
	got, err := podRequests(spec)
	if err != nil {
		t.Fatal(err)
	}
	// cpu: max(150+100, 500+100) + 10; memory: max(128+32, 16+32).
	if want := (resources{610, 160 << 20}); got != want {
		t.Errorf("podRequests = %v; want %v", got, want)
	}
	if got, err := podRequests(podSpec{}); err != nil || got != (resources{}) {
		t.Errorf("empty Pod: %v, %v", got, err)
	}
}

// requests gives a fixture Pod one container with the CPU and memory
// requests.
func requests(p map[string]any, cpu, memory string) map[string]any {
	set(p, []any{map[string]any{"resources": map[string]any{"requests": map[string]any{"cpu": cpu, "memory": memory}}}}, "spec", "containers")
	return p
}

// newFullCluster is the healthy fixture with server-a's web replica asking
// for 2 CPUs and server-b and server-c each holding Pods that leave 1.5 CPUs
// free.
func newFullCluster(t *testing.T) *fakeCluster {
	f := newHealthyCluster(t)
	requests(f.pods[0], "2", "1Gi")
	requests(f.pods[1], "2500m", "1Gi")
	f.pods = append(f.pods, requests(readyPod(t, "default", "cache-1", "server-c", "", "", `{}`), "2500m", "1Gi"))
	return f
}

func TestDisplacedPodsThatDoNotFitStopBeforeCordon(t *testing.T) {
	f := newFullCluster(t)
	output := runFailure(t, f, "reboot", nil,
		"default/web-1\tcpu=2000m memory=1024Mi\tno remaining node has room",
		"the remaining nodes cannot hold the Pods drained from server-a by their CPU and memory requests")
	mustContain(t, output, "server-b\tcpu=1500m memory=15360Mi\tcpu=1500m memory=15360Mi\n")
	mustContain(t, output, "displaced-pods\t1\tunplaced\t1\n")
	if mutations := f.mutations(); len(mutations) > 0 {
		t.Fatalf("mutations: %v", mutations)
	}

	// --check-only runs the same check; --skip-drain evicts nothing.
	runFailure(t, f, "reboot", []string{"--check-only"}, "the remaining nodes cannot hold the Pods drained from server-a")
	f = newFullCluster(t)
	output = run(t, f, "reboot", "--skip-drain")
	if regexp.MustCompile("fit on the remaining nodes").MatchString(output) {
		t.Fatalf("--skip-drain ran the fit check:\n%s", output)
	}
}

// A Pod goes to the node with the most free CPU that admits it.
func TestDisplacedPodsFitOnTheRoomiestNode(t *testing.T) {
	f := newFullCluster(t)
	requests(f.pods[0], "1", "1Gi")
	requests(f.pods[1], "1", "1Gi")
	output := run(t, f, "reboot", "--check-only")
	mustContain(t, output, "server-b\tcpu=3000m memory=15360Mi\tcpu=2000m memory=14336Mi\n")
	mustContain(t, output, "server-c\tcpu=1500m memory=15360Mi\tcpu=1500m memory=15360Mi\n")
	mustContain(t, output, "displaced-pods\t1\tunplaced\t0\n")
}

// Only the Pods the availability gate needs elsewhere count: a StatefulSet
// may lose one replica, a Deployment pinned to the target waits for it, and
// a Job or DaemonSet Pod is not gated.
func TestDisplacedPodsOnlyCountWhatTheGateNeeds(t *testing.T) {
	f := newFullCluster(t)
	requests(f.pods[0], "100m", "64Mi")
	f.pods = append(f.pods,
		requests(readyPod(t, "default", "kv-0", "server-a", "StatefulSet", "kv", `{"app": "kv"}`), "3", "1Gi"),
		requests(readyPod(t, "default", "kv-1", "server-b", "StatefulSet", "kv", `{"app": "kv"}`), "0", "0"),
		requests(readyPod(t, "default", "kv-2", "server-c", "StatefulSet", "kv", `{"app": "kv"}`), "0", "0"),
		requests(readyPod(t, "default", "edge-1", "server-a", "ReplicaSet", "edge-7f", `{"app": "edge"}`), "3", "1Gi"),
		requests(readyPod(t, "default", "backup-1", "server-a", "Job", "backup", `{}`), "3", "1Gi"),
	)
	f.controllers["default/statefulset/kv"] = decode(t, `{"spec": {"replicas": 3, "selector": {"matchLabels": {"app": "kv"}},
	  "template": {"metadata": {"labels": {"app": "kv"}}, "spec": {}}}, "status": {"readyReplicas": 3}}`)
	f.controllers["default/replicaset/edge-7f"] = decode(t, `{"metadata": {"ownerReferences": [{"kind": "Deployment", "name": "edge"}]}}`)
	f.controllers["default/deployment/edge"] = decode(t, `{"spec": {"replicas": 1, "selector": {"matchLabels": {"app": "edge"}},
	  "template": {"metadata": {"labels": {"app": "edge"}}, "spec": {"nodeSelector": {"kubernetes.io/hostname": "server-a"}}}},
	  "status": {"readyReplicas": 1}}`)
	output := run(t, f, "reboot", "--check-only")
	mustContain(t, output, "displaced-pods\t1\tunplaced\t0\n")
}

// A node the Pod may not run on does not count, whatever its room: here a
// node selector and a one-per-node anti-affinity each rule out one node.
func TestDisplacedPodsHonourPlacement(t *testing.T) {
	f := newFullCluster(t)
	requests(f.pods[0], "1", "1Gi")
	requests(f.pods[1], "0", "0")
	set(f.node("server-b"), "fast", "metadata", "labels", "disk")
	set(f.pods[0], map[string]any{"disk": "fast"}, "spec", "nodeSelector")
	output := run(t, f, "reboot", "--check-only")
	mustContain(t, output, "server-b\tcpu=4000m memory=16384Mi\tcpu=3000m memory=15360Mi\n")

	// A taint the Pod does not tolerate rules server-b out.
	f = newFullCluster(t)
	requests(f.pods[1], "0", "0")
	set(f.node("server-b"), []any{map[string]any{"key": "dedicated", "value": "gpu", "effect": "NoSchedule"}}, "spec", "taints")
	runFailure(t, f, "reboot", []string{"--check-only"}, "default/web-1\tcpu=2000m memory=1024Mi\tno remaining node has room")

	// server-b runs web-2 and server-c has 1.5 CPUs free.
	f = newFullCluster(t)
	requests(f.pods[1], "0", "0")
	antiAffinity := decode(t, `{"podAntiAffinity": {"requiredDuringSchedulingIgnoredDuringExecution": [
	  {"topologyKey": "kubernetes.io/hostname", "labelSelector": {"matchLabels": {"app": "web"}}}]}}`)
	set(f.controllers["default/deployment/web"], antiAffinity, "spec", "template", "spec", "affinity")
	set(f.pods[0], antiAffinity, "spec", "affinity")
	runFailure(t, f, "reboot", []string{"--check-only"}, "default/web-1\tcpu=2000m memory=1024Mi\tno remaining node has room")
}

// After the drain, a replica the scheduler cannot place ends the gate's wait
// after the grace instead of the settle timeout, and the run uncordons the
// node it drained.
func TestUnschedulableReplicaFailsFastAndUncordons(t *testing.T) {
	f := newHealthyCluster(t)
	set(f.controllers["default/deployment/web"], 1.0, "status", "readyReplicas")
	pending := decode(t, `{
	  "metadata": {"namespace": "default", "name": "web-3", "labels": {"app": "web"}, "ownerReferences": [{"kind": "ReplicaSet", "name": "web-5d"}]},
	  "spec": {},
	  "status": {"phase": "Pending", "conditions": [{"type": "PodScheduled", "status": "False", "reason": "Unschedulable",
	    "message": "0/3 nodes are available: 1 node(s) were unschedulable, 2 Insufficient cpu."}]}
	}`)
	f.pods = append(f.pods, pending)
	runFailure(t, f, "reboot", nil,
		"default/deployment/web has 1 Ready replicas; maintenance requires 2, and the scheduler cannot place one: default/web-3: 0/3 nodes are available: 1 node(s) were unschedulable, 2 Insufficient cpu.",
		"server-a was uncordoned")
	if f.sleeps < 60 || f.sleeps > 62 {
		t.Errorf("waited %d polls; want the 5m grace", f.sleeps)
	}
	if get(f.targetNode(), "spec", "unschedulable") != false {
		t.Fatal("node left cordoned")
	}
	mustNotCall(t, f, regexp.QuoteMeta(rebootCommand))
}

// The grace counts an unschedulable replica's continuous wait: one the
// scheduler places in between starts it again, and the gate then runs to
// its settle timeout.
func TestUnschedulableGraceRestarts(t *testing.T) {
	f := newHealthyCluster(t)
	set(f.controllers["default/deployment/web"], 1.0, "status", "readyReplicas")
	pending := decode(t, `{
	  "metadata": {"namespace": "default", "name": "web-3", "labels": {"app": "web"}},
	  "spec": {},
	  "status": {"phase": "Pending", "conditions": [{"type": "PodScheduled", "status": "False", "reason": "Unschedulable", "message": "no room"}]}
	}`)
	lists := 0
	f.respond = func(c Command, line string) (Result, bool) {
		if line != "kubectl -n default get pods -o json" {
			return Result{}, false
		}
		lists++
		if lists%50 == 0 {
			return list(nil), true
		}
		return list([]map[string]any{pending}), true
	}
	o, _ := newOperation(f, options(t, "reboot", "--settle-timeout", "10m", "server-a"))
	err := o.waitForControllerReadyFloor(workload{Namespace: "default", Kind: "deployment", Name: "web"}, 2)
	expectFailure(t, err, "default/deployment/web has 1 Ready replicas; maintenance requires 2")
	if strings.Contains(err.Error(), "scheduler") {
		t.Fatalf("failed fast although the replica was placed in between: %v", err)
	}
}

// A drain gets the longest grace period of the Pods it evicts plus a
// margin, unless the operator chose its timeout.
func TestDrainTimeoutCoversGracePeriod(t *testing.T) {
	f := newHealthyCluster(t)
	set(f.pods[0], 600.0, "spec", "terminationGracePeriodSeconds")
	set(f.pods[2], 3600.0, "spec", "terminationGracePeriodSeconds")
	output := run(t, f, "reboot")
	mustContain(t, output, "raising the drain timeout to 720s: default/web-1 may take 600s to stop")
	mustCall(t, f, `^kubectl drain server-a .* --timeout=720s$`)

	f = newHealthyCluster(t)
	set(f.pods[0], 600.0, "spec", "terminationGracePeriodSeconds")
	output = run(t, f, "reboot", "--drain-timeout", "5m")
	mustContain(t, output, "warning: default/web-1 may take 600s to stop, longer than the drain timeout of 5m allows")
	mustCall(t, f, `^kubectl drain server-a .* --timeout=5m$`)
}
