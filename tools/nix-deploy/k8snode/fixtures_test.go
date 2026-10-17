package k8snode

import (
	"bytes"
	"fmt"
	"testing"
	"time"
)

// serverNode is a Ready, schedulable stable control-plane Node.
func serverNode(t *testing.T, name, address string) map[string]any {
	return decode(t, fmt.Sprintf(`{
	  "metadata": {
	    "name": %[1]q,
	    "labels": {
	      "kubernetes.io/hostname": %[1]q,
	      "workload-class": "stable",
	      "node-role.kubernetes.io/control-plane": "true"
	    },
	    "annotations": {"flannel.alpha.coreos.com/public-ip": %[2]q}
	  },
	  "spec": {},
	  "status": {
	    "conditions": [{"type": "Ready", "status": "True"}],
	    "addresses": [{"type": "InternalIP", "address": %[2]q}],
	    "allocatable": {"pods": "110", "cpu": "4", "memory": "16Gi"}
	  }
	}`, name, address))
}

// readyPod is a running Pod with every container Ready.
func readyPod(t *testing.T, namespace, name, node, ownerKind, ownerName string, labels string) map[string]any {
	owners := "[]"
	if ownerKind != "" {
		owners = fmt.Sprintf(`[{"kind": %q, "name": %q}]`, ownerKind, ownerName)
	}
	return decode(t, fmt.Sprintf(`{
	  "metadata": {"namespace": %q, "name": %q, "labels": %s, "ownerReferences": %s},
	  "spec": {"nodeName": %q},
	  "status": {"phase": "Running", "conditions": [{"type": "Ready", "status": "True"}],
	    "containerStatuses": [{"ready": true, "state": {"running": {}}}]}
	}`, namespace, name, labels, owners, node))
}

// newHealthyCluster is three Ready stable servers, server-a the target,
// with a two-replica Deployment, a DaemonSet Pod, the Longhorn
// instance-manager, one healthy three-replica volume attached to server-b
// and Longhorn rebuild admission closed. The target has no self-fence agent;
// withAgent adds one.
func newHealthyCluster(t *testing.T) *fakeCluster {
	f := &fakeCluster{
		t:     t,
		clock: time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC),
		nodes: []map[string]any{
			serverNode(t, "server-a", "192.0.2.1"),
			serverNode(t, "server-b", "192.0.2.2"),
			serverNode(t, "server-c", "192.0.2.3"),
		},
		pods: []map[string]any{
			readyPod(t, "default", "web-1", "server-a", "ReplicaSet", "web-5d", `{"app": "web"}`),
			readyPod(t, "default", "web-2", "server-b", "ReplicaSet", "web-5d", `{"app": "web"}`),
			readyPod(t, "kube-system", "agent-a", "server-a", "DaemonSet", "agent", `{"app": "agent"}`),
			readyPod(t, "longhorn-system", "instance-manager-a", "server-a", "InstanceManager", "instance-manager-a",
				`{"longhorn.io/component": "instance-manager"}`),
		},
		controllers: map[string]map[string]any{
			"default/replicaset/web-5d": decode(t, `{
			  "metadata": {"name": "web-5d", "ownerReferences": [{"kind": "Deployment", "name": "web"}]},
			  "spec": {"replicas": 2, "selector": {"matchLabels": {"app": "web", "pod-template-hash": "5d"}}}
			}`),
			"default/deployment/web": decode(t, `{
			  "metadata": {"name": "web"},
			  "spec": {"replicas": 2, "selector": {"matchLabels": {"app": "web"}},
			    "template": {"metadata": {"labels": {"app": "web"}}, "spec": {}}},
			  "status": {"readyReplicas": 2}
			}`),
		},
		longhorn: true,
		volumes: []map[string]any{decode(t, `{
		  "metadata": {"name": "pvc-1"},
		  "spec": {"numberOfReplicas": 3, "nodeID": "server-b"},
		  "status": {"state": "attached", "robustness": "healthy", "currentNodeID": "server-b",
		    "kubernetesStatus": {"namespace": "default", "pvcName": "data"}}
		}`)},
		replicas: []map[string]any{
			decode(t, `{"spec": {"volumeName": "pvc-1", "nodeID": "server-a"}, "status": {"currentState": "running"}}`),
			decode(t, `{"spec": {"volumeName": "pvc-1", "nodeID": "server-b"}, "status": {"currentState": "running"}}`),
			decode(t, `{"spec": {"volumeName": "pvc-1", "nodeID": "server-c"}, "status": {"currentState": "running"}}`),
		},
		settings: map[string]string{
			rebuildLimitSetting:                   "0",
			"replica-replenishment-wait-interval": "600",
		},
		backup: decode(t, `{"status": {"available": true}}`),
		pdbs: []map[string]any{decode(t, `{
		  "metadata": {"namespace": "default", "name": "web"},
		  "spec": {"selector": {"matchLabels": {"app": "web"}}},
		  "status": {"disruptionsAllowed": 1}
		}`)},
		baselineManager: baselineManager,
		bootID:          "boot-1",
	}
	return f
}

