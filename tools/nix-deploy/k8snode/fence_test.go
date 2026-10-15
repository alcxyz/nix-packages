package k8snode

import (
	"encoding/json"
	"strings"
	"testing"
)

// These tests port test-k8s-node-reboot-fence-gates.sh case by case; each
// comment repeats the Bash check's intent.

func TestFencePreflightRecordsModeAndRecoveryPasses(t *testing.T) {
	f := newFenceCluster(t)
	o, _ := fenceOperation(t, f, "reboot")
	if err := o.verifyFencePreflight(); err != nil {
		t.Fatal(err)
	}
	if !o.fence.hasAgent || o.fence.previousMode != "enforce" {
		t.Fatalf("hasAgent=%v previousMode=%q", o.fence.hasAgent, o.fence.previousMode)
	}
	if err := o.waitForReturnedFenceAgent(); err != nil {
		t.Fatal(err)
	}
	mustCall(t, f, `--show-managed-fields=true`)
	// A resumed target may already be cordoned; fence preflight is
	// independent of it.
	o.opts.ResumeMaintenance = true
	if err := o.verifyFencePreflight(); err != nil {
		t.Fatal(err)
	}
}

func TestFencePreflightFailures(t *testing.T) {
	cases := []struct {
		name string
		edit func(t *testing.T, f *fakeCluster)
		want string
	}{
		{"peer fenced", func(t *testing.T, f *fakeCluster) {
			set(f.status, true, "states", 1, "fenced")
		}, "server-b: fenced"},
		// A server missing from the agent's peer list is not silently
		// skipped.
		{"control-plane server missing from the peer list", func(t *testing.T, f *fakeCluster) {
			f.nodes = append(f.nodes, decode(t, `{"metadata":{"name":"server-c","labels":{"node-role.kubernetes.io/control-plane":"true"}},"spec":{}}`))
		}, "server-c: control-plane Node has no verified self-fence state"},
		{"unverified peer named by its address", func(t *testing.T, f *fakeCluster) {
			set(f.status, map[string]any{"address": "192.0.2.2", "self": false, "ok": false, "error": "unauthenticated"}, "states", 1)
			f.statusExit = 1
		}, "192.0.2.2: unverified fence state (unauthenticated)"},
		{"peer classification", func(t *testing.T, f *fakeCluster) {
			set(f.status, "ambiguous", "states", 1, "classification")
		}, "server-b: classification ambiguous"},
		{"recent unfence", func(t *testing.T, f *fakeCluster) {
			set(f.status, 899.0, "states", 1, "since_unfence")
		}, "server-b: recent unfence (899s < refence window 900s)"},
		{"disabled annotation", func(t *testing.T, f *fakeCluster) {
			set(f.nodes[1], "true", "metadata", "annotations", "fence.alc.xyz/disabled")
		}, "server-b: fence.alc.xyz/disabled=true"},
		{"out-of-service taint", func(t *testing.T, f *fakeCluster) {
			set(f.nodes[1], []any{map[string]any{"key": "node.kubernetes.io/out-of-service", "effect": "NoExecute"}}, "spec", "taints")
		}, "server-b: node.kubernetes.io/out-of-service taint"},
		{"agent not deployed", func(t *testing.T, f *fakeCluster) {
			f.statusExit = 127
			empty := ""
			f.statusText = &empty
		}, "self-fence agent is not deployed"},
		{"status command failed", func(t *testing.T, f *fakeCluster) {
			f.statusExit = 2
		}, "node-self-fence-status failed (exit 2)"},
		{"invalid JSON", func(t *testing.T, f *fakeCluster) {
			invalid := "invalid json\n"
			f.statusText = &invalid
		}, "invalid or unavailable self-fence status"},
		{"two JSON documents", func(t *testing.T, f *fakeCluster) {
			data, _ := json.Marshal(f.status)
			twice := string(data) + "\n" + string(data) + "\n"
			f.statusText = &twice
		}, "invalid or unavailable self-fence status"},
		{"missing since_unfence", func(t *testing.T, f *fakeCluster) {
			del(f.status, "states", 1, "since_unfence")
		}, "server-b: missing or invalid unfence timing"},
		{"no states", func(t *testing.T, f *fakeCluster) {
			set(f.status, []any{}, "states")
		}, "invalid or unavailable self-fence status"},
		{"only the own state", func(t *testing.T, f *fakeCluster) {
			set(f.status, []any{get(f.status, "states", 0)}, "states")
		}, "incomplete self-fence server states"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFenceCluster(t)
			c.edit(t, f)
			o, _ := fenceOperation(t, f, "reboot")
			expectFailure(t, o.verifyFencePreflight(), c.want)
		})
	}
}

