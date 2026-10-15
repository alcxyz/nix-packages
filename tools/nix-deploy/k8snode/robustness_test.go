package k8snode

import (
	"context"
	"strings"
	"testing"
)

// An interrupt that kills the probe for SSH dropping is no evidence the
// power action took effect, so the stopped self-fence agent is still
// reported.
func TestSSHDownProbeKilledByInterruptKeepsAgentWarning(t *testing.T) {
	f := withAgent(t, newHealthyCluster(t))
	o, output := newOperation(f, options(t, "reboot", "server-a"))
	ctx, cancel := context.WithCancel(context.Background())
	o.ctx = ctx
	f.respond = func(c Command, line string) (Result, bool) {
		if c.Argv[0] == "ssh" && strings.Contains(line, "ConnectTimeout=2") {
			cancel()
			return Result{Status: 130}, true
		}
		return Result{}, false
	}
	if status := o.Run(); status == 0 {
		t.Fatal("interrupted run passed")
	}
	mustContain(t, output.String(), "node-self-fence is still stopped on server-a")
	// Nothing runs after the killed probe: no return gate, no uncordon.
	if last := f.calls[len(f.calls)-1].line; !strings.Contains(last, "ConnectTimeout=2") {
		t.Fatalf("command after the interrupt: %s", last)
	}
}

// kubectl rejects a bare number of seconds other than 0, so durations reach
// it with a unit.
func TestBareSecondDurationsGainUnit(t *testing.T) {
	opts := options(t, "reboot", "--drain-timeout", "45", "--ready-timeout", "30", "--settle-timeout", "0", "--fence-timeout", "120", "server-a")
	if opts.DrainTimeout != "45s" || opts.ReadyTimeout != "30s" || opts.SettleTimeout != "0s" || opts.FenceTimeout != "120s" {
		t.Fatalf("durations = %q %q %q %q", opts.DrainTimeout, opts.ReadyTimeout, opts.SettleTimeout, opts.FenceTimeout)
	}
	f := newHealthyCluster(t)
	o, output := newOperation(f, opts)
	if status := o.Run(); status != 0 {
		t.Fatalf("run failed:\n%s", output)
	}
	mustCall(t, f, `drain server-a .*--timeout=45s`)
	mustCall(t, f, `wait node/server-a --for=condition=Ready --timeout=30s`)
}

// A zero SSH timeout still probes the host once.
func TestSSHUpZeroTimeoutProbesOnce(t *testing.T) {
	f := newHealthyCluster(t)
	o, _ := newOperation(f, options(t, "on", "--ssh-timeout", "0", "server-a"))
	if err := o.waitForSSHUp(); err != nil {
		t.Fatalf("reachable host failed: %v", err)
	}

	f = newHealthyCluster(t)
	f.hostDown = 5
	o, _ = newOperation(f, options(t, "on", "--ssh-timeout", "0", "server-a"))
	expectFailure(t, o.waitForSSHUp(), "SSH did not return within 0s")
	probes := 0
	for _, c := range f.calls {
		if strings.Contains(c.line, "ConnectTimeout=5") {
			probes++
		}
	}
	if probes != 1 || f.sleeps != 0 {
		t.Fatalf("probes = %d, sleeps = %d; want one probe and no wait", probes, f.sleeps)
	}
}

// Output kubectl returned but that does not decode fails the gate instead of
// taking the fallback meant for an object kubectl cannot read.
func TestUndecodableOptionalReadsFailClosed(t *testing.T) {
	f := newHealthyCluster(t)
	f.respond = func(c Command, line string) (Result, bool) {
		if strings.HasSuffix(line, "get deployment/fixture -o json") {
			return Result{Stdout: `{"spec": {"replicas": "three"}}`}, true
		}
		return Result{}, false
	}
	o, _ := newOperation(f, options(t, "reboot", "server-a"))
	fixture := workload{Namespace: "default", Kind: "deployment", Name: "fixture"}
	if _, err := o.controllerReplicas(fixture); err == nil {
		t.Fatal("undecodable replicas fell back to one")
	}
	if _, _, err := o.deploymentReadyFloor(fixture); err == nil {
		t.Fatal("undecodable Deployment gave a ready floor")
	}
	if _, err := o.workloadRequiresTargetNode(fixture); err == nil {
		t.Fatal("undecodable Deployment counted as portable")
	}

	// kubectl failing keeps the Bash fallback.
	f.respond = func(c Command, line string) (Result, bool) {
		if strings.HasSuffix(line, "get deployment/fixture -o json") {
			return Result{Status: 1}, true
		}
		return Result{}, false
	}
	if replicas, err := o.controllerReplicas(fixture); err != nil || replicas != 1 {
		t.Fatalf("unreadable controller: %d, %v; want the fallback of one", replicas, err)
	}
}

// A backup target that does not decode cannot pass
// --require-longhorn-backup-target.
func TestUndecodableBackupTargetFails(t *testing.T) {
	f := newHealthyCluster(t)
	f.respond = func(c Command, line string) (Result, bool) {
		if strings.HasSuffix(line, "get backuptargets.longhorn.io default -o json") {
			return Result{Stdout: `{"status": {"available": "yes"}}`}, true
		}
		return Result{}, false
	}
	o, _ := newOperation(f, options(t, "reboot", "--require-longhorn-backup-target", "server-a"))
	expectFailure(t, o.waitForLonghornHealth(), "invalid JSON")
}