// withAgent enables an enforce-mode self-fence agent on every server, with
// a heartbeat before the run and an agent that publishes its stop and
// returns with the host.
func withAgent(t *testing.T, f *fakeCluster) *fakeCluster {
	set(f.targetNode(), "enforce", "metadata", "annotations", agentModeAnnotation)
	set(f.targetNode(), []any{map[string]any{"manager": agentManager, "time": "2026-01-01T00:03:00Z"}}, "metadata", "managedFields")
	f.status = decode(t, `{"node": "server-a", "servers": 3, "quorum": 2, "states": [
	  {"address": "192.0.2.1", "self": true, "ok": true, "node": "server-a", "mode": "enforce",
	   "classification": "healthy", "fenced": false, "since_unfence": null, "refence_window": 900},
	  {"address": "192.0.2.2", "self": false, "ok": true, "node": "server-b", "mode": "enforce",
	   "classification": "healthy", "fenced": false, "since_unfence": null, "refence_window": 900},
	  {"address": "192.0.2.3", "self": false, "ok": true, "node": "server-c", "mode": "enforce",
	   "classification": "healthy", "fenced": false, "since_unfence": null, "refence_window": 900}
	]}`)
	f.agentPublishes = true
	return f
}

// newFenceCluster is the fixture of the Bash fence-gate checks: an
// already cordoned, enforce-mode server-a with a heartbeat at 00:03, an
// observe-mode peer server-b and an agent node, without Longhorn or
// CloudNativePG.
func newFenceCluster(t *testing.T) *fakeCluster {
	target := decode(t, `{
	  "metadata": {"name": "server-a",
	    "labels": {"node-role.kubernetes.io/control-plane": "true"},
	    "annotations": {"fence.alc.xyz/agent-mode": "enforce", "flannel.alpha.coreos.com/public-ip": "192.0.2.1"},
	    "managedFields": [{"manager": "node-self-fence", "time": "2026-01-01T00:03:00Z"}]},
	  "spec": {"unschedulable": true},
	  "status": {"conditions": [{"type": "Ready", "status": "True", "lastTransitionTime": "2026-01-01T00:02:00Z"}],
	    "addresses": [{"type": "InternalIP", "address": "192.0.2.1"}]}
	}`)
	peer := decode(t, `{
	  "metadata": {"name": "server-b", "labels": {"node-role.kubernetes.io/control-plane": "true"}},
	  "spec": {},
	  "status": {"conditions": [{"type": "Ready", "status": "True"}], "addresses": [{"type": "InternalIP", "address": "192.0.2.2"}]}
	}`)
	agent := decode(t, `{
	  "metadata": {"name": "agent-c"},
	  "spec": {},
	  "status": {"conditions": [{"type": "Ready", "status": "True"}], "addresses": [{"type": "InternalIP", "address": "192.0.2.3"}]}
	}`)
	return &fakeCluster{
		t:     t,
		clock: time.Date(2026, 1, 1, 0, 10, 0, 0, time.UTC),
		nodes: []map[string]any{target, peer, agent},
		status: decode(t, `{"node":"server-a","servers":2,"quorum":2,"states":[
		  {"address":"127.0.0.1","self":true,"ok":true,"node":"server-a","mode":"enforce","classification":"healthy","fenced":false,"since_unfence":null,"refence_window":900},
		  {"address":"192.0.2.2","self":false,"ok":true,"node":"server-b","mode":"observe","classification":"healthy","fenced":false,"since_unfence":900,"refence_window":900}]}`),
		baselineManager: baselineManager,
		baselineTime:    "2026-01-01T00:04:00Z",
		bootID:          "boot-1",
	}
}

// fenceOperation runs against the fence fixture with fence timeouts of zero,
// so every wait checks once.
func fenceOperation(t *testing.T, f *fakeCluster, action string) (*Operation, *bytes.Buffer) {
	opts := options(t, action, "--ssh-target", "server-admin@maintenance-target", "--fence-timeout", "0s", "server-a")
	o, output := newOperation(f, opts)
	o.fenceStopTimeout = 0
	return o, output
}

func unix(t *testing.T, text string) int64 {
	parsed, err := time.Parse(timeLayout, text)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.Unix()
}
