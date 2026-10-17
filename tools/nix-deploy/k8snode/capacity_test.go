package k8snode

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

func TestSelectorMatches(t *testing.T) {
	selector := &labelSelector{
		MatchLabels: map[string]string{"app": "web"},
		MatchExpressions: []requirement{
			{Key: "tier", Operator: "NotIn", Values: []string{"batch"}},
			{Key: "track", Operator: "DoesNotExist"},
		},
	}
	cases := []struct {
		labels map[string]string
		want   bool
	}{
		{map[string]string{"app": "web"}, true},
		{map[string]string{"app": "web", "tier": "front"}, true},
		{map[string]string{"app": "web", "tier": "batch"}, false},
		{map[string]string{"app": "web", "track": "canary"}, false},
		{map[string]string{"app": "api"}, false},
	}
	for _, c := range cases {
		if got := selectorMatches(c.labels, selector); got != c.want {
			t.Errorf("selectorMatches(%v) = %v; want %v", c.labels, got, c.want)
		}
	}
	if !selectorMatches(nil, &labelSelector{}) || selectorMatches(nil, nil) {
		t.Error("an empty selector selects every Pod and a nil one none")
	}
}

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

// webRequests gives the target's web replica and the templates of its
// ReplicaSet, from which the replacement is made, and Deployment the CPU
// and memory requests.
func webRequests(f *fakeCluster, cpu, memory string) {
	requests(f.pods[0], cpu, memory)
	templateRequests(f, "default/replicaset/web-5d", cpu, memory)
	templateRequests(f, "default/deployment/web", cpu, memory)
}

// templateRequests gives a controller's Pod template the requests.
func templateRequests(f *fakeCluster, ref, cpu, memory string) {
	set(f.controllers[ref], []any{map[string]any{"resources": map[string]any{"requests": map[string]any{"cpu": cpu, "memory": memory}}}},
		"spec", "template", "spec", "containers")
}

