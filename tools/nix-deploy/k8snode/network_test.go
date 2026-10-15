package k8snode

import (
	"strings"
	"testing"
	"time"
)

// These tests port test-k8s-node-reboot-network-audits.sh.

// networkCluster is the Bash fixture: nex, the target, reached through
// admin@nex-maintenance, and its peers nux and xev.
func networkCluster(t *testing.T) *fakeCluster {
	return &fakeCluster{
		t:     t,
		clock: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		nodes: []map[string]any{
			decode(t, `{"metadata":{"name":"nex","annotations":{"flannel.alpha.coreos.com/public-ip":"192.0.2.16"}},"status":{"addresses":[{"type":"InternalIP","address":"192.0.2.16"}],"conditions":[{"type":"Ready","status":"True"}]}}`),
			decode(t, `{"metadata":{"name":"nux"},"status":{"addresses":[{"type":"InternalIP","address":"192.0.2.15"}],"conditions":[{"type":"Ready","status":"True"}]}}`),
			decode(t, `{"metadata":{"name":"xev"},"status":{"addresses":[{"type":"InternalIP","address":"192.0.2.13"}],"conditions":[{"type":"Ready","status":"True"}]}}`),
		},
	}
}

func networkOperation(t *testing.T, f *fakeCluster, args ...string) *Operation {
	args = append(args, "--ssh-target", "admin@nex-maintenance", "nex")
	o, _ := newOperation(f, options(t, "reboot", args...))
	return o
}

func TestNetworkAuditFanout(t *testing.T) {
	f := networkCluster(t)
	o := networkOperation(t, f)
	if err := o.verifyAllNodeNetworkPaths(""); err != nil {
		t.Fatal(err)
	}
	want := "nex\t192.0.2.16\tadmin@nex-maintenance|nux\t192.0.2.15\troot@nux|xev\t192.0.2.13\troot@xev"
	if got := strings.Join(f.audits, "|"); got != want {
		t.Fatalf("audits %q, want %q", got, want)
	}

	// The target is not included in survivor audits.
	f.audits = nil
	if err := o.verifySurvivorNodeNetworkPaths(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.audits, "|"); got != "nux\t192.0.2.15\troot@nux|xev\t192.0.2.13\troot@xev" {
		t.Fatalf("survivor audits %q", got)
	}
}

// Node Ready can precede Flannel startup. Wait for the interface, then
// retain the strict audit; neither timeout nor audit failure may advance
// recovery.
func TestReturnedNodeNetworkWaitsForFlannel(t *testing.T) {
	f := networkCluster(t)
	f.flannelDown = 2
	o := networkOperation(t, f, "--ready-timeout", "30s")
	if err := o.waitForReturnedNodeNetwork(); err != nil {
		t.Fatal(err)
	}
	probes := 0
	for _, c := range f.calls {
		if strings.Contains(c.line, "ip link show dev flannel.1") {
			probes++
		}
	}
	// One strict audit covers every node.
	if probes != 3 || f.sleeps != 2 || len(f.audits) != 3 {
		t.Fatalf("probes=%d sleeps=%d audits=%d", probes, f.sleeps, len(f.audits))
	}
}

func TestReturnedNodeNetworkTimeoutLeavesNodeCordoned(t *testing.T) {
	f := networkCluster(t)
	f.flannelDown = 1000
	o := networkOperation(t, f, "--ready-timeout", "0s")
	expectFailure(t, o.waitForReturnedNodeNetwork(), "node remains cordoned")
	if len(f.audits) != 0 {
		t.Fatal("audit ran without a Flannel interface")
	}
}

func TestReturnedNodeNetworkAuditFailureStops(t *testing.T) {
	f := networkCluster(t)
	f.auditExit = 1
	o := networkOperation(t, f)
	expectFailure(t, o.waitForReturnedNodeNetwork(), "nex Kubernetes network path audit failed")
}

// The returned node's Flannel public IP must match its InternalIP before
// scheduling resumes.
func TestReturnedNodeFlannelMismatch(t *testing.T) {
	f := networkCluster(t)
	set(f.nodes[0], "100.64.0.16", "metadata", "annotations", "flannel.alpha.coreos.com/public-ip")
	o := networkOperation(t, f)
	expectFailure(t, o.verifyReturnedNodeNetwork(), "nex Flannel public IP (100.64.0.16) does not match InternalIP (192.0.2.16)")

	del(f.nodes[0], "metadata", "annotations", "flannel.alpha.coreos.com/public-ip")
	expectFailure(t, o.verifyReturnedNodeNetwork(), "Flannel public IP (missing)")

	set(f.nodes[0], []any{}, "status", "addresses")
	expectFailure(t, o.verifyReturnedNodeNetwork(), "nex has no Kubernetes InternalIP")
}

func TestNetworkAuditRejectsUnsafeOrUnreadyNodes(t *testing.T) {
	cases := []struct {
		name string
		edit func(f *fakeCluster)
		want string
	}{
		{"survivor not Ready", func(f *fakeCluster) {
			set(f.nodes[1], "False", "status", "conditions", 0, "status")
		}, "survivor node nux is not Ready"},
		{"no InternalIP", func(f *fakeCluster) {
			set(f.nodes[1], []any{}, "status", "addresses")
		}, "nux has no Kubernetes InternalIP"},
		{"unsafe address", func(f *fakeCluster) {
			set(f.nodes[1], "192.0.2.15;true", "status", "addresses", 0, "address")
		}, "unsafe Kubernetes node address: 192.0.2.15;true"},
		{"unsafe name", func(f *fakeCluster) {
			set(f.nodes[1], "nux;true", "metadata", "name")
		}, "unsafe Kubernetes node name: nux;true"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := networkCluster(t)
			c.edit(f)
			o := networkOperation(t, f)
			expectFailure(t, o.verifyAllNodeNetworkPaths(""), c.want)
		})
	}

	f := networkCluster(t)
	o := networkOperation(t, f)
	o.opts.NetworkAuditScript = ""
	expectFailure(t, o.verifyAllNodeNetworkPaths(""), "network audit helper is unavailable")
}