// The recheck before the power action keeps the pre-maintenance mode.
func TestFencePreflightRecheckKeepsMode(t *testing.T) {
	f := newFenceCluster(t)
	o, _ := fenceOperation(t, f, "reboot")
	if err := o.verifyFencePreflight(); err != nil {
		t.Fatal(err)
	}
	set(f.status, "observe", "states", 0, "mode")
	expectFailure(t, o.verifyFencePreflight(), "agent mode changed from enforce to observe")
}

// Leading zeros would be read as octal by the Bash duration arithmetic; the
// same spellings stay rejected.
func TestFenceTimeoutRejectsLeadingZeros(t *testing.T) {
	_, err := ParseArgs("reboot", []string{"--fence-timeout", "08m", "server-a"})
	expectFailure(t, err, "unsupported fence duration: 08m")

	opts, err := ParseArgs("reboot", []string{"--fence-timeout", "3m", "--check-only", "server-a"})
	if err != nil || opts.FenceTimeout != "3m" || !opts.CheckOnly {
		t.Fatalf("got %+v, %v", opts, err)
	}
}

func TestAgentlessTargetSkipsFenceGates(t *testing.T) {
	f := newFenceCluster(t)
	del(f.targetNode(), "metadata", "annotations", agentModeAnnotation)
	o, output := fenceOperation(t, f, "reboot")
	if err := o.verifyFencePreflight(); err != nil {
		t.Fatal(err)
	}
	if err := o.stopFenceAgent(); err != nil {
		t.Fatal(err)
	}
	if err := o.waitForReturnedFenceAgent(); err != nil {
		t.Fatal(err)
	}
	if o.fence.hasAgent {
		t.Fatal("agentless target has an agent")
	}
	mustContain(t, output.String(), "skipping all fence gates")
	if len(f.calls) != 1 {
		t.Fatalf("agentless gates made %d calls:\n%s", len(f.calls), f.callLog())
	}
	// Any annotation value marks an enabled agent.
	set(f.targetNode(), "", "metadata", "annotations", agentModeAnnotation)
	if err := o.verifyFencePreflight(); err != nil {
		t.Fatal(err)
	}
	if !o.fence.hasAgent {
		t.Fatal("an empty agent-mode annotation did not enable the gates")
	}
}

func TestStopFenceAgentRecordsStopTime(t *testing.T) {
	f := newFenceCluster(t)
	o, _ := fenceOperation(t, f, "reboot")
	if err := o.verifyFencePreflight(); err != nil {
		t.Fatal(err)
	}
	set(f.targetNode(), "stopped", "metadata", "annotations", agentModeAnnotation)
	if err := o.stopFenceAgent(); err != nil {
		t.Fatal(err)
	}
	mustCall(t, f, `systemctl stop node-self-fence`)
	if !o.fence.agentStopped || !o.fence.hasBaseline || o.fence.baseline != unix(t, "2026-01-01T00:03:00Z") {
		t.Fatalf("stopped=%v baseline=%d (%v)", o.fence.agentStopped, o.fence.baseline, o.fence.hasBaseline)
	}
}

// A peer that fences while the target drains blocks the power action.
func TestStopFenceAgentRechecksPeers(t *testing.T) {
	f := newFenceCluster(t)
	o, _ := fenceOperation(t, f, "reboot")
	if err := o.verifyFencePreflight(); err != nil {
		t.Fatal(err)
	}
	set(f.status, true, "states", 1, "fenced")
	set(f.status, "fenced", "states", 1, "classification")
	expectFailure(t, o.stopFenceAgent(), "fenced or missing fenced state")
	mustNotCall(t, f, `systemctl stop node-self-fence`)
}

