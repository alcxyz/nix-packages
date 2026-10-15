package k8snode

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// These tests port test-k8s-node-reboot-workload-phases.sh; case names are
// the Bash check's reasons.

// placementCluster is the Bash fixture: worker-a, the target, is the only
// node labelled browser-worker; worker-b is unlabelled.
func placementCluster(t *testing.T) *fakeCluster {
	return &fakeCluster{
		t:     t,
		clock: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		nodes: []map[string]any{
			decode(t, `{"metadata":{"name":"worker-a","labels":{"kubernetes.io/hostname":"worker-a","browser-worker":"true"}},"spec":{},"status":{"conditions":[{"type":"Ready","status":"True"}]}}`),
			decode(t, `{"metadata":{"name":"worker-b","labels":{"kubernetes.io/hostname":"worker-b"}},"spec":{},"status":{"conditions":[{"type":"Ready","status":"True"}]}}`),
		},
		controllers: map[string]map[string]any{},
	}
}

func placementOperation(t *testing.T, f *fakeCluster) *Operation {
	o, _ := newOperation(f, options(t, "reboot", "--settle-timeout", "1s", "worker-a"))
	return o
}

var fixture = workload{Namespace: "default", Kind: "deployment", Name: "fixture"}

func TestWorkloadRequiresTargetNode(t *testing.T) {
	cases := []struct {
		name     string
		workload string
		pinned   bool
	}{
		{"node selector only the target matches",
			`{"spec": {"template": {"spec": {"nodeSelector": {"browser-worker": "true"}}}}}`, true},
		{"portable workload",
			`{"spec": {"template": {"spec": {}}}}`, false},
		{"required node affinity only the target matches",
			`{"spec": {"template": {"spec": {"affinity": {"nodeAffinity": {
			  "requiredDuringSchedulingIgnoredDuringExecution": {"nodeSelectorTerms": [{
			    "matchExpressions": [{"key": "browser-worker", "operator": "In", "values": ["true"]}]
			  }]}}}}}}}`, true},
		// NotIn admits nodes without the label, as the Kubernetes scheduler
		// does, so the unlabelled worker-b can still host this workload.
		{"NotIn affinity admits a node without the label",
			`{"spec": {"template": {"spec": {"affinity": {"nodeAffinity": {
			  "requiredDuringSchedulingIgnoredDuringExecution": {"nodeSelectorTerms": [{
			    "matchExpressions": [{"key": "browser-worker", "operator": "NotIn", "values": ["false"]}]
			  }]}}}}}}}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := placementCluster(t)
			f.controllers["default/deployment/fixture"] = decode(t, c.workload)
			o := placementOperation(t, f)
			if got, err := o.workloadRequiresTargetNode(fixture); err != nil || got != c.pinned {
				t.Fatalf("pinned = %v (%v), want %v", got, err, c.pinned)
			}
		})
	}
}

const onePerNodeJSON = `{
  "spec": {"replicas": 3, "selector": {"matchLabels": {"app": "edge"}}, "template": {
    "metadata": {"labels": {"app": "edge", "tier": "core"}},
    "spec": {"affinity": {"podAntiAffinity": {
      "requiredDuringSchedulingIgnoredDuringExecution": [{
        "labelSelector": {"matchLabels": {"app": "edge"}},
        "topologyKey": "kubernetes.io/hostname"
      }]
    }}}
  }}
}`

const spreadJSON = `{
  "spec": {"replicas": 2, "selector": {"matchLabels": {"app": "edge"}}, "template": {
    "metadata": {"labels": {"app": "edge"}},
    "spec": {"topologySpreadConstraints": [{
      "maxSkew": 1,
      "topologyKey": "kubernetes.io/hostname",
      "whenUnsatisfiable": "DoNotSchedule",
      "labelSelector": {"matchLabels": {"app": "edge"}}
    }]}
  }}
}`

// antiAffinity is the path to the one-per-node term.
var antiAffinity = []any{"spec", "template", "spec", "affinity", "podAntiAffinity", "requiredDuringSchedulingIgnoredDuringExecution", 0}

// spread is the path to the hostname spread constraint.
var spread = []any{"spec", "template", "spec", "topologySpreadConstraints", 0}

func path(base []any, keys ...any) []any {
	return append(append([]any{}, base...), keys...)
}

// spreadPod is a running ReplicaSet Pod with the app label on the node.
func spreadPod(t *testing.T, node, app string) map[string]any {
	return decode(t, fmt.Sprintf(`{
	  "metadata": {"labels": {"app": %q}, "ownerReferences": [{"kind": "ReplicaSet"}]},
	  "spec": {"nodeName": %q},
	  "status": {"phase": "Running"}
	}`, app, node))
}

func daemonSetPod(t *testing.T, node string, labels map[string]any) map[string]any {
	p := spreadPod(t, node, "edge")
	set(p, labels, "metadata", "labels")
	set(p, []any{map[string]any{"kind": "DaemonSet"}}, "metadata", "ownerReferences")
	return p
}

func extraNode(t *testing.T, name string, spec string) map[string]any {
	return decode(t, fmt.Sprintf(`{"metadata":{"name":%q,"labels":{"kubernetes.io/hostname":%q}},"spec":%s,"status":{"conditions":[{"type":"Ready","status":"True"}]}}`, name, name, spec))
}

func TestDeploymentReadyFloor(t *testing.T) {
	cases := []struct {
		reason string
		want   int
		// setup edits the nodes and Pods of a fresh fixture and returns the
		// Deployment.
		setup func(t *testing.T, f *fakeCluster) map[string]any
	}{
		{"one-per-node Deployment with one remaining node", 1, func(t *testing.T, f *fakeCluster) (w map[string]any) {
			return decode(t, onePerNodeJSON)
		}},
		{"anti-affinity against other Pods", 3, func(t *testing.T, f *fakeCluster) (w map[string]any) {
			w = decode(t, onePerNodeJSON)
			set(w, "other", path(antiAffinity, "labelSelector", "matchLabels", "app")...)
			return w
		}},
		{"non-hostname anti-affinity", 3, func(t *testing.T, f *fakeCluster) (w map[string]any) {
			w = decode(t, onePerNodeJSON)
			set(w, "topology.kubernetes.io/zone", path(antiAffinity, "topologyKey")...)
			return w
		}},
		{"anti-affinity narrowed by mismatchLabelKeys", 3, func(t *testing.T, f *fakeCluster) (w map[string]any) {
			w = decode(t, onePerNodeJSON)
			set(w, []any{"tier"}, path(antiAffinity, "mismatchLabelKeys")...)
			return w
		}},
		{"portable Deployment", 2, func(t *testing.T, f *fakeCluster) (w map[string]any) {
			return decode(t, `{"spec": {"replicas": 2, "template": {"spec": {}}}}`)
		}},
		// Drained worker-a counts as an empty domain, so worker-b keeps its
		// one replica.
		{"hostname spread that keeps the drained node in the skew", 1, func(t *testing.T, f *fakeCluster) (w map[string]any) {
			f.pods = []map[string]any{spreadPod(t, "worker-b", "edge")}
			return decode(t, spreadJSON)
		}},
		// With cordoned nodes dropped from the skew, the replacement fits on
		// worker-b.
		{"hostname spread that drops cordoned nodes from the skew", 2, func(t *testing.T, f *fakeCluster) (w map[string]any) {
			f.pods = []map[string]any{spreadPod(t, "worker-b", "edge")}
			w = decode(t, spreadJSON)
			set(w, "Honor", path(spread, "nodeTaintsPolicy")...)
			return w
		}},
		{"soft hostname spread", 2, func(t *testing.T, f *fakeCluster) (w map[string]any) {
			f.pods = []map[string]any{spreadPod(t, "worker-b", "edge")}
			w = decode(t, spreadJSON)
			set(w, "ScheduleAnyway", path(spread, "whenUnsatisfiable")...)
			return w
		}},
		// A DaemonSet Pod the spread selects stays on worker-a, so the
		// replacement fits.
		{"DaemonSet Pods the spread selects on the drained node", 2, func(t *testing.T, f *fakeCluster) (w map[string]any) {
			b := spreadPod(t, "worker-b", "edge")
			set(b, "web", "metadata", "labels", "component")
			f.pods = []map[string]any{daemonSetPod(t, "worker-a", map[string]any{"app": "edge", "daemon": "yes"}), b}
			w = decode(t, spreadJSON)
			set(w, map[string]any{"app": "edge", "component": "web"}, "spec", "selector", "matchLabels")
			set(w, "web", "spec", "template", "metadata", "labels", "component")
			return w
		}},
		// A DaemonSet Pod sharing the Deployment selector still survives
		// the drain.
		{"DaemonSet Pod with the Deployment labels on the drained node", 2, func(t *testing.T, f *fakeCluster) (w map[string]any) {
			f.pods = []map[string]any{daemonSetPod(t, "worker-a", map[string]any{"app": "edge"}), spreadPod(t, "worker-b", "edge")}
			return decode(t, spreadJSON)
		}},
		// DaemonSet Pods sharing the selector are not Deployment replicas:
		// A=1, B=2 after the drain, so no replacement fits and only B's
		// replica counts.
		{"DaemonSet Pods are not counted as running replicas", 1, func(t *testing.T, f *fakeCluster) (w map[string]any) {
			f.pods = []map[string]any{
				daemonSetPod(t, "worker-a", map[string]any{"app": "edge"}),
				daemonSetPod(t, "worker-b", map[string]any{"app": "edge"}),
				spreadPod(t, "worker-b", "edge"),
			}
			w = decode(t, spreadJSON)
			set(w, 3.0, "spec", "replicas")
			return w
		}},
		// Surviving replicas stacked beyond one per node still count.
		{"replicas already stacked on a surviving node", 2, func(t *testing.T, f *fakeCluster) (w map[string]any) {
			f.pods = []map[string]any{spreadPod(t, "worker-b", "edge"), spreadPod(t, "worker-b", "edge")}
			w = decode(t, spreadJSON)
			set(w, 3.0, "spec", "replicas")
			return w
		}},
		// Another DoNotSchedule constraint the replay does not model keeps
		// every replica.
		{"an additional zone spread", 2, func(t *testing.T, f *fakeCluster) (w map[string]any) {
			f.pods = []map[string]any{spreadPod(t, "worker-b", "edge")}
			w = decode(t, spreadJSON)
			appendTo(w, map[string]any{
				"maxSkew": 1, "topologyKey": "topology.kubernetes.io/zone",
				"whenUnsatisfiable": "DoNotSchedule", "labelSelector": map[string]any{"matchLabels": map[string]any{"app": "edge"}},
			}, "spec", "template", "spec", "topologySpreadConstraints")
			return w
		}},
		// An empty node with an untolerated NoSchedule taint: Ignore keeps
		// it as an empty domain, but nothing can be placed on it.
		{"Ignore spread with an empty untolerated node", 1, func(t *testing.T, f *fakeCluster) (w map[string]any) {
			f.nodes = append(f.nodes, extraNode(t, "worker-d", `{"taints": [{"key": "dedicated", "value": "gpu", "effect": "NoSchedule"}]}`))
			f.pods = []map[string]any{spreadPod(t, "worker-b", "edge")}
			return decode(t, spreadJSON)
		}},
		// Honor drops it from the domains, so the replacement fits on
		// worker-b.
		{"Honor spread drops an untolerated node", 2, func(t *testing.T, f *fakeCluster) (w map[string]any) {
			f.nodes = append(f.nodes, extraNode(t, "worker-d", `{"taints": [{"key": "dedicated", "value": "gpu", "effect": "NoSchedule"}]}`))
			f.pods = []map[string]any{spreadPod(t, "worker-b", "edge")}
			w = decode(t, spreadJSON)
			set(w, "Honor", path(spread, "nodeTaintsPolicy")...)
			return w
		}},
		// Tolerating the taint opens worker-d for the replacement.
		{"tolerated taint opens a node", 2, func(t *testing.T, f *fakeCluster) (w map[string]any) {
			f.nodes = append(f.nodes, extraNode(t, "worker-d", `{"taints": [{"key": "dedicated", "value": "gpu", "effect": "NoSchedule"}]}`))
			f.pods = []map[string]any{spreadPod(t, "worker-b", "edge")}
			w = decode(t, spreadJSON)
			set(w, []any{map[string]any{"key": "dedicated", "operator": "Equal", "value": "gpu", "effect": "NoSchedule"}},
				"spec", "template", "spec", "tolerations")
			return w
		}},
		// No other eligible node still leaves a floor of one Ready replica.
		{"one-per-node Deployment with no eligible remaining node", 1, func(t *testing.T, f *fakeCluster) (w map[string]any) {
			w = decode(t, onePerNodeJSON)
			set(w, map[string]any{"browser-worker": "true"}, "spec", "template", "spec", "nodeSelector")
			return w
		}},
		// A cordoned node still running a Ready replica counts; an empty one
		// does not.
		{"cordoned node without a replica", 1, func(t *testing.T, f *fakeCluster) (w map[string]any) {
			f.nodes = append(f.nodes, extraNode(t, "worker-c", `{"unschedulable": true}`))
			return decode(t, onePerNodeJSON)
		}},
		{"cordoned node still running a Ready replica", 2, func(t *testing.T, f *fakeCluster) (w map[string]any) {
			f.nodes = append(f.nodes, extraNode(t, "worker-c", `{"unschedulable": true}`))
			f.pods = []map[string]any{decode(t, `{"metadata": {"labels": {"app": "edge"}}, "spec": {"nodeName": "worker-c"},
			  "status": {"conditions": [{"type": "Ready", "status": "True"}]}}`)}
			return decode(t, onePerNodeJSON)
		}},
		{"cordoned node running a replica that is not Ready yet", 2, func(t *testing.T, f *fakeCluster) (w map[string]any) {
			f.nodes = append(f.nodes, extraNode(t, "worker-c", `{"unschedulable": true}`))
			f.pods = []map[string]any{decode(t, `{"metadata": {"labels": {"app": "edge"}}, "spec": {"nodeName": "worker-c"},
			  "status": {"phase": "Running", "conditions": [{"type": "Ready", "status": "False"}]}}`)}
			return decode(t, onePerNodeJSON)
		}},
		{"Pods that tolerate the cordon may return to the target node", 3, func(t *testing.T, f *fakeCluster) (w map[string]any) {
			f.nodes = append(f.nodes, extraNode(t, "worker-c", `{"unschedulable": true}`))
			w = decode(t, onePerNodeJSON)
			set(w, []any{map[string]any{"key": "node.kubernetes.io/unschedulable", "operator": "Exists", "effect": "NoSchedule"}},
				"spec", "template", "spec", "tolerations")
			return w
		}},
		{"a toleration whose value differs from the cordon taint", 1, func(t *testing.T, f *fakeCluster) (w map[string]any) {
			f.nodes = append(f.nodes, extraNode(t, "worker-c", `{"unschedulable": true}`))
			w = decode(t, onePerNodeJSON)
			set(w, []any{map[string]any{"key": "node.kubernetes.io/unschedulable", "operator": "Equal", "value": "true", "effect": "NoSchedule"}},
				"spec", "template", "spec", "tolerations")
			return w
		}},
		// A Ready replica still counts on a node whose labels no longer
		// match.
		{"Ready replicas on nodes that no longer match the selector", 2, func(t *testing.T, f *fakeCluster) (w map[string]any) {
			f.nodes = append(f.nodes, extraNode(t, "worker-c", `{"unschedulable": true}`))
			f.pods = []map[string]any{
				decode(t, `{"metadata": {"labels": {"app": "edge"}}, "spec": {"nodeName": "worker-b"}, "status": {"conditions": [{"type": "Ready", "status": "True"}]}}`),
				decode(t, `{"metadata": {"labels": {"app": "edge"}}, "spec": {"nodeName": "worker-c"}, "status": {"conditions": [{"type": "Ready", "status": "True"}]}}`),
			}
			w = decode(t, onePerNodeJSON)
			set(w, map[string]any{"browser-worker": "true"}, "spec", "template", "spec", "nodeSelector")
			return w
		}},
		// Namespace Pod lists larger than one argv string must still be
		// evaluated (the Bash tool passed them to jq as files).
		{"large namespace Pod list", 1, func(t *testing.T, f *fakeCluster) (w map[string]any) {
			f.nodes = append(f.nodes, extraNode(t, "worker-c", `{"unschedulable": true}`))
			for i := range 400 {
				f.pods = append(f.pods, decode(t, fmt.Sprintf(`{"metadata": {"name": "filler-%d", "labels": {"app": "filler"},
				  "annotations": {"note": %q}}, "spec": {"nodeName": "worker-b"},
				  "status": {"conditions": [{"type": "Ready", "status": "True"}]}}`, i, strings.Repeat("x", 512))))
			}
			return decode(t, onePerNodeJSON)
		}},
	}
	for _, c := range cases {
		t.Run(c.reason, func(t *testing.T) {
			f := placementCluster(t)
			w := c.setup(t, f)
			for _, p := range f.pods {
				set(p, "default", "metadata", "namespace")
			}
			f.controllers["default/deployment/fixture"] = w
			o := placementOperation(t, f)
			floor, ok, err := o.deploymentReadyFloor(fixture)
			if err != nil || !ok || floor != c.want {
				t.Fatalf("floor was %d (ok=%v, %v), expected %d", floor, ok, err, c.want)
			}
		})
	}
}

// survivabilityCluster holds the Bash phase fixture's four Deployments:
// pinned (only worker-a matches), portable (one replica), edge (one per
// node, three replicas, two other nodes) and unknown (its floor cannot be
// read).
func survivabilityCluster(t *testing.T, ready int) *fakeCluster {
	f := placementCluster(t)
	f.nodes = append(f.nodes, extraNode(t, "worker-c", `{}`))
	pinned := decode(t, `{"spec": {"replicas": 1, "template": {"spec": {"nodeSelector": {"browser-worker": "true"}}}}}`)
	portable := decode(t, `{"spec": {"replicas": 1, "template": {"spec": {}}}}`)
	edge := decode(t, onePerNodeJSON)
	unknown := decode(t, onePerNodeJSON)
	for _, w := range []map[string]any{pinned, portable, edge, unknown} {
		set(w, float64(ready), "status", "readyReplicas")
	}
	f.controllers["default/deployment/pinned"] = pinned
	f.controllers["default/deployment/portable"] = portable
	f.controllers["default/deployment/edge"] = edge
	f.controllers["batch/deployment/unknown"] = unknown
	// A failed floor lookup keeps every replica.
	f.respond = func(c Command, line string) (Result, bool) {
		if line == "kubectl -n batch get pods -o json" {
			return Result{Status: 1}, true
		}
		return Result{}, false
	}
	return f
}

var (
	pinnedWorkload   = workload{Namespace: "default", Kind: "deployment", Name: "pinned"}
	portableWorkload = workload{Namespace: "default", Kind: "deployment", Name: "portable"}
	edgeWorkload     = workload{Namespace: "default", Kind: "deployment", Name: "edge"}
	unknownWorkload  = workload{Namespace: "batch", Kind: "deployment", Name: "unknown"}
)

// A one-per-node floor is passed through; a failed floor lookup keeps every
// replica; a target-pinned workload is not checked before its node returns.
func TestDisplacedWorkloadSurvivabilityFloors(t *testing.T) {
	cases := []struct {
		workload workload
		want     string
	}{
		{portableWorkload, "default/deployment/portable has 0 Ready replicas; maintenance requires 1"},
		{edgeWorkload, "default/deployment/edge has 0 Ready replicas; maintenance requires 2"},
		{unknownWorkload, "batch/deployment/unknown has 0 Ready replicas; maintenance requires 3"},
	}
	for _, c := range cases {
		t.Run(c.workload.Name, func(t *testing.T) {
			f := survivabilityCluster(t, 0)
			o := placementOperation(t, f)
			o.workloads = []workload{c.workload}
			expectFailure(t, o.waitForDisplacedWorkloadSurvivability(), c.want)
		})
	}

	f := survivabilityCluster(t, 0)
	o, output := newOperation(f, options(t, "reboot", "--settle-timeout", "1s", "worker-a"))
	o.workloads = []workload{pinnedWorkload}
	if err := o.waitForDisplacedWorkloadSurvivability(); err != nil {
		t.Fatalf("target-pinned workload was checked before its node returned: %v", err)
	}
	mustContain(t, output.String(), "deferring target-pinned default/deployment/pinned until worker-a returns")
}

func TestDisplacedWorkloadsDeferThenSettle(t *testing.T) {
	f := survivabilityCluster(t, 3)
	o, output := newOperation(f, options(t, "reboot", "--settle-timeout", "1s", "worker-a"))
	o.workloads = []workload{pinnedWorkload, portableWorkload, edgeWorkload, unknownWorkload}
	if err := o.waitForDisplacedWorkloadSurvivability(); err != nil {
		t.Fatal(err)
	}
	mustContain(t, output.String(), "default/deployment/edge cannot place every replica while worker-a is out; requiring 2 of 3 Ready")

	if err := o.waitForWorkloads(); err != nil {
		t.Fatal(err)
	}
	mustCall(t, f, `^kubectl -n default rollout status deployment/pinned --timeout=1s$`)
	mustCall(t, f, `^kubectl -n default rollout status deployment/portable --timeout=1s$`)
}

func TestWaitForWorkloadsReportsFailures(t *testing.T) {
	f := survivabilityCluster(t, 3)
	f.rolloutExit = 1
	f.controllers["default/replicaset/solo"] = decode(t, `{"spec": {"selector": {"matchLabels": {"b": "2", "a": "1"}}}}`)
	o, output := newOperation(f, options(t, "reboot", "worker-a"))
	o.workloads = []workload{portableWorkload, {Namespace: "default", Kind: "replicaset", Name: "solo"}, {Namespace: "default", Kind: "cluster", Name: "db"}}
	expectFailure(t, o.waitForWorkloads(), "one or more displaced workloads did not settle")
	mustCall(t, f, `^kubectl -n default wait --for=condition=Ready pod --selector=a=1,b=2 --timeout=90m$`)
	mustContain(t, output.String(), "cannot generically wait for default/cluster/db; skipping")
}

func TestCollectDisplacedWorkloads(t *testing.T) {
	f := newHealthyCluster(t)
	f.pods = append(f.pods,
		readyPod(t, "default", "db-0", "server-a", "StatefulSet", "db", `{}`),
		readyPod(t, "default", "job-x", "server-a", "Job", "job", `{}`),
		readyPod(t, "kube-system", "static", "server-a", "Node", "server-a", `{}`),
		readyPod(t, "default", "orphan", "server-a", "ReplicaSet", "orphan-1", `{}`),
		readyPod(t, "default", "pg-1", "server-a", "Cluster", "pg", `{}`),
	)
	o, _ := newOperation(f, options(t, "reboot", "server-a"))
	workloads, err := o.collectDisplacedWorkloads()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, w := range workloads {
		got = append(got, w.String())
	}
	// The instance-manager's owner is collected like any other controller,
	// as in the Bash tool, and gets its dedicated availability gate.
	want := "default/cluster/pg default/deployment/web default/replicaset/orphan-1 default/statefulset/db longhorn-system/instancemanager/instance-manager-a"
	if strings.Join(got, " ") != want {
		t.Fatalf("got %v", got)
	}
}
