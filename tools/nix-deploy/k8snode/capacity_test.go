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
	// A Pod-level request replaces the containers' sum for its resource.
	spec.Resources.Requests = map[string]string{"cpu": "2"}
	if got, err := podRequests(spec); err != nil || got != (resources{2010, 160 << 20}) {
		t.Errorf("Pod-level cpu: %v, %v", got, err)
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
	mustContain(t, output, "needed-pods\t1\tunplaced\t1\tother-pods\t0\tunplaced\t0\n")
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
	mustContain(t, output, "needed-pods\t1\tunplaced\t0\tother-pods\t0\tunplaced\t0\n")
}

// Only the Pods the availability gate needs elsewhere must fit: a
// StatefulSet may lose one replica, which is only warned about, a
// Deployment pinned to the target waits for it, and a Job or DaemonSet Pod
// is not recreated elsewhere.
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
	mustContain(t, output, "needed-pods\t1\tunplaced\t0\tother-pods\t1\tunplaced\t1\n")
	mustContain(t, output, "warning: 1 other Pods drained from server-a do not fit after the needed ones")
	mustContain(t, output, "default/kv-0\tcpu=3000m memory=1024Mi\tno remaining node has room\n")

	// Another Pod no node may run waits for the target by design.
	f = newFullCluster(t)
	requests(f.pods[0], "100m", "64Mi")
	f.pods = append(f.pods,
		requests(readyPod(t, "default", "kv-0", "server-a", "StatefulSet", "kv", `{"app": "kv"}`), "100m", "64Mi"),
		requests(readyPod(t, "default", "kv-1", "server-b", "StatefulSet", "kv", `{"app": "kv"}`), "0", "0"),
		requests(readyPod(t, "default", "kv-2", "server-c", "StatefulSet", "kv", `{"app": "kv"}`), "0", "0"),
	)
	set(f.pods[len(f.pods)-3], decode(t, `{"podAntiAffinity": {"requiredDuringSchedulingIgnoredDuringExecution": [
	  {"topologyKey": "kubernetes.io/hostname", "labelSelector": {"matchLabels": {"app": "kv"}}}]}}`), "spec", "affinity")
	f.controllers["default/statefulset/kv"] = decode(t, `{"spec": {"replicas": 3, "selector": {"matchLabels": {"app": "kv"}},
	  "template": {"metadata": {"labels": {"app": "kv"}}, "spec": {}}}, "status": {"readyReplicas": 3}}`)
	output = run(t, f, "reboot", "--check-only")
	mustContain(t, output, "needed-pods\t1\tunplaced\t0\tother-pods\t1\tunplaced\t0\n")
	if strings.Contains(output, "warning") {
		t.Fatalf("warned about a Pod no node may run:\n%s", output)
	}
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

	// A needed Pod no node may run is named as such.
	f = newFullCluster(t)
	set(f.pods[0], map[string]any{"disk": "fast"}, "spec", "nodeSelector")
	runFailure(t, f, "reboot", []string{"--check-only"}, "default/web-1\tcpu=2000m memory=1024Mi\tno remaining node may run it")

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

	// The anti-affinity term, not the controller's selector, decides: a
	// Pod of another release that the term selects also rules a node out.
	f = newFullCluster(t)
	requests(f.pods[0], "1", "1Gi")
	requests(f.pods[1], "0", "0")
	set(f.pods[1], map[string]any{"app": "web", "release": "green"}, "metadata", "labels")
	set(f.pods[1], []any{map[string]any{"kind": "ReplicaSet", "name": "web-green"}}, "metadata", "ownerReferences")
	set(f.pods[0], map[string]any{"app": "web", "release": "blue"}, "metadata", "labels")
	set(f.pods[0], antiAffinity, "spec", "affinity")
	set(f.controllers["default/deployment/web"], map[string]any{"matchLabels": map[string]any{"app": "web", "release": "blue"}}, "spec", "selector")
	set(f.controllers["default/deployment/web"], 1.0, "status", "readyReplicas")
	requests(f.pods[len(f.pods)-1], "3500m", "1Gi")
	runFailure(t, f, "reboot", []string{"--check-only"}, "default/web-1\tcpu=1000m memory=1024Mi\tno remaining node has room")
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
	// After the drain the replacement of web-1 is web-3, which waits.
	f.respond = func(c Command, line string) (Result, bool) {
		if line == "kubectl -n default get pods -o json" && f.callIndex(`^kubectl drain`) >= 0 {
			return list([]map[string]any{f.pods[1], pending}), true
		}
		return Result{}, false
	}
	runFailure(t, f, "reboot", nil,
		"default/deployment/web has 1 Ready replicas; maintenance requires 2, and the scheduler cannot place 1: default/web-3: 0/3 nodes are available: 1 node(s) were unschedulable, 2 Insufficient cpu.",
		"server-a was uncordoned")
	if f.sleeps < 60 || f.sleeps > 62 {
		t.Errorf("waited %d polls; want the 5m grace", f.sleeps)
	}
	if get(f.targetNode(), "spec", "unschedulable") != false {
		t.Fatal("node left cordoned")
	}
	mustNotCall(t, f, regexp.QuoteMeta(rebootCommand))
}