func TestStopFenceAgentFailuresLeaveNodeCordoned(t *testing.T) {
	f := newFenceCluster(t)
	o, _ := fenceOperation(t, f, "reboot")
	if err := o.verifyFencePreflight(); err != nil {
		t.Fatal(err)
	}
	err := o.stopFenceAgent()
	expectFailure(t, err, "agent did not publish mode=stopped")
	expectFailure(t, err, "remains cordoned")
	expectFailure(t, err, "systemctl start node-self-fence")
	if o.fence.agentStopped {
		t.Fatal("an unconfirmed stop was recorded as stopped")
	}

	f.stopExit = 1
	expectFailure(t, o.stopFenceAgent(), "failed to stop node-self-fence")
}

func TestReturnedFenceAgentFailures(t *testing.T) {
	cases := []struct {
		name string
		edit func(f *fakeCluster)
		want []string
	}{
		{"own agent in grace", func(f *fakeCluster) {
			set(f.status, "grace", "states", 0, "classification")
		}, []string{"own agent is not healthy", "remains cordoned"}},
		{"own agent fenced", func(f *fakeCluster) {
			set(f.status, true, "states", 0, "fenced")
		}, []string{"fenced=true"}},
		{"status command failed", func(f *fakeCluster) {
			f.statusExit = 2
		}, []string{"node-self-fence-status failed (exit 2)"}},
		{"mode differs from before maintenance", func(f *fakeCluster) {
			set(f.status, "observe", "states", 0, "mode")
		}, []string{"differs from pre-maintenance mode enforce"}},
		// The pre-power-action stop also writes with the agent's field
		// manager; it is not a heartbeat.
		{"stop write is not a heartbeat", func(f *fakeCluster) {
			set(f.targetNode(), "stopped", "metadata", "annotations", agentModeAnnotation)
		}, []string{"no node-self-fence heartbeat after the maintenance baseline"}},
		{"heartbeat from another manager", func(f *fakeCluster) {
			set(f.targetNode(), []any{map[string]any{"manager": "other-manager", "time": "2026-01-01T00:04:00Z"}}, "metadata", "managedFields")
		}, []string{"no node-self-fence heartbeat after the maintenance baseline"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFenceCluster(t)
			o, _ := fenceOperation(t, f, "reboot")
			if err := o.verifyFencePreflight(); err != nil {
				t.Fatal(err)
			}
			c.edit(f)
			err := o.waitForReturnedFenceAgent()
			for _, want := range c.want {
				expectFailure(t, err, want)
			}
		})
	}
}

// The on action accepts either mode.
func TestReturnedFenceAgentOnAcceptsEitherMode(t *testing.T) {
	f := newFenceCluster(t)
	o, output := fenceOperation(t, f, "reboot")
	if err := o.verifyFencePreflight(); err != nil {
		t.Fatal(err)
	}
	set(f.status, "observe", "states", 0, "mode")
	o.opts.Action = "on"
	set(f.targetNode(), "observe", "metadata", "annotations", agentModeAnnotation)
	if err := o.waitForReturnedFenceAgent(); err != nil {
		t.Fatal(err)
	}
	mustContain(t, output.String(), "recovered in observe mode")
}

// Return gates require only the own entry; an unverified peer is not a
// blocker.
func TestReturnedFenceAgentIgnoresUnverifiedPeer(t *testing.T) {
	f := newFenceCluster(t)
	o, _ := fenceOperation(t, f, "reboot")
	if err := o.verifyFencePreflight(); err != nil {
		t.Fatal(err)
	}
	set(f.status, false, "states", 1, "ok")
	set(f.status, "no answer", "states", 1, "error")
	f.statusExit = 1
	if err := o.waitForReturnedFenceAgent(); err != nil {
		t.Fatal(err)
	}
}