// newFullCluster is the healthy fixture with server-a's web replica asking
// for 2 CPUs and server-b and server-c each holding Pods that leave 1.5 CPUs
// free.
func newFullCluster(t *testing.T) *fakeCluster {
	f := newHealthyCluster(t)
	webRequests(f, "2", "1Gi")
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
	mustContain(t, output, "needed-pods\t1\tunplaced\t1\tunsettled\t0\tother-pods\t0\tunplaced\t0\n")
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

// A Pod goes to the node with the largest free share that admits it.
func TestDisplacedPodsFitOnTheRoomiestNode(t *testing.T) {
	f := newFullCluster(t)
	webRequests(f, "1", "1Gi")
	requests(f.pods[1], "1", "1Gi")
	output := run(t, f, "reboot", "--check-only")
	mustContain(t, output, "server-b\tcpu=3000m memory=15360Mi\tcpu=2000m memory=14336Mi\n")
	mustContain(t, output, "server-c\tcpu=1500m memory=15360Mi\tcpu=1500m memory=15360Mi\n")
	mustContain(t, output, "needed-pods\t1\tunplaced\t0\tunsettled\t0\tother-pods\t0\tunplaced\t0\n")
}

// Only the Pods the availability gate needs elsewhere must fit: a
// StatefulSet may lose one replica, which is only warned about, a
// Deployment pinned to the target waits for it, and a Job or DaemonSet Pod
// is not recreated elsewhere.
func TestDisplacedPodsOnlyCountWhatTheGateNeeds(t *testing.T) {
	f := newFullCluster(t)
	webRequests(f, "100m", "64Mi")
	f.pods = append(f.pods,
		requests(readyPod(t, "default", "kv-0", "server-a", "StatefulSet", "kv", `{"app": "kv"}`), "3", "1Gi"),
		requests(readyPod(t, "default", "kv-1", "server-b", "StatefulSet", "kv", `{"app": "kv"}`), "0", "0"),
		requests(readyPod(t, "default", "kv-2", "server-c", "StatefulSet", "kv", `{"app": "kv"}`), "0", "0"),
		requests(readyPod(t, "default", "edge-1", "server-a", "ReplicaSet", "edge-7f", `{"app": "edge"}`), "3", "1Gi"),
		requests(readyPod(t, "default", "backup-1", "server-a", "Job", "backup", `{}`), "3", "1Gi"),
	)
	f.controllers["default/statefulset/kv"] = decode(t, `{"spec": {"replicas": 3, "selector": {"matchLabels": {"app": "kv"}},
	  "template": {"metadata": {"labels": {"app": "kv"}}, "spec": {}}}, "status": {"readyReplicas": 3}}`)
	templateRequests(f, "default/statefulset/kv", "3", "1Gi")
	f.controllers["default/replicaset/edge-7f"] = decode(t, `{"metadata": {"ownerReferences": [{"kind": "Deployment", "name": "edge"}]}}`)
	f.controllers["default/deployment/edge"] = decode(t, `{"spec": {"replicas": 1, "selector": {"matchLabels": {"app": "edge"}},
	  "template": {"metadata": {"labels": {"app": "edge"}}, "spec": {"nodeSelector": {"kubernetes.io/hostname": "server-a"}}}},
	  "status": {"readyReplicas": 1}}`)
	output := run(t, f, "reboot", "--check-only")
	mustContain(t, output, "needed-pods\t1\tunplaced\t0\tunsettled\t0\tother-pods\t1\tunplaced\t1\n")
	mustContain(t, output, "warning: 1 other Pods drained from server-a do not fit after the needed ones")
	mustContain(t, output, "default/kv-0\tcpu=3000m memory=1024Mi\tno remaining node has room\n")

	// Another Pod no node may run waits for the target by design.
	f = newFullCluster(t)
	webRequests(f, "100m", "64Mi")
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
	mustContain(t, output, "needed-pods\t1\tunplaced\t0\tunsettled\t0\tother-pods\t1\tunplaced\t0\n")
	if strings.Contains(output, "warning") {
		t.Fatalf("warned about a Pod no node may run:\n%s", output)
	}
}

// A node the Pod may not run on does not count, whatever its room: here a
// node selector and a one-per-node anti-affinity each rule out one node.
func TestDisplacedPodsHonourPlacement(t *testing.T) {
	f := newFullCluster(t)
	webRequests(f, "1", "1Gi")
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
	webRequests(f, "1", "1Gi")
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

	// A replica whose ReplicaSet cannot be read still counts toward the
	// minimum, though it is never taken for stuck.
	unread := owned(decode(t, `{"metadata": {"namespace": "default", "name": "web-u", "labels": {"app": "web"}},
	  "spec": {"nodeName": "server-c"}, "status": {"phase": "Pending", "conditions": [{"type": "PodScheduled", "status": "True"}]}}`))
	set(unread, "web-6e", "metadata", "ownerReferences", 0, "name")
	if err := wait(3, 2, func(int) []map[string]any { return []map[string]any{ready, unread, a} }); failsFast(err) {
		t.Fatalf("a replica with an unreadable ReplicaSet was dropped: %v", err)
	}
	unreadStuck := unschedulable("web-v")
	set(unreadStuck, "web-6e", "metadata", "ownerReferences", 0, "name")
	if err := wait(2, 2, func(int) []map[string]any { return []map[string]any{ready, unreadStuck} }); failsFast(err) {
		t.Fatalf("a Pod with an unreadable ReplicaSet was taken for stuck: %v", err)
	}

	// A controller selector's NotIn selects Pods without the label, as in
	// Kubernetes, so a stuck one is found.
	err = func() error {
		f := newHealthyCluster(t)
		set(f.controllers["default/deployment/web"], map[string]any{"matchLabels": map[string]any{"app": "web"},
			"matchExpressions": []any{map[string]any{"key": "tier", "operator": "NotIn", "values": []any{"batch"}}}}, "spec", "selector")
		set(f.controllers["default/deployment/web"], 1.0, "status", "readyReplicas")
		f.respond = func(c Command, line string) (Result, bool) {
			if line != "kubectl -n default get pods -o json" {
				return Result{}, false
			}
			return list([]map[string]any{ready, a}), true
		}
		o, _ := newOperation(f, options(t, "reboot", "--settle-timeout", "15m", "server-a"))
		return o.waitForControllerReadyFloor(web, 2)
	}()
	if !failsFast(err) {
		t.Fatalf("a stuck replica selected by NotIn was missed: %v", err)
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
	mustContain(t, output, "warning: default/web-1 may take 600s to stop; with a 2m0s margin that is more than the drain timeout of 5m")
	mustCall(t, f, `^kubectl drain server-a .* --timeout=5m$`)

	// A mirror Pod, which the drain skips, does not raise it, even with an
	// empty annotation.
	f = newHealthyCluster(t)
	f.pods = append(f.pods, readyPod(t, "kube-system", "static-a", "server-a", "Node", "server-a", `{}`))
	set(f.pods[len(f.pods)-1], map[string]any{"kubernetes.io/config.mirror": ""}, "metadata", "annotations")
	set(f.pods[len(f.pods)-1], 3600.0, "spec", "terminationGracePeriodSeconds")
	output = run(t, f, "reboot")
	mustCall(t, f, `^kubectl drain server-a .* --timeout=10m$`)

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
// searches for a placement, and refuses only a set that does not fit.
func TestPlaceNeededSearchesWhenSpreadingFails(t *testing.T) {
	node := func(name string, cpu int64) *roomyNode {
		var n node
		n.Metadata.Name = name
		r := resources{cpu, 16 << 30}
		return &roomyNode{node: n, allocatable: r, free: r}
	}
	pods := func(cpus ...int64) []displacedPod {
		var displaced []displacedPod
		for i, cpu := range cpus {
			var p pod
			p.Metadata.Namespace, p.Metadata.Name = "default", fmt.Sprintf("p%d", i)
			displaced = append(displaced, displacedPod{pod: p, requests: resources{cpu, 1 << 30}})
		}
		return displaced
	}
	cases := []struct {
		name     string
		nodes    []int64
		pods     []int64
		spread   int
		unplaced int
	}{
		{"tighter fit", []int64{4000, 3000}, []int64{3000, 2000, 2000}, 1, 0},
		{"neither greedy order", []int64{10000, 10000}, []int64{5000, 4000, 3000, 2000, 2000, 2000, 2000}, 1, 0},
		{"too much", []int64{4000, 3000}, []int64{3000, 2000, 2000, 100}, 1, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var room []*roomyNode
			for i, cpu := range c.nodes {
				room = append(room, node(fmt.Sprintf("server-%d", i), cpu))
			}
			if noRoom, _ := place(room, pods(c.pods...), nil); len(noRoom) != c.spread {
				t.Fatalf("spreading left out %v", noRoom)
			}
			for _, r := range room {
				r.free, r.placed = r.allocatable, nil
			}
			unplaced, unsure := placeNeeded(room, pods(c.pods...), nil, packBudget)
			if len(unsure) > 0 {
				t.Fatalf("unsettled %v", unsure)
			}
			if len(unplaced) != c.unplaced {
				t.Fatalf("unplaced %v", unplaced)
			}
			if c.unplaced == 0 {
				placed := 0
				for _, r := range room {
					if r.free.cpu < 0 {
						t.Errorf("%s overcommitted: %v", r.node.Metadata.Name, r.free)
					}
					placed += len(r.placed)
				}
				if placed != len(c.pods) {
					t.Errorf("placed %d of %d", placed, len(c.pods))
				}
			}
		})
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

// The scheduler holds, per container, the larger of its request and what
// the node allocated to it or it runs with, then adds the Pod overhead.
func TestResidentRequestsPerContainer(t *testing.T) {
	var p pod
	newContainer := func(name, cpu string) container {
		var c container
		c.Name = name
		c.Resources.Requests = map[string]string{"cpu": cpu}
		return c
	}
	p.Spec.Containers = []container{newContainer("a", "2"), newContainer("b", "1")}
	p.Spec.Overhead = map[string]string{"cpu": "100m"}
	var a, b containerStatus
	a.Name, a.AllocatedResources = "a", map[string]string{"cpu": "1"}
	b.Name = "b"
	b.Resources.Requests = map[string]string{"cpu": "3"}
	p.Status.ContainerStatuses = []containerStatus{a, b}
	got, err := residentRequests(p)
	if err != nil || got.cpu != 5100 {
		t.Fatalf("residentRequests = %v, %v; want 5100m", got, err)
	}
	// A sidecar init container's status counts too.
	p.Spec.InitContainers = []container{newContainer("s", "100m")}
	p.Spec.InitContainers[0].RestartPolicy = "Always"
	var s containerStatus
	s.Name, s.AllocatedResources = "s", map[string]string{"cpu": "1"}
	p.Status.InitContainerStatuses = []containerStatus{s}
	if got, err := residentRequests(p); err != nil || got.cpu != 6100 {
		t.Fatalf("with sidecar: %v, %v; want 6100m", got, err)
	}
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

// The search tries the Pods with the fewest nodes first, so one that only
// one node may run is not crowded out; when it runs out of steps, the
// spreading's misses are unsettled, not refused; and when it succeeds, a
// Pod the spreading barred only by where it put a neighbour is placed.
func TestPlaceNeededSearchOrderAndLimit(t *testing.T) {
	node := func(name string, cpu, memory int64) *roomyNode {
		var n node
		n.Metadata.Name = name
		n.Metadata.Labels = map[string]string{"kubernetes.io/hostname": name}
		r := resources{cpu, memory}
		return &roomyNode{node: n, allocatable: r, free: r}
	}
	newPod := func(name string, cpu, memory int64) displacedPod {
		var p pod
		p.Metadata.Namespace, p.Metadata.Name = "default", name
		return displacedPod{pod: p, requests: resources{cpu, memory}}
	}
	needed := []displacedPod{newPod("big", 200, 12<<30)}
	for i := range 20 {
		needed = append(needed, newPod(fmt.Sprintf("small-%d", i), 100, 16<<20))
	}
	pinned := newPod("pinned", 50, 8<<30)
	pinned.pod.Spec.NodeSelector = map[string]string{"kubernetes.io/hostname": "server-b"}
	needed = append(needed, pinned)
	room := []*roomyNode{node("server-b", 4000, 16<<30), node("server-c", 4000, 16<<30)}
	unplaced, unsure := placeNeeded(room, needed, nil, packBudget)
	if len(unplaced) != 0 || len(unsure) != 0 {
		t.Fatalf("unplaced %v, unsettled %v", unplaced, unsure)
	}

	// With a budget too small to settle it, the miss is unsettled.
	room = []*roomyNode{node("server-b", 10000, 16<<30), node("server-c", 10000, 16<<30)}
	var tight []displacedPod
	for i, cpu := range []int64{5000, 4000, 3000, 2000, 2000, 2000, 2000} {
		tight = append(tight, newPod(fmt.Sprintf("p%d", i), cpu, 1<<30))
	}
	unplaced, unsure = placeNeeded(room, tight, nil, 3)
	if len(unplaced) != 0 || len(unsure) != 1 {
		t.Fatalf("unplaced %v, unsettled %v", unplaced, unsure)
	}

	// The spreading puts green on server-b and so bars blue, which only
	// server-b may run; the search places both.
	room = []*roomyNode{node("server-b", 4000, 16<<30), node("server-c", 6500, 16<<30)}
	anti := repelling(map[string]string{"app": "web"})
	green, blue, filler := newPod("green", 3000, 1<<30), newPod("blue", 1000, 1<<30), newPod("filler", 3000, 1<<30)
	green.pod.Metadata.Labels = map[string]string{"app": "web"}
	blue.pod.Metadata.Labels = map[string]string{"app": "web"}
	green.pod.Spec.Affinity, blue.pod.Spec.Affinity = anti, anti
	blue.pod.Spec.NodeSelector = map[string]string{"kubernetes.io/hostname": "server-b"}
	filler.pod.Spec.NodeSelector = map[string]string{"kubernetes.io/hostname": "server-c"}
	if noRoom, barred := place(room, []displacedPod{green, blue, filler}, nil); len(noRoom)+len(barred) == 0 {
		t.Fatal("the spreading placed everything; the case tests nothing")
	}
	for _, r := range room {
		r.free, r.placed = r.allocatable, nil
	}
	unplaced, unsure = placeNeeded(room, []displacedPod{green, blue, filler}, nil, packBudget)
	if len(unplaced) != 0 || len(unsure) != 0 {
		t.Fatalf("unplaced %v, unsettled %v", unplaced, unsure)
	}
}

// A resize the node cannot grant leaves the container at what it holds.
func TestResidentRequestsInfeasibleResize(t *testing.T) {
	var p pod
	var c container
	c.Name = "a"
	c.Resources.Requests = map[string]string{"cpu": "5"}
	p.Spec.Containers = []container{c}
	var s containerStatus
	s.Name, s.AllocatedResources = "a", map[string]string{"cpu": "1"}
	p.Status.ContainerStatuses = []containerStatus{s}
	if got, _ := residentRequests(p); got.cpu != 5000 {
		t.Fatalf("pending resize: %v; want 5000m", got)
	}
	p.Status.Conditions = []condition{{Type: "PodResizePending", Status: "True", Reason: "Infeasible"}}
	if got, _ := residentRequests(p); got.cpu != 1000 {
		t.Fatalf("infeasible resize: %v; want 1000m", got)
	}
	p.Status.Conditions = nil
	p.Status.Resize = "Infeasible"
	if got, _ := residentRequests(p); got.cpu != 1000 {
		t.Fatalf("infeasible resize (status.resize): %v; want 1000m", got)
	}
	// A status without resources does not lower the request.
	p.Status.ContainerStatuses = []containerStatus{{Name: "a"}}
	if got, _ := residentRequests(p); got.cpu != 5000 {
		t.Fatalf("status without resources: %v; want 5000m", got)
	}
}

// The search settles a plain shortage at once by the total requests, and
// a shortage hidden by many identical small Pods by trying each of their
// arrangements once; when it cannot settle, every Pod the spreading left
// out is unsettled, one barred only by a neighbour included.
func TestPackSettlesShortages(t *testing.T) {
	node := func(name string, cpu int64) *roomyNode {
		var n node
		n.Metadata.Name = name
		r := resources{cpu, 16 << 30}
		return &roomyNode{node: n, allocatable: r, free: r}
	}
	newPod := func(name string, cpu int64) displacedPod {
		var p pod
		p.Metadata.Namespace, p.Metadata.Name = "default", name
		return displacedPod{pod: p, requests: resources{cpu, 1 << 20}}
	}
	many := func(n int, cpu int64, prefix string) []displacedPod {
		var displaced []displacedPod
		for i := range n {
			displaced = append(displaced, newPod(fmt.Sprintf("%s%d", prefix, i), cpu))
		}
		return displaced
	}

	// 21 x 100m against 2 x 1000m.
	room := []*roomyNode{node("server-b", 1000), node("server-c", 1000)}
	unplaced, unsure := placeNeeded(room, many(21, 100, "p"), nil, packBudget)
	if len(unplaced) != 1 || len(unsure) != 0 {
		t.Fatalf("plain shortage: unplaced %v, unsettled %v", unplaced, unsure)
	}

	// 40 Pods of different sizes, 2380m against 2000m: settled by the
	// total, not by the steps.
	var distinct []displacedPod
	for i := range 40 {
		distinct = append(distinct, newPod(fmt.Sprintf("d%d", i), int64(40+i)))
	}
	room = []*roomyNode{node("server-b", 1000), node("server-c", 1000)}
	if found, settled := pack(room, distinct, nil, packBudget); found || !settled {
		t.Fatalf("distinct shortage: found %v, settled %v", found, settled)
	}

	// Two 600m Pods cannot share a 1000m node, whatever the 20 x 10m do.
	room = []*roomyNode{node("server-b", 1000), node("server-c", 1000)}
	fragmented := append(many(3, 600, "big"), many(20, 10, "small")...)
	if found, settled := pack(room, fragmented, nil, packBudget); found || !settled {
		t.Fatalf("fragmentation: found %v, settled %v", found, settled)
	}

	// Three Pods that allow one per node, on two nodes.
	web := func(name string) displacedPod {
		d := newPod(name, 50)
		d.pod.Metadata.Labels = map[string]string{"app": "web"}
		d.pod.Spec.Affinity = repelling(map[string]string{"app": "web"})
		return d
	}
	needed := append(many(20, 100, "p"), web("web-1"), web("web-2"), web("web-3"))
	room = []*roomyNode{node("server-b", 4000), node("server-c", 4000)}
	unplaced, unsure = placeNeeded(room, needed, nil, packBudget)
	if len(unplaced) != 1 || !strings.Contains(unplaced[0], "no remaining node may run it") || len(unsure) != 0 {
		t.Fatalf("settled bar: unplaced %v, unsettled %v", unplaced, unsure)
	}
	room = []*roomyNode{node("server-b", 4000), node("server-c", 4000)}
	unplaced, unsure = placeNeeded(room, needed, nil, 1)
	if len(unplaced) != 0 || len(unsure) != 1 || !strings.Contains(unsure[0], "no remaining node may run it") {
		t.Fatalf("unsettled bar: unplaced %v, unsettled %v", unplaced, unsure)
	}
}

// Pods with an anti-affinity are not taken for interchangeable: here the
// only placement puts the first of two equal Pods on the later node.
func TestPackKeepsOrderForAntiAffinity(t *testing.T) {
	node := func(name string, cpu int64) *roomyNode {
		var n node
		n.Metadata.Name = name
		n.Metadata.Labels = map[string]string{"kubernetes.io/hostname": name}
		r := resources{cpu, 16 << 30}
		return &roomyNode{node: n, allocatable: r, free: r}
	}
	newPod := func(name, label string) displacedPod {
		var p pod
		p.Metadata.Namespace, p.Metadata.Name = "default", name
		p.Metadata.Labels = map[string]string{"app": label}
		return displacedPod{pod: p, requests: resources{1000, 1 << 20}}
	}
	// w runs only on server-b; a may not join it; b may go anywhere.
	w, a, b := newPod("w", "z"), newPod("a", "q"), newPod("b", "q")
	w.pod.Spec.NodeSelector = map[string]string{"kubernetes.io/hostname": "server-b"}
	a.pod.Spec.Affinity = repelling(map[string]string{"app": "z"})
	room := []*roomyNode{node("server-b", 2000), node("server-c", 1000)}
	if found, settled := pack(room, []displacedPod{w, a, b}, nil, packBudget); !found || !settled {
		t.Fatalf("found %v, settled %v", found, settled)
	}
}

// repelling is a required hostname anti-affinity against Pods with the
// labels.
func repelling(labels map[string]string) *affinity {
	a := &affinity{PodAntiAffinity: &struct {
		Required []podAffinityTerm `json:"requiredDuringSchedulingIgnoredDuringExecution"`
	}{}}
	a.PodAntiAffinity.Required = []podAffinityTerm{{TopologyKey: hostnameLabel, LabelSelector: &labelSelector{MatchLabels: labels}}}
	return a
}

// Pods with different labels are not interchangeable, since another Pod's
// anti-affinity tells them apart, and a running Pod's anti-affinity keeps
// a Pod it selects off its node, as the scheduler enforces both ways.
func TestAntiAffinityBothWays(t *testing.T) {
	node := func(name string, cpu int64) *roomyNode {
		var n node
		n.Metadata.Name = name
		r := resources{cpu, 16 << 30}
		return &roomyNode{node: n, allocatable: r, free: r}
	}
	newPod := func(name, app string, cpu int64) displacedPod {
		var p pod
		p.Metadata.Namespace, p.Metadata.Name = "default", name
		p.Metadata.Labels = map[string]string{"app": app}
		return displacedPod{pod: p, requests: resources{cpu, 1 << 20}}
	}
	// The only placement is a on server-c, b and c on server-b.
	a, b, c := newPod("a", "web", 1000), newPod("b", "api", 1000), newPod("c", "web", 100)
	c.pod.Spec.Affinity = repelling(map[string]string{"app": "web"})
	room := []*roomyNode{node("server-b", 1100), node("server-c", 1000)}
	if found, settled := pack(room, []displacedPod{a, b, c}, nil, packBudget); !found || !settled {
		t.Fatalf("found %v, settled %v", found, settled)
	}

	// A Pod the check placed repels a later one it selects, though the
	// later one has no anti-affinity of its own.
	x, y := newPod("x", "x", 1000), newPod("y", "web", 1000)
	x.pod.Spec.Affinity = repelling(map[string]string{"app": "web"})
	room = []*roomyNode{node("server-b", 2000), node("server-c", 500)}
	if noRoom, barred := place(room, []displacedPod{x, y}, nil); len(noRoom)+len(barred) != 1 {
		t.Fatalf("placed repels: noRoom %v, barred %v", noRoom, barred)
	}

	// A running web Pod on server-b repels another web Pod, which then
	// fits nowhere.
	var resident pod
	resident.Metadata.Namespace, resident.Metadata.Name = "default", "web-0"
	resident.Metadata.Labels = map[string]string{"app": "web"}
	resident.Spec.NodeName = "server-b"
	resident.Spec.Affinity = repelling(map[string]string{"app": "web"})
	resident.Status.Phase = "Running"
	room = []*roomyNode{node("server-b", 4000), node("server-c", 100)}
	noRoom, barred := place(room, []displacedPod{newPod("web-1", "web", 1000)}, []pod{resident})
	if len(noRoom) != 1 || len(barred) != 0 {
		t.Fatalf("noRoom %v, barred %v", noRoom, barred)
	}
	// A NotIn term selects a Pod without the label, as in Kubernetes.
	notIn := resident
	notIn.Spec.Affinity = &affinity{PodAntiAffinity: &struct {
		Required []podAffinityTerm `json:"requiredDuringSchedulingIgnoredDuringExecution"`
	}{Required: []podAffinityTerm{{TopologyKey: hostnameLabel, LabelSelector: &labelSelector{
		MatchExpressions: []requirement{{Key: "tier", Operator: "NotIn", Values: []string{"batch"}}}}}}}}
	room = []*roomyNode{node("server-b", 4000), node("server-c", 100)}
	if noRoom, barred := place(room, []displacedPod{newPod("api-1", "api", 1000)}, []pod{notIn}); len(noRoom) != 1 || len(barred) != 0 {
		t.Fatalf("NotIn: noRoom %v, barred %v", noRoom, barred)
	}

	// In another namespace it does not.
	other := newPod("web-1", "web", 1000)
	other.pod.Metadata.Namespace = "other"
	room = []*roomyNode{node("server-b", 4000), node("server-c", 100)}
	if noRoom, barred := place(room, []displacedPod{other}, []pod{resident}); len(noRoom)+len(barred) != 0 {
		t.Fatalf("other namespace: noRoom %v, barred %v", noRoom, barred)
	}
}

// A recreated Pod under the same name starts its grace again.
func TestUnschedulableGraceFollowsPodUID(t *testing.T) {
	f := newHealthyCluster(t)
	set(f.controllers["default/deployment/web"], 1.0, "status", "readyReplicas")
	pending := func(uid string) map[string]any {
		return decode(t, `{"metadata": {"namespace": "default", "name": "web-3", "uid": "`+uid+`", "labels": {"app": "web"},
		  "ownerReferences": [{"kind": "ReplicaSet", "name": "web-5d"}]}, "spec": {},
		  "status": {"phase": "Pending", "conditions": [{"type": "PodScheduled", "status": "False", "reason": "Unschedulable", "message": "no room"}]}}`)
	}
	polls := 0
	f.respond = func(c Command, line string) (Result, bool) {
		if line != "kubectl -n default get pods -o json" {
			return Result{}, false
		}
		polls++
		uid := "first"
		if polls > 55 {
			uid = "second"
		}
		return list([]map[string]any{f.pods[1], pending(uid)}), true
	}
	o, _ := newOperation(f, options(t, "reboot", "--settle-timeout", "15m", "server-a"))
	err := o.waitForControllerReadyFloor(workload{Namespace: "default", Kind: "deployment", Name: "web"}, 2)
	expectFailure(t, err, "the scheduler cannot place 1")
	if polls < 55+60 {
		t.Fatalf("failed after %d polls; the recreated Pod did not start its grace again", polls)
	}
}

// The replacement needs the larger of the running Pod's requests, which
// carry what admission added, and its own ReplicaSet's template, which an
// in-place resize does not change and a paused rollout keeps; and a Pod
// that tolerates the cordon may land back on the target, so it need not
// fit.
func TestDisplacedPodsFollowTheTemplate(t *testing.T) {
	// Resized down in place to 500m; the ReplicaSet asks 2 CPUs.
	f := newFullCluster(t)
	requests(f.pods[0], "500m", "64Mi")
	runFailure(t, f, "reboot", []string{"--check-only"}, "default/web-1\tcpu=2000m memory=1024Mi\tno remaining node has room")

	// A paused rollout: the Deployment asks 500m, its ReplicaSet 2 CPUs.
	f = newFullCluster(t)
	requests(f.pods[0], "500m", "64Mi")
	templateRequests(f, "default/deployment/web", "500m", "64Mi")
	runFailure(t, f, "reboot", []string{"--check-only"}, "default/web-1\tcpu=2000m memory=1024Mi\tno remaining node has room")

	// Resized down, with RuntimeClass overhead admission added to the Pod:
	// the template's 2 CPUs plus the overhead.
	f = newFullCluster(t)
	requests(f.pods[0], "500m", "64Mi")
	set(f.pods[0], map[string]any{"cpu": "100m"}, "spec", "overhead")
	runFailure(t, f, "reboot", []string{"--check-only"}, "default/web-1\tcpu=2100m memory=1024Mi\tno remaining node has room")

	// A LimitRange default: the templates ask nothing, the Pod 2 CPUs.
	f = newFullCluster(t)
	templateRequests(f, "default/replicaset/web-5d", "0", "0")
	templateRequests(f, "default/deployment/web", "0", "0")
	runFailure(t, f, "reboot", []string{"--check-only"}, "default/web-1\tcpu=2000m memory=1024Mi\tno remaining node has room")

	f = newFullCluster(t)
	set(f.controllers["default/deployment/web"], []any{map[string]any{"operator": "Exists"}}, "spec", "template", "spec", "tolerations")
	output := run(t, f, "reboot", "--check-only")
	mustContain(t, output, "needed-pods\t0\tunplaced\t0\tunsettled\t0\tother-pods\t0\tunplaced\t0\n")
}

// A StatefulSet Pod below a rolling update's partition comes back at the
// current revision, not the latest template.
func TestDisplacedStatefulSetPodFollowsItsRevision(t *testing.T) {
	f := newFullCluster(t)
	webRequests(f, "100m", "64Mi")
	f.pods = append(f.pods, requests(readyPod(t, "default", "kv-0", "server-a", "StatefulSet", "kv", `{"app": "kv"}`), "500m", "64Mi"))
	f.controllers["default/statefulset/kv"] = decode(t, `{"spec": {"replicas": 1, "selector": {"matchLabels": {"app": "kv"}},
	  "template": {"metadata": {"labels": {"app": "kv"}}, "spec": {"containers": [{"resources": {"requests": {"cpu": "500m"}}}]}},
	  "updateStrategy": {"type": "RollingUpdate", "rollingUpdate": {"partition": 1}}},
	  "status": {"readyReplicas": 1, "currentRevision": "kv-old", "updateRevision": "kv-new"}}`)
	f.controllers["default/controllerrevision/kv-old"] = decode(t, `{"data": {"spec": {"template": {"spec":
	  {"containers": [{"resources": {"requests": {"cpu": "2"}}}]}}}}}`)
	runFailure(t, f, "reboot", []string{"--check-only"}, "default/kv-0\tcpu=2000m memory=64Mi\tno remaining node has room")

	// At or above the partition it comes back at the latest template.
	set(f.controllers["default/statefulset/kv"], 0.0, "spec", "updateStrategy", "rollingUpdate", "partition")
	run(t, f, "reboot", "--check-only")
}

// The estimate adds the template's containers the Pod lacks and takes the
// larger Pod-level request; a StatefulSet's partition counts from its
// first ordinal.
func TestReplacementRequestsMergeTheTemplate(t *testing.T) {
	newContainer := func(name, cpu string) container {
		var c container
		c.Name = name
		c.Resources.Requests = map[string]string{"cpu": cpu}
		return c
	}
	f := newHealthyCluster(t)
	o, _ := newOperation(f, options(t, "reboot", "server-a"))
	rs := f.controllers["default/replicaset/web-5d"]
	set(rs, []any{map[string]any{"name": "app", "resources": map[string]any{"requests": map[string]any{"cpu": "500m"}}},
		map[string]any{"name": "extra", "resources": map[string]any{"requests": map[string]any{"cpu": "2"}}}},
		"spec", "template", "spec", "containers")
	var p pod
	p.Metadata.Namespace, p.Metadata.Name = "default", "web-1"
	p.Metadata.OwnerReferences = []ownerReference{{Kind: "ReplicaSet", Name: "web-5d"}}
	p.Spec.Containers = []container{newContainer("app", "500m")}
	if got, err := o.replacementRequests(p); err != nil || got.cpu != 2500 {
		t.Fatalf("template-only container: %v, %v; want 2500m", got, err)
	}

	set(rs, map[string]any{"requests": map[string]any{"cpu": "4"}}, "spec", "template", "spec", "resources")
	p.Spec.Resources.Requests = map[string]string{"cpu": "1"}
	if got, err := o.replacementRequests(p); err != nil || got.cpu != 4000 {
		t.Fatalf("template Pod-level request: %v, %v; want 4000m", got, err)
	}
	p.Spec.Resources.Requests = map[string]string{"cpu": "6"}
	if got, err := o.replacementRequests(p); err != nil || got.cpu != 6000 {
		t.Fatalf("Pod's larger Pod-level request: %v, %v; want 6000m", got, err)
	}

	// kv-10, first ordinal 10, partition 1: the current revision.
	f.controllers["default/statefulset/kv"] = decode(t, `{"spec": {"ordinals": {"start": 10},
	  "template": {"spec": {"containers": [{"name": "app", "resources": {"requests": {"cpu": "500m"}}}]}},
	  "updateStrategy": {"type": "RollingUpdate", "rollingUpdate": {"partition": 1}}},
	  "status": {"currentRevision": "kv-old", "updateRevision": "kv-new"}}`)
	f.controllers["default/controllerrevision/kv-old"] = decode(t, `{"data": {"spec": {"template": {"spec":
	  {"containers": [{"name": "app", "resources": {"requests": {"cpu": "2"}}}]}}}}}`)
	var kv pod
	kv.Metadata.Namespace, kv.Metadata.Name = "default", "kv-10"
	kv.Metadata.OwnerReferences = []ownerReference{{Kind: "StatefulSet", Name: "kv"}}
	kv.Spec.Containers = []container{newContainer("app", "500m")}
	if got, err := o.replacementRequests(kv); err != nil || got.cpu != 2000 {
		t.Fatalf("start ordinal: %v, %v; want 2000m", got, err)
	}
}

// The template's view counts a template that drops the Pod's Pod-level
// request for larger containers, and a sidecar the template starts before
// an init container.
func TestReplacementRequestsTemplateView(t *testing.T) {
	f := newHealthyCluster(t)
	o, _ := newOperation(f, options(t, "reboot", "server-a"))
	rs := f.controllers["default/replicaset/web-5d"]
	var p pod
	p.Metadata.Namespace, p.Metadata.Name = "default", "web-1"
	p.Metadata.OwnerReferences = []ownerReference{{Kind: "ReplicaSet", Name: "web-5d"}}
	var app container
	app.Name = "app"
	app.Resources.Requests = map[string]string{"cpu": "500m"}
	p.Spec.Containers = []container{app}

	set(rs, []any{map[string]any{"name": "app", "resources": map[string]any{"requests": map[string]any{"cpu": "4"}}}},
		"spec", "template", "spec", "containers")
	p.Spec.Resources.Requests = map[string]string{"cpu": "1"}
	if got, err := o.replacementRequests(p); err != nil || got.cpu != 4000 {
		t.Fatalf("dropped Pod-level request: %v, %v; want 4000m", got, err)
	}
	// The Pod's overhead counts in the template's view too.
	p.Spec.Overhead = map[string]string{"cpu": "100m"}
	if got, err := o.replacementRequests(p); err != nil || got.cpu != 4100 {
		t.Fatalf("template view with overhead: %v, %v; want 4100m", got, err)
	}
	p.Spec.Overhead = nil

	p.Spec.Resources.Requests = nil
	var initC container
	initC.Name = "init"
	initC.Resources.Requests = map[string]string{"cpu": "2"}
	p.Spec.InitContainers = []container{initC}
	set(rs, []any{map[string]any{"name": "app", "resources": map[string]any{"requests": map[string]any{"cpu": "500m"}}}},
		"spec", "template", "spec", "containers")
	set(rs, []any{
		map[string]any{"name": "sidecar", "restartPolicy": "Always", "resources": map[string]any{"requests": map[string]any{"cpu": "1"}}},
		map[string]any{"name": "init", "resources": map[string]any{"requests": map[string]any{"cpu": "2"}}},
	}, "spec", "template", "spec", "initContainers")
	if got, err := o.replacementRequests(p); err != nil || got.cpu != 3000 {
		t.Fatalf("template init order: %v, %v; want 3000m", got, err)
	}
}

// A Pod-level in-place resize holds the Pod's allocation plus overhead.
func TestResidentRequestsPodLevelResize(t *testing.T) {
	var p pod
	p.Spec.Resources.Requests = map[string]string{"cpu": "1"}
	p.Spec.Overhead = map[string]string{"cpu": "100m"}
	p.Status.AllocatedResources = map[string]string{"cpu": "3"}
	if got, err := residentRequests(p); err != nil || got.cpu != 3100 {
		t.Fatalf("residentRequests = %v, %v; want 3100m", got, err)
	}
}

// Of a controller's replicas on the target, the smallest are the needed
// ones, since any of them meets the gate; an ordered StatefulSet recreates
// its lowest ordinals first, so those are.
func TestNeededReplicasAreTheSmallest(t *testing.T) {
	cluster := func(policy string) *fakeCluster {
		f := newFullCluster(t)
		webRequests(f, "100m", "64Mi")
		f.pods = append(f.pods,
			requests(readyPod(t, "default", "kv-0", "server-a", "StatefulSet", "kv", `{"app": "kv"}`), "2", "64Mi"),
			requests(readyPod(t, "default", "kv-1", "server-a", "StatefulSet", "kv", `{"app": "kv"}`), "500m", "64Mi"),
			requests(readyPod(t, "default", "kv-2", "server-b", "StatefulSet", "kv", `{"app": "kv"}`), "0", "0"),
		)
		f.controllers["default/statefulset/kv"] = decode(t, `{"spec": {"replicas": 3, "podManagementPolicy": "`+policy+`",
		  "selector": {"matchLabels": {"app": "kv"}}, "template": {"metadata": {"labels": {"app": "kv"}}, "spec": {}}},
		  "status": {"readyReplicas": 3}}`)
		return f
	}
	output := run(t, cluster("Parallel"), "reboot", "--check-only")
	mustContain(t, output, "needed-pods\t2\tunplaced\t0\tunsettled\t0\tother-pods\t1\tunplaced\t1\n")
	mustContain(t, output, "default/kv-0\tcpu=2000m memory=64Mi\tno remaining node has room\n")

	runFailure(t, cluster("OrderedReady"), "reboot", []string{"--check-only"}, "default/kv-0\tcpu=2000m memory=64Mi\tno remaining node has room")
}

// The union estimate runs every sidecar of the Pod and the template beside
// the largest init container, whatever their order.
func TestReplacementRequestsUnionOfSidecars(t *testing.T) {
	f := newHealthyCluster(t)
	o, _ := newOperation(f, options(t, "reboot", "server-a"))
	newContainer := func(name, cpu, restart string) container {
		var c container
		c.Name, c.RestartPolicy = name, restart
		c.Resources.Requests = map[string]string{"cpu": cpu}
		return c
	}
	var p pod
	p.Metadata.Namespace, p.Metadata.Name = "default", "web-1"
	p.Metadata.OwnerReferences = []ownerReference{{Kind: "ReplicaSet", Name: "web-5d"}}
	p.Spec.Containers = []container{newContainer("app", "500m", "")}
	p.Spec.InitContainers = []container{newContainer("proxy", "1", "Always"), newContainer("setup", "2", "")}
	set(f.controllers["default/replicaset/web-5d"], map[string]any{
		"containers": []any{map[string]any{"name": "app", "resources": map[string]any{"requests": map[string]any{"cpu": "500m"}}}},
		"initContainers": []any{
			map[string]any{"name": "log", "restartPolicy": "Always", "resources": map[string]any{"requests": map[string]any{"cpu": "1"}}},
			map[string]any{"name": "setup", "resources": map[string]any{"requests": map[string]any{"cpu": "2"}}},
		},
	}, "spec", "template", "spec")
	if got, err := o.replacementRequests(p); err != nil || got.cpu != 4000 {
		t.Fatalf("replacementRequests = %v, %v; want 4000m", got, err)
	}
}

// An infeasible Pod-level resize holds only its allocation.
func TestResidentRequestsInfeasiblePodLevelResize(t *testing.T) {
	var p pod
	p.Spec.Resources.Requests = map[string]string{"cpu": "5"}
	p.Status.AllocatedResources = map[string]string{"cpu": "1"}
	p.Status.Conditions = []condition{{Type: "PodResizePending", Status: "True", Reason: "Infeasible"}}
	if got, err := residentRequests(p); err != nil || got.cpu != 1000 {
		t.Fatalf("residentRequests = %v, %v; want 1000m", got, err)
	}
}

// A template's limit stands in for a missing request, as on admission; and
// a sidecar after the largest init container does not run beside it.
func TestReplacementRequestsLimitsAndSidecarOrder(t *testing.T) {
	f := newHealthyCluster(t)
	o, _ := newOperation(f, options(t, "reboot", "server-a"))
	var p pod
	p.Metadata.Namespace, p.Metadata.Name = "default", "web-1"
	p.Metadata.OwnerReferences = []ownerReference{{Kind: "ReplicaSet", Name: "web-5d"}}
	var app container
	app.Name = "app"
	app.Resources.Requests = map[string]string{"cpu": "500m"}
	p.Spec.Containers = []container{app}
	rs := f.controllers["default/replicaset/web-5d"]
	set(rs, map[string]any{"containers": []any{map[string]any{"name": "app", "resources": map[string]any{"limits": map[string]any{"cpu": "4"}}}}},
		"spec", "template", "spec")
	if got, err := o.replacementRequests(p); err != nil || got.cpu != 4000 {
		t.Fatalf("template limit: %v, %v; want 4000m", got, err)
	}

	// setup (2 CPUs) then agent (1 CPU) on both sides: 2 CPUs at most.
	setup := map[string]any{"name": "setup", "resources": map[string]any{"requests": map[string]any{"cpu": "2"}}}
	agent := map[string]any{"name": "agent", "restartPolicy": "Always", "resources": map[string]any{"requests": map[string]any{"cpu": "1"}}}
	set(rs, map[string]any{"containers": []any{map[string]any{"name": "app", "resources": map[string]any{"requests": map[string]any{"cpu": "500m"}}}},
		"initContainers": []any{setup, agent}}, "spec", "template", "spec")
	var s, a container
	s.Name, s.Resources.Requests = "setup", map[string]string{"cpu": "2"}
	a.Name, a.RestartPolicy, a.Resources.Requests = "agent", "Always", map[string]string{"cpu": "1"}
	p.Spec.InitContainers = []container{s, a}
	if got, err := o.replacementRequests(p); err != nil || got.cpu != 2000 {
		t.Fatalf("sidecar after init: %v, %v; want 2000m", got, err)
	}
}

// An infeasible Pod-level resize drops the refused request only for the
// resources the node reports.
func TestResidentRequestsInfeasibleKeepsUnreported(t *testing.T) {
	var p pod
	p.Spec.Resources.Requests = map[string]string{"cpu": "5", "memory": "4Gi"}
	p.Status.AllocatedResources = map[string]string{"cpu": "1"}
	p.Status.Conditions = []condition{{Type: "PodResizePending", Status: "True", Reason: "Infeasible"}}
	if got, err := residentRequests(p); err != nil || got.cpu != 1000 || got.memory != 4<<30 {
		t.Fatalf("residentRequests = %v, %v; want 1000m and 4Gi", got, err)
	}
}

// Of a controller's replicas, one a resident's anti-affinity bars from
// every remaining node is not chosen as needed while another may run.
func TestNeededReplicasPreferThoseThatMayRun(t *testing.T) {
	f := newFullCluster(t)
	webRequests(f, "100m", "64Mi")
	f.pods = append(f.pods,
		requests(readyPod(t, "default", "kv-0", "server-a", "StatefulSet", "kv", `{"app": "kv", "zone": "a"}`), "100m", "64Mi"),
		requests(readyPod(t, "default", "kv-1", "server-a", "StatefulSet", "kv", `{"app": "kv"}`), "500m", "64Mi"),
		requests(readyPod(t, "default", "kv-2", "server-b", "StatefulSet", "kv", `{"app": "kv"}`), "0", "0"),
	)
	// A guard on each remaining node repels zone=a.
	for _, n := range []string{"server-b", "server-c"} {
		g := readyPod(t, "default", "guard-"+n, n, "", "", `{}`)
		set(g, decode(t, `{"podAntiAffinity": {"requiredDuringSchedulingIgnoredDuringExecution": [
		  {"topologyKey": "kubernetes.io/hostname", "labelSelector": {"matchLabels": {"zone": "a"}}}]}}`), "spec", "affinity")
		f.pods = append(f.pods, g)
	}
	f.controllers["default/statefulset/kv"] = decode(t, `{"spec": {"replicas": 3, "podManagementPolicy": "Parallel",
	  "selector": {"matchLabels": {"app": "kv"}}, "template": {"metadata": {"labels": {"app": "kv"}}, "spec": {}}},
	  "status": {"readyReplicas": 3}}`)
	output := run(t, f, "reboot", "--check-only")
	mustContain(t, output, "needed-pods\t2\tunplaced\t0\tunsettled\t0\tother-pods\t1\tunplaced\t0\n")
}

// An init container only the template has runs beside all the Pod's
// sidecars, whose place for it is unknown; and a template's Pod-level
// limit stands in for a Pod-level request no container makes.
func TestReplacementRequestsOneSidedInitAndPodLimit(t *testing.T) {
	f := newHealthyCluster(t)
	o, _ := newOperation(f, options(t, "reboot", "server-a"))
	var p pod
	p.Metadata.Namespace, p.Metadata.Name = "default", "web-1"
	p.Metadata.OwnerReferences = []ownerReference{{Kind: "ReplicaSet", Name: "web-5d"}}
	var app, proxy container
	app.Name, app.Resources.Requests = "app", map[string]string{"cpu": "500m"}
	proxy.Name, proxy.RestartPolicy, proxy.Resources.Requests = "istio-proxy", "Always", map[string]string{"cpu": "1"}
	p.Spec.Containers = []container{app}
	p.Spec.InitContainers = []container{proxy}
	rs := f.controllers["default/replicaset/web-5d"]
	set(rs, map[string]any{
		"containers":     []any{map[string]any{"name": "app", "resources": map[string]any{"requests": map[string]any{"cpu": "500m"}}}},
		"initContainers": []any{map[string]any{"name": "migrate", "resources": map[string]any{"requests": map[string]any{"cpu": "2"}}}},
	}, "spec", "template", "spec")
	if got, err := o.replacementRequests(p); err != nil || got.cpu != 3000 {
		t.Fatalf("template-only init: %v, %v; want 3000m", got, err)
	}

	p.Spec.InitContainers = nil
	set(rs, map[string]any{
		"containers": []any{map[string]any{"name": "app"}},
		"resources":  map[string]any{"limits": map[string]any{"cpu": "4"}},
	}, "spec", "template", "spec")
	if got, err := o.replacementRequests(p); err != nil || got.cpu != 4000 {
		t.Fatalf("Pod-level limit: %v, %v; want 4000m", got, err)
	}
	// A LimitRange default for the container comes after it.
	f.limitRanges = []map[string]any{decode(t, `{"spec": {"limits": [{"type": "Container", "defaultRequest": {"cpu": "100m"}}]}}`)}
	o, _ = newOperation(f, options(t, "reboot", "server-a"))
	if got, err := o.replacementRequests(p); err != nil || got.cpu != 4000 {
		t.Fatalf("Pod-level limit with a LimitRange: %v, %v; want 4000m", got, err)
	}
}

// A template container without requests takes the namespace's LimitRange
// defaults, as the replacement will on admission: a default request, or a
// default limit standing in for it.
func TestReplacementRequestsLimitRangeDefaults(t *testing.T) {
	f := newHealthyCluster(t)
	o, _ := newOperation(f, options(t, "reboot", "server-a"))
	var p pod
	p.Metadata.Namespace, p.Metadata.Name = "default", "web-1"
	p.Metadata.OwnerReferences = []ownerReference{{Kind: "ReplicaSet", Name: "web-5d"}}
	var app container
	app.Name, app.Resources.Requests = "app", map[string]string{"cpu": "500m"}
	p.Spec.Containers = []container{app}
	set(f.controllers["default/replicaset/web-5d"], map[string]any{"containers": []any{
		map[string]any{"name": "app", "resources": map[string]any{"requests": map[string]any{"cpu": "500m"}}},
		map[string]any{"name": "extra"},
	}}, "spec", "template", "spec")
	f.limitRanges = []map[string]any{decode(t, `{"spec": {"limits": [{"type": "Container", "defaultRequest": {"cpu": "2"}}]}}`)}
	if got, err := o.replacementRequests(p); err != nil || got.cpu != 2500 {
		t.Fatalf("default request: %v, %v; want 2500m", got, err)
	}
	// The LimitRanges are read once per namespace and run.
	f.limitRanges = []map[string]any{decode(t, `{"spec": {"limits": [{"type": "Container", "default": {"cpu": "3"}}]}}`)}
	if got, err := o.replacementRequests(p); err != nil || got.cpu != 2500 {
		t.Fatalf("cached: %v, %v; want 2500m", got, err)
	}
	o, _ = newOperation(f, options(t, "reboot", "server-a"))
	if got, err := o.replacementRequests(p); err != nil || got.cpu != 3500 {
		t.Fatalf("default limit: %v, %v; want 3500m", got, err)
	}

	// The container's own limit comes before a default request, and of
	// several LimitRanges the largest default counts.
	f.limitRanges = []map[string]any{
		decode(t, `{"spec": {"limits": [{"type": "Container", "defaultRequest": {"cpu": "100m"}}]}}`),
		decode(t, `{"spec": {"limits": [{"type": "Container", "defaultRequest": {"cpu": "1"}}]}}`),
	}
	set(f.controllers["default/replicaset/web-5d"], map[string]any{"containers": []any{
		map[string]any{"name": "app", "resources": map[string]any{"limits": map[string]any{"cpu": "4"}}},
		map[string]any{"name": "extra"},
	}}, "spec", "template", "spec")
	o, _ = newOperation(f, options(t, "reboot", "server-a"))
	if got, err := o.replacementRequests(p); err != nil || got.cpu != 5000 {
		t.Fatalf("own limit and largest default: %v, %v; want 5000m", got, err)
	}

	// Unreadable LimitRanges leave a defaulted size unknown.
	f.respond = func(c Command, line string) (Result, bool) {
		if strings.Contains(line, "get limitranges") {
			return Result{Status: 1, Stderr: "forbidden\n"}, true
		}
		return Result{}, false
	}
	o, _ = newOperation(f, options(t, "reboot", "server-a"))
	if _, err := o.replacementRequests(p); err == nil || !strings.Contains(err.Error(), "cannot read the LimitRanges") {
		t.Fatalf("unreadable LimitRanges: %v", err)
	}
}

// An infeasible container resize holds what the node reports, and the
// request for a resource it does not report.
func TestResidentRequestsInfeasibleContainerKeepsUnreported(t *testing.T) {
	var p pod
	var c container
	c.Name, c.Resources.Requests = "a", map[string]string{"cpu": "5", "memory": "4Gi"}
	p.Spec.Containers = []container{c}
	var s containerStatus
	s.Name, s.AllocatedResources = "a", map[string]string{"cpu": "1"}
	p.Status.ContainerStatuses = []containerStatus{s}
	p.Status.Conditions = []condition{{Type: "PodResizePending", Status: "True", Reason: "Infeasible"}}
	if got, err := residentRequests(p); err != nil || got.cpu != 1000 || got.memory != 4<<30 {
		t.Fatalf("residentRequests = %v, %v; want 1000m and 4Gi", got, err)
	}
}

// A recreating controller or revision that cannot be read leaves the size
// unknown, and the check fails rather than guess from the running Pod.
func TestReplacementRequestsNeedTheRecreatingTemplate(t *testing.T) {
	f := newHealthyCluster(t)
	f.controllers["default/statefulset/kv"] = decode(t, `{"spec": {"template": {"spec": {}},
	  "updateStrategy": {"type": "RollingUpdate", "rollingUpdate": {"partition": 1}}},
	  "status": {"currentRevision": "kv-old", "updateRevision": "kv-new"}}`)
	var kv pod
	kv.Metadata.Namespace, kv.Metadata.Name = "default", "kv-0"
	kv.Metadata.OwnerReferences = []ownerReference{{Kind: "StatefulSet", Name: "kv"}}
	o, _ := newOperation(f, options(t, "reboot", "server-a"))
	if _, err := o.replacementRequests(kv); err == nil || !strings.Contains(err.Error(), "cannot read the revision kv-old") {
		t.Fatalf("unreadable revision: %v", err)
	}
	var web pod
	web.Metadata.Namespace, web.Metadata.Name = "default", "web-9"
	web.Metadata.OwnerReferences = []ownerReference{{Kind: "ReplicaSet", Name: "gone"}}
	if _, err := o.replacementRequests(web); err == nil || !strings.Contains(err.Error(), "cannot read the ReplicaSet") {
		t.Fatalf("unreadable ReplicaSet: %v", err)
	}
}