// Each replica's wait counts on its own, and the gate fails early only
// once the replicas not stuck for the grace cannot reach the minimum: two
// replicas Unschedulable in overlapping windows, neither for the whole
// grace, a replica the floor lets stay unplaced, a rollout's surge Pod, an
// unrelated Pod with the same labels and a preemptor waiting for its
// victims do not fail it.
func TestUnschedulableReplicasCountSeparately(t *testing.T) {
	owned := func(p map[string]any) map[string]any {
		set(p, []any{map[string]any{"kind": "ReplicaSet", "name": "web-5d"}}, "metadata", "ownerReferences")
		return p
	}
	unschedulable := func(name string) map[string]any {
		return owned(decode(t, `{"metadata": {"namespace": "default", "name": "`+name+`", "labels": {"app": "web"}},
		  "spec": {}, "status": {"phase": "Pending", "conditions": [{"type": "PodScheduled", "status": "False",
		  "reason": "Unschedulable", "message": "no room"}]}}`))
	}
	ready := readyPod(t, "default", "web-r", "server-b", "ReplicaSet", "web-5d", `{"app": "web"}`)
	starting := owned(decode(t, `{"metadata": {"namespace": "default", "name": "web-s", "labels": {"app": "web"}},
	  "spec": {"nodeName": "server-c"}, "status": {"phase": "Pending", "conditions": [{"type": "PodScheduled", "status": "True"}]}}`))
	web := workload{Namespace: "default", Kind: "deployment", Name: "web"}
	wait := func(desired float64, minimum int, serve func(poll int) []map[string]any) error {
		f := newHealthyCluster(t)
		set(f.controllers["default/deployment/web"], desired, "spec", "replicas")
		set(f.controllers["default/deployment/web"], 1.0, "status", "readyReplicas")
		polls := 0
		f.respond = func(c Command, line string) (Result, bool) {
			if line != "kubectl -n default get pods -o json" {
				return Result{}, false
			}
			polls++
			return list(serve(polls)), true
		}
		o, _ := newOperation(f, options(t, "reboot", "--settle-timeout", "15m", "server-a"))
		return o.waitForControllerReadyFloor(web, minimum)
	}
	failsFast := func(err error) bool {
		return err != nil && strings.Contains(err.Error(), "scheduler")
	}

	// A is blocked for four minutes (48 polls), B from minute two to six.
	a, b := unschedulable("web-a"), unschedulable("web-b")
	err := wait(2, 2, func(poll int) []map[string]any {
		pods := []map[string]any{ready}
		if poll <= 48 {
			pods = append(pods, a)
		}
		if poll > 24 && poll <= 72 {
			pods = append(pods, b)
		}
		return pods
	})
	expectFailure(t, err, "default/deployment/web has 1 Ready replicas; maintenance requires 2")
	if failsFast(err) {
		t.Fatalf("overlapping waits failed fast: %v", err)
	}

	// The floor needs 2 of 3: one stuck replica leaves two that can make
	// it; two stuck do not.
	if err := wait(3, 2, func(int) []map[string]any { return []map[string]any{ready, starting, a} }); failsFast(err) {
		t.Fatalf("a replica the floor allows to stay unplaced failed fast: %v", err)
	}
	err = wait(3, 2, func(int) []map[string]any { return []map[string]any{ready, a, b} })
	expectFailure(t, err, "the scheduler cannot place 2: default/web-a: no room; default/web-b: no room")

	// A rollout's surge Pod stuck while a replacement still starts.
	if err := wait(2, 2, func(int) []map[string]any { return []map[string]any{ready, starting, a} }); failsFast(err) {
		t.Fatalf("a surge Pod failed fast: %v", err)
	}

	// A Job's Pod with the same labels, and a nominated preemptor.
	job := unschedulable("backup-1")
	set(job, []any{map[string]any{"kind": "Job", "name": "backup"}}, "metadata", "ownerReferences")
	nominated := unschedulable("web-c")
	set(nominated, "server-b", "status", "nominatedNodeName")
	if err := wait(2, 2, func(int) []map[string]any { return []map[string]any{ready, job, nominated} }); failsFast(err) {
		t.Fatalf("a Job's Pod or a nominated preemptor failed fast: %v", err)
	}
}