// Heartbeats use an API-server baseline, independent of the kubelet clock.
func TestReturnedFenceAgentHeartbeatBaseline(t *testing.T) {
	for _, timestamp := range []string{"2026-01-01T00:02:00Z", "2026-01-01T00:03:00Z"} {
		f := newFenceCluster(t)
		o, _ := fenceOperation(t, f, "reboot")
		if err := o.verifyFencePreflight(); err != nil {
			t.Fatal(err)
		}
		o.fence.baseline, o.fence.hasBaseline = unix(t, "2026-01-01T00:03:00Z"), true
		set(f.targetNode(), timestamp, "metadata", "managedFields", 0, "time")
		expectFailure(t, o.waitForReturnedFenceAgent(), "no node-self-fence heartbeat after the maintenance baseline")
	}

	f := newFenceCluster(t)
	o, _ := fenceOperation(t, f, "reboot")
	if err := o.verifyFencePreflight(); err != nil {
		t.Fatal(err)
	}
	o.fence.baseline, o.fence.hasBaseline = unix(t, "2026-01-01T00:02:30Z"), true
	set(f.targetNode(), "2026-01-01T00:09:00Z", "status", "conditions", 0, "lastTransitionTime")
	if err := o.waitForReturnedFenceAgent(); err != nil {
		t.Fatal(err)
	}
}

// Fence preflight must run in both check-only paths, and before the
// disruption, ahead of every other maintenance gate.
func TestFencePreflightRunsBeforeOtherGates(t *testing.T) {
	for _, resume := range []bool{false, true} {
		f := newFenceCluster(t)
		set(f.status, true, "states", 1, "fenced")
		o, _ := fenceOperation(t, f, "reboot")
		o.opts.ResumeMaintenance = resume
		expectFailure(t, o.checkOnly(), "server-b: fenced")
		expectFailure(t, o.prepareNodeForDisruption(), "server-b: fenced")
		if len(f.audits) > 0 {
			t.Fatalf("network gate reached before the fence failure: %v", f.audits)
		}
		if mutations := f.mutations(); len(mutations) > 0 {
			t.Fatalf("failed preflight attempted a mutation: %v", mutations)
		}
	}
}

// assertStillCordoned is the Bash check's assert_still_cordoned.
func assertStillCordoned(t *testing.T, f *fakeCluster, output string) {
	t.Helper()
	if get(f.targetNode(), "spec", "unschedulable") != true {
		t.Fatal("the node was uncordoned")
	}
	mustNotCall(t, f, `^kubectl uncordon`)
	mustContain(t, output, "remains cordoned")
}

// A new power-on process must require a heartbeat after its API-clock
// baseline even when an outage left mode=enforce behind. Equal timestamps
// are stale too.
func TestPowerOnRequiresHeartbeatAfterBaseline(t *testing.T) {
	for _, timestamp := range []string{"2026-01-01T00:03:00Z", "2026-01-01T00:04:00Z"} {
		f := newFenceCluster(t)
		set(f.targetNode(), timestamp, "metadata", "managedFields", 0, "time")
		o, output := fenceOperation(t, f, "on")
		expectRunFailure(t, o, output, "no node-self-fence heartbeat after the maintenance baseline")
		assertStillCordoned(t, f, output.String())
		mustCall(t, f, `^kubectl annotate node server-a fence.alc.xyz/maintenance-baseline=`)
		mustCall(t, f, `^kubectl annotate node server-a fence.alc.xyz/maintenance-baseline-`)
		if get(f.targetNode(), "metadata", "annotations", baselineAnnotation) != nil {
			t.Fatal("the baseline annotation was left behind")
		}
	}
}

func TestPowerOnUncordonsAfterFreshHeartbeat(t *testing.T) {
	f := newFenceCluster(t)
	set(f.targetNode(), "2026-01-01T00:05:00Z", "metadata", "managedFields", 0, "time")
	set(f.targetNode(), "2026-01-01T00:09:00Z", "status", "conditions", 0, "lastTransitionTime")
	o, output := fenceOperation(t, f, "on")
	if status := o.Run(); status != 0 {
		t.Fatalf("status %d:\n%s", status, output)
	}
	if o.fence.baseline != unix(t, "2026-01-01T00:04:00Z") {
		t.Fatalf("baseline %d", o.fence.baseline)
	}
	mustCall(t, f, `^kubectl uncordon server-a`)
	if get(f.targetNode(), "spec", "unschedulable") != false {
		t.Fatal("node not uncordoned")
	}
	if get(f.targetNode(), "metadata", "annotations", baselineAnnotation) != nil {
		t.Fatal("the baseline annotation was left behind")
	}
}