// The grace counts an unschedulable replica's continuous wait: one the
// scheduler places in between starts it again, and the gate then runs to
// its settle timeout.
func TestUnschedulableGraceRestarts(t *testing.T) {
	f := newHealthyCluster(t)
	set(f.controllers["default/deployment/web"], 1.0, "status", "readyReplicas")
	pending := decode(t, `{
	  "metadata": {"namespace": "default", "name": "web-3", "labels": {"app": "web"}, "ownerReferences": [{"kind": "ReplicaSet", "name": "web-5d"}]},
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

	// A zero timeout already waits without a limit.
	f = newHealthyCluster(t)
	set(f.pods[0], 600.0, "spec", "terminationGracePeriodSeconds")
	output = run(t, f, "reboot", "--drain-timeout", "0")
	if strings.Contains(output, "drain timeout") {
		t.Fatalf("zero drain timeout warned or was raised:\n%s", output)
	}
	mustCall(t, f, `^kubectl drain server-a .* --timeout=0s$`)
}

// Spreading the Pods can leave one out of a set that fits; the check then
// packs them tightly instead.
func TestPlaceNeededPacksWhenSpreadingFails(t *testing.T) {
	node := func(name string, cpu int64) *roomyNode {
		var n node
		n.Metadata.Name = name
		r := resources{cpu, 16 << 30}
		return &roomyNode{node: n, allocatable: r, free: r}
	}
	pod := func(name string, cpu int64) displacedPod {
		var p pod
		p.Metadata.Namespace, p.Metadata.Name = "default", name
		return displacedPod{pod: p, requests: resources{cpu, 1 << 30}}
	}
	needed := []displacedPod{pod("a", 3000), pod("b", 2000), pod("c", 2000)}
	room := []*roomyNode{node("server-b", 4000), node("server-c", 3000)}
	if noRoom, barred := place(room, needed, nil, false); len(noRoom) != 1 || len(barred) != 0 {
		t.Fatalf("spreading placed what it should not: %v %v", noRoom, barred)
	}
	room = []*roomyNode{node("server-b", 4000), node("server-c", 3000)}
	if unplaced := placeNeeded(room, needed, nil); len(unplaced) != 0 {
		t.Fatalf("a set that fits was refused: %v", unplaced)
	}
	if room[0].free.cpu != 0 || room[1].free.cpu != 0 || len(room[0].placed) != 2 {
		t.Errorf("packing: %v %v", room[0], room[1])
	}
	room = []*roomyNode{node("server-b", 4000), node("server-c", 3000)}
	if unplaced := placeNeeded(room, append(needed, pod("d", 100)), nil); len(unplaced) != 1 {
		t.Fatalf("a set that does not fit passed: %v", unplaced)
	}
}

// The target's Pods count as Ready by their Ready condition, so a replica
// whose readiness gate holds it back does not raise what must fit.
func TestDisplacedPodsCountPodReadiness(t *testing.T) {
	f := newFullCluster(t)
	requests(f.pods[1], "0", "0")
	antiAffinity := decode(t, `{"podAntiAffinity": {"requiredDuringSchedulingIgnoredDuringExecution": [
	  {"topologyKey": "kubernetes.io/hostname", "labelSelector": {"matchLabels": {"app": "web"}}}]}}`)
	set(f.controllers["default/deployment/web"], 3.0, "spec", "replicas")
	set(f.controllers["default/deployment/web"], antiAffinity, "spec", "template", "spec", "affinity")
	f.pods = append(f.pods, readyPod(t, "default", "web-3", "server-c", "ReplicaSet", "web-5d", `{"app": "web"}`))
	for _, p := range []map[string]any{f.pods[0], f.pods[1], f.pods[len(f.pods)-1]} {
		set(p, antiAffinity, "spec", "affinity")
	}
	set(f.pods[0], []any{map[string]any{"type": "Ready", "status": "False"}}, "status", "conditions")
	output := run(t, f, "reboot", "--check-only")
	mustContain(t, output, "needed-pods\t0\tunplaced\t0\t")
}

// A Pod in the middle of an in-place resize holds the larger of its new
// requests and what the node still allocates to it.
func TestResidentPodsHoldResizeAllocation(t *testing.T) {
	f := newFullCluster(t)
	requests(f.pods[1], "1", "1Gi")
	set(f.pods[1], map[string]any{"cpu": "3", "memory": "1Gi"}, "status", "containerStatuses", 0, "allocatedResources")
	output := runFailure(t, f, "reboot", []string{"--check-only"}, "default/web-1\tcpu=2000m memory=1024Mi\tno remaining node has room")
	mustContain(t, output, "server-b\tcpu=1000m memory=15360Mi\t")
}