// Baseline write, read and cleanup errors, and missing or invalid manager
// timestamps, fail closed even if the agent's old heartbeat otherwise looks
// healthy.
func TestPowerOnBaselineFailuresFailClosed(t *testing.T) {
	for _, failure := range []string{"write", "read", "cleanup", "missing-manager", "invalid-time"} {
		t.Run(failure, func(t *testing.T) {
			f := newFenceCluster(t)
			switch failure {
			case "write":
				f.baselineWriteExit = 1
			case "read":
				f.baselineReadExit = 1
			case "cleanup":
				f.baselineCleanupExit = 1
			case "missing-manager":
				f.baselineManager = "other-manager"
			case "invalid-time":
				f.baselineTime = "invalid"
			}
			o, output := fenceOperation(t, f, "on")
			expectRunFailure(t, o, output, "could not establish self-fence heartbeat baseline")
			assertStillCordoned(t, f, output.String())
			if failure == "read" || failure == "missing-manager" || failure == "invalid-time" {
				mustCall(t, f, `^kubectl annotate node server-a fence.alc.xyz/maintenance-baseline-`)
			}
		})
	}
}

// Agentless targets skip the baseline write and recovery gate.
func TestPowerOnAgentlessSkipsBaseline(t *testing.T) {
	f := newFenceCluster(t)
	del(f.targetNode(), "metadata", "annotations", agentModeAnnotation)
	o, output := fenceOperation(t, f, "on")
	if status := o.Run(); status != 0 {
		t.Fatalf("status %d:\n%s", status, output)
	}
	mustCall(t, f, `^kubectl uncordon server-a`)
	mustNotCall(t, f, `^kubectl annotate`)
}

// A recovery gate failure must stop before uncordon.
func TestPowerOnRecoveryFailureLeavesNodeCordoned(t *testing.T) {
	f := newFenceCluster(t)
	set(f.targetNode(), "2026-01-01T00:05:00Z", "metadata", "managedFields", 0, "time")
	set(f.status, "grace", "states", 0, "classification")
	o, output := fenceOperation(t, f, "on")
	expectRunFailure(t, o, output, "own agent is not healthy")
	assertStillCordoned(t, f, output.String())
}

func TestParseFenceStatus(t *testing.T) {
	valid := `{"node":"server-a","states":[{"self":true,"node":"server-a"},null,{"self":false,"node":"server-b"}]}`
	if _, ok := parseFenceStatus(valid, "server-a"); !ok {
		t.Fatal("valid status rejected")
	}
	for _, text := range []string{
		"",
		`{"node":"server-b","states":[{"self":true,"node":"server-a"}]}`,
		`{"node":"server-a","states":[{"self":true,"node":"server-b"}]}`,
		`{"node":"server-a","states":[{"self":true,"node":"server-a"},{"self":true,"node":"server-a"}]}`,
		`{"node":"server-a","states":[{"self":true,"node":"server-a"},5]}`,
		`{"node":"server-a","states":[{"self":true,"node":"server-a"}]} trailing`,
		`[]`,
	} {
		if _, ok := parseFenceStatus(text, "server-a"); ok {
			t.Errorf("accepted %q", text)
		}
	}
}

func TestFenceStateFailureText(t *testing.T) {
	states := []map[string]any{
		{"ok": true, "fenced": false, "classification": "healthy", "mode": "enforce", "refence_window": 900.0, "since_unfence": "soon", "node": "server-b"},
		{"ok": true, "fenced": false, "classification": "healthy", "mode": "stopped", "refence_window": 900.0, "since_unfence": nil, "node": "server-c"},
		{},
	}
	got := strings.Join(fenceStateFailures(states), "\n")
	want := "server-b: invalid since_unfence\nserver-c: agent mode stopped\nunknown server: unverified fence state (missing ok)"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
