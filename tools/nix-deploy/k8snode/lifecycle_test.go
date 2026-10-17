package k8snode

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// End-to-end runs of each action against the healthy fixture, and the
// ADR-0068 and ADR-0082 gates the Bash checks did not cover.

// callIndex is the position of the first call matching the pattern, or -1.
func (f *fakeCluster) callIndex(pattern string) int {
	re := regexp.MustCompile(pattern)
	for i, c := range f.calls {
		if re.MatchString(c.line) || re.MatchString(c.stdin) {
			return i
		}
	}
	return -1
}

// requireOrder checks that calls matching the patterns happened in order.
func requireOrder(t *testing.T, f *fakeCluster, patterns ...string) {
	t.Helper()
	previous := -1
	for _, pattern := range patterns {
		i := f.callIndex(pattern)
		if i < 0 {
			t.Fatalf("no call matching %q:\n%s", pattern, f.callLog())
		}
		if i < previous {
			t.Fatalf("%q ran before the previous step:\n%s", pattern, f.callLog())
		}
		previous = i
	}
}

func run(t *testing.T, f *fakeCluster, action string, args ...string) string {
	t.Helper()
	o, output := newOperation(f, options(t, action, append(args, "server-a")...))
	if status := o.Run(); status != 0 {
		t.Fatalf("status %d:\n%s\ncalls:\n%s", status, output, f.callLog())
	}
	return output.String()
}

func runFailure(t *testing.T, f *fakeCluster, action string, args []string, want ...string) string {
	t.Helper()
	o, output := newOperation(f, options(t, action, append(args, "server-a")...))
	expectRunFailure(t, o, output, want...)
	return output.String()
}

func TestRebootLifecycle(t *testing.T) {
	f := newHealthyCluster(t)
	output := run(t, f, "reboot")
	requireOrder(t, f,
		`^kubectl get node server-a$`,
		regexp.QuoteMeta(privilegeCheckCommand),
		`^kubectl get poddisruptionbudgets`,
		`^kubectl get nodes -l workload-class=stable`,
		`^kubectl cordon server-a$`,
		`^kubectl drain server-a --ignore-daemonsets --delete-emptydir-data --pod-selector=longhorn.io/component!=instance-manager --timeout=10m$`,
		`^kubectl get volumeattachments`,
		`cleanup_sessions=true`,
		`^ssh -o BatchMode=yes root@server-a cat /proc/sys/kernel/random/boot_id$`,
		regexp.QuoteMeta(rebootCommand),
		`ConnectTimeout=2 root@server-a true$`,
		`ConnectTimeout=5 root@server-a true$`,
		`^kubectl wait node/server-a --for=condition=Ready --timeout=10m$`,
		`ip link show dev flannel.1`,
		`^kubectl uncordon server-a$`,
		`^kubectl -n default rollout status deployment/web --timeout=90m$`,
	)
	mustContain(t, output, "==> done")
	if get(f.targetNode(), "spec", "unschedulable") != false {
		t.Fatal("node left cordoned")
	}
	// The availability gate reads the Deployment again after the drain.
	drain := f.callIndex(`^kubectl drain server-a`)
	gate := slices.IndexFunc(f.calls[drain:], func(c call) bool { return c.line == "kubectl -n default get deployment/web -o json" })
	if gate < 0 || drain+gate > f.callIndex(`^kubectl get volumeattachments`) {
		t.Fatalf("no availability gate between the drain and the storage detach:\n%s", f.callLog())
	}
	if f.settings[rebuildLimitSetting] != "0" {
		t.Fatalf("rebuild limit %q", f.settings[rebuildLimitSetting])
	}
	mustNotCall(t, f, `^kubectl -n longhorn-system patch`)
}

func TestRebootLeaveCordoned(t *testing.T) {
	f := newHealthyCluster(t)
	output := run(t, f, "reboot", "--leave-cordoned", "--force-drain", "--bypass-pdb")
	mustContain(t, output, "leaving server-a cordoned")
	mustCall(t, f, `^kubectl drain server-a .* --force --disable-eviction$`)
	mustNotCall(t, f, `^kubectl uncordon`)
}

func TestSkipDrainCordonsOnly(t *testing.T) {
	f := newHealthyCluster(t)
	output := run(t, f, "reboot", "--skip-drain")
	mustContain(t, output, "skipping drain by request")
	mustNotCall(t, f, `^kubectl drain`)
	requireOrder(t, f, `^kubectl cordon`, `cleanup_sessions=true`, regexp.QuoteMeta(rebootCommand))
}

func TestPowerOffLeavesNodeCordoned(t *testing.T) {
	f := newHealthyCluster(t)
	output := run(t, f, "off")
	requireOrder(t, f, `^kubectl cordon`, `^kubectl drain`, regexp.QuoteMeta(powerOffCommand), `ConnectTimeout=2 root@server-a true$`)
	mustContain(t, output, "server-a is powered off and remains cordoned")
	mustNotCall(t, f, `^kubectl uncordon`)
	// Survivors are audited after the node left service; the target is not.
	for _, audit := range f.audits[len(f.audits)-2:] {
		if strings.HasPrefix(audit, "server-a\t") {
			t.Fatalf("target audited after power-off: %v", f.audits)
		}
	}
}

func TestPowerOnFinalizes(t *testing.T) {
	f := newHealthyCluster(t)
	set(f.targetNode(), true, "spec", "unschedulable")
	output := run(t, f, "on")
	requireOrder(t, f, `ConnectTimeout=5 root@server-a true$`, `^kubectl wait node/server-a`, `ip link show dev flannel.1`, `^kubectl uncordon server-a$`)
	mustContain(t, output, "==> done")
	mustNotCall(t, f, `^kubectl (cordon|drain)`)
}

func TestPowerOnRequiresCordon(t *testing.T) {
	f := newHealthyCluster(t)
	runFailure(t, f, "on", nil, "server-a is not cordoned")
	mustNotCall(t, f, `^kubectl uncordon`)
}

// A check-only run validates the fresh preflight and reports planned
// switchovers without one mutating call.
func TestCheckOnlyMakesNoChanges(t *testing.T) {
	f := withAgent(t, withDatabase(t, newHealthyCluster(t)))
	output := run(t, f, "reboot", "--check-only")
	mustContain(t, output, "default\tdb\tdb-1\tdb-2\tserver-b")
	mustContain(t, output, "checks passed; no changes were made")
	if mutations := f.mutations(); len(mutations) > 0 {
		t.Fatalf("check-only changed state: %v", mutations)
	}
}

func TestCheckOnlyResumeMakesNoChanges(t *testing.T) {
	f := withAgent(t, drained(t, newHealthyCluster(t)))
	output := run(t, f, "off", "--check-only", "--resume-maintenance")
	mustContain(t, output, "validating resumed maintenance state for server-a")
	// The host check validates that sessions could be reconciled without
	// reconciling them.
	mustCall(t, f, `cleanup_sessions=false`)
	if mutations := f.mutations(); len(mutations) > 0 {
		t.Fatalf("check-only changed state: %v", mutations)
	}
}

// Even with degraded volumes and closed rebuild admission, check-only never
// raises the rebuild limit.
func TestCheckOnlyDoesNotRaiseRebuildLimit(t *testing.T) {
	f := newHealthyCluster(t)
	set(f.volumes[0], "degraded", "status", "robustness")
	runFailure(t, f, "reboot", []string{"--check-only", "--settle-timeout", "1m"}, "Longhorn volumes are not healthy")
	mustNotCall(t, f, `patch`)
}

func TestCheckOnlyRejectsCordonedNode(t *testing.T) {
	f := newHealthyCluster(t)
	set(f.targetNode(), true, "spec", "unschedulable")
	runFailure(t, f, "reboot", []string{"--check-only"}, "already cordoned; use --resume-maintenance for maintenance-state checks")
}

func TestPrivilegeFailureChangesNothing(t *testing.T) {
	f := newHealthyCluster(t)
	f.privilegeExit = 1
	runFailure(t, f, "reboot", nil, "root@server-a does not provide noninteractive root access; no cluster state was changed")
	if mutations := f.mutations(); len(mutations) > 0 {
		t.Fatalf("mutations: %v", mutations)
	}
}

// A drain that fails leaves the node running untouched, so the run lifts
// its cordon and the cluster gets the node's room back.
func TestDrainFailureUncordons(t *testing.T) {
	for _, action := range []string{"reboot", "off"} {
		t.Run(action, func(t *testing.T) {
			f := newHealthyCluster(t)
			f.drainExit = 1
			runFailure(t, f, action, nil, "drain failed; node disruption was not started; server-a was uncordoned")
			if get(f.targetNode(), "spec", "unschedulable") != false {
				t.Fatal("node left cordoned")
			}
			requireOrder(t, f, `^kubectl cordon server-a$`, `^kubectl drain server-a`, `^kubectl uncordon server-a$`)
			mustNotCall(t, f, regexp.QuoteMeta(rebootCommand))
			mustNotCall(t, f, `poweroff`)
		})
	}

	// A reboot with --leave-cordoned keeps the cordon.
	f := newHealthyCluster(t)
	f.drainExit = 1
	output := runFailure(t, f, "reboot", []string{"--leave-cordoned"}, "drain failed; node disruption was not started")
	mustNotCall(t, f, `^kubectl uncordon`)
	if strings.Contains(output, "was uncordoned") {
		t.Fatalf("output claims an uncordon:\n%s", output)
	}
}

// When the uncordon fails too, the error says the node stays cordoned.
func TestDrainFailureReportsFailedUncordon(t *testing.T) {
	f := newHealthyCluster(t)
	f.drainExit = 1
	f.respond = func(c Command, line string) (Result, bool) {
		if line == "kubectl uncordon server-a" {
			return Result{Status: 1}, true
		}
		return Result{}, false
	}
	runFailure(t, f, "reboot", nil, "drain failed; node disruption was not started; could not uncordon server-a, which remains cordoned")
}

// An interrupted drain keeps the cordon for the operator to inspect.
func TestInterruptedDrainKeepsCordon(t *testing.T) {
	f := newHealthyCluster(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.respond = func(c Command, line string) (Result, bool) {
		if strings.HasPrefix(line, "kubectl drain ") {
			cancel()
			return Result{Status: 1}, true
		}
		return Result{}, false
	}
	o, output := newOperation(f, options(t, "reboot", "server-a"))
	o.ctx = ctx
	expectRunFailure(t, o, output, "interrupted; server-a may need inspection")
	mustNotCall(t, f, `^kubectl uncordon`)
	if strings.Contains(output.String(), "uncordon") {
		t.Fatalf("an interrupted run tried to uncordon:\n%s", output)
	}
}

// A resumed maintenance never lifts the cordon it found.
func TestResumedMaintenanceKeepsCordonOnFailure(t *testing.T) {
	f := newHealthyCluster(t)
	set(f.targetNode(), true, "spec", "unschedulable")
	runFailure(t, f, "reboot", []string{"--resume-maintenance"}, "non-maintenance Pods remain on server-a")
	mustNotCall(t, f, `^kubectl uncordon`)
}

func TestRemoteRebootFailureLeavesNodeCordoned(t *testing.T) {
	f := newHealthyCluster(t)
	f.rebootExit = 1
	runFailure(t, f, "reboot", nil, "remote reboot command failed before reboot started; server-a remains cordoned")
	mustNotCall(t, f, `^kubectl uncordon`)
}

func TestRemotePowerOffFailure(t *testing.T) {
	f := newHealthyCluster(t)
	f.rebootExit = 1
	runFailure(t, f, "off", nil, "remote poweroff command failed before shutdown started")
}

func TestUnchangedBootIDLeavesNodeCordoned(t *testing.T) {
	f := newHealthyCluster(t)
	f.rebootKeepsBoot = true
	runFailure(t, f, "reboot", nil, "root@server-a returned without rebooting; server-a remains cordoned")
	mustNotCall(t, f, `^kubectl uncordon`)
}

func TestSSHThatNeverDropsLeavesNodeCordoned(t *testing.T) {
	f := newHealthyCluster(t)
	f.sshNeverDrops = true
	runFailure(t, f, "reboot", nil, "SSH did not drop; the power action was not verified and server-a remains cordoned")
	probes := 0
	for _, c := range f.calls {
		if strings.HasSuffix(c.line, "ConnectTimeout=2 root@server-a true") {
			probes++
		}
	}
	if probes != sshDownAttempts {
		t.Fatalf("%d probes", probes)
	}
	mustNotCall(t, f, `^kubectl uncordon`)
}

func TestSSHThatNeverReturns(t *testing.T) {
	f := newHealthyCluster(t)
	f.respond = func(c Command, line string) (Result, bool) {
		if f.callIndex(regexp.QuoteMeta(rebootCommand)) >= 0 && strings.HasSuffix(line, "ConnectTimeout=5 root@server-a true") {
			return Result{Status: 255}, true
		}
		return Result{}, false
	}
	runFailure(t, f, "reboot", []string{"--ssh-timeout", "20"}, "SSH did not return within 20s")
	mustNotCall(t, f, `^kubectl uncordon`)
}

func TestFlannelMismatchBlocksUncordon(t *testing.T) {
	f := newHealthyCluster(t)
	set(f.targetNode(), "192.0.2.99", "metadata", "annotations", "flannel.alpha.coreos.com/public-ip")
	runFailure(t, f, "reboot", nil, "server-a Flannel public IP (192.0.2.99) does not match InternalIP (192.0.2.1)")
	mustCall(t, f, regexp.QuoteMeta(rebootCommand))
	mustNotCall(t, f, `^kubectl uncordon`)
}

func TestAlreadyCordonedNodeNeedsResume(t *testing.T) {
	f := newHealthyCluster(t)
	set(f.targetNode(), true, "spec", "unschedulable")
	runFailure(t, f, "reboot", nil, "server-a is already cordoned; use --resume-maintenance for another power cycle")
	mustNotCall(t, f, `^kubectl drain`)
}

// drained is the healthy cluster after a cordon and drain of server-a.
func drained(t *testing.T, f *fakeCluster) *fakeCluster {
	set(f.targetNode(), true, "spec", "unschedulable")
	f.drain("server-a")
	return f
}

func TestResumeMaintenanceRebootsDrainedNode(t *testing.T) {
	f := drained(t, newHealthyCluster(t))
	output := run(t, f, "reboot", "--resume-maintenance")
	mustContain(t, output, "validating resumed maintenance state for server-a")
	mustNotCall(t, f, `^kubectl (cordon|drain)`)
	requireOrder(t, f, `cleanup_sessions=true`, regexp.QuoteMeta(rebootCommand), `^kubectl uncordon server-a$`)
}

func TestResumeMaintenanceRejectsWorkloadPods(t *testing.T) {
	f := newHealthyCluster(t)
	set(f.targetNode(), true, "spec", "unschedulable")
	runFailure(t, f, "reboot", []string{"--resume-maintenance"}, "default/web-1\towner=ReplicaSet\tphase=Running", "non-maintenance Pods remain on server-a")
	mustNotCall(t, f, regexp.QuoteMeta(rebootCommand))
}

// withPinned adds a Deployment that can run only on server-a.
func withPinned(t *testing.T, f *fakeCluster, phase string) *fakeCluster {
	set(f.targetNode(), "true", "metadata", "labels", "browser-worker")
	f.controllers["default/replicaset/pinned-7c"] = decode(t, `{"metadata": {"ownerReferences": [{"kind": "Deployment", "name": "pinned"}]}}`)
	f.controllers["default/deployment/pinned"] = decode(t, `{"spec": {"replicas": 1, "selector": {"matchLabels": {"app": "pinned"}},
	  "template": {"metadata": {"labels": {"app": "pinned"}}, "spec": {"nodeSelector": {"browser-worker": "true"}}}}}`)
	p := readyPod(t, "default", "pinned-1", "server-a", "ReplicaSet", "pinned-7c", `{"app": "pinned"}`)
	if phase == "Pending" {
		set(p, "", "spec", "nodeName")
		set(p, map[string]any{"phase": "Pending"}, "status")
	}
	f.pods = append(f.pods, p)
	return f
}

// A controller that can run only on the target is deferred during the
// drain and must settle after uncordon.
func TestTargetPinnedWorkloadDeferredUntilReturn(t *testing.T) {
	f := withPinned(t, newHealthyCluster(t), "Running")
	output := run(t, f, "reboot")
	mustContain(t, output, "deferring target-pinned default/deployment/pinned until server-a returns")
	requireOrder(t, f, `^kubectl uncordon server-a$`, `^kubectl -n default rollout status deployment/pinned`)
}

// Resumed reboots and power-on reconstruct the deferred set from Pending
// Pods.
func TestPendingTargetPinnedWorkloadsReconstructed(t *testing.T) {
	f := withPinned(t, drained(t, newHealthyCluster(t)), "Pending")
	run(t, f, "reboot", "--resume-maintenance")
	requireOrder(t, f, `^kubectl uncordon server-a$`, `^kubectl -n default rollout status deployment/pinned`)

	f = withPinned(t, drained(t, newHealthyCluster(t)), "Pending")
	run(t, f, "on")
	requireOrder(t, f, `^kubectl uncordon server-a$`, `^kubectl -n default rollout status deployment/pinned`)
}

func TestLonghornReplicaFloorFailsImmediately(t *testing.T) {
	f := newHealthyCluster(t)
	set(f.volumes[0], 2.0, "spec", "numberOfReplicas")
	runFailure(t, f, "reboot", nil, "pvc-1\tdesired=2\tattached\tdefault/data", "attached Longhorn volumes are below the replica policy floor")
	if f.sleeps != 0 {
		t.Fatalf("waited %d times on a policy failure", f.sleeps)
	}
	mustNotCall(t, f, `^kubectl cordon`)
}

// With rebuild admission closed and a degraded volume, the run admits two
// rebuilds per node and restores the original value when the health gate
// fails.
func TestRebuildLimitRestoredWhenHealthFails(t *testing.T) {
	f := newHealthyCluster(t)
	set(f.volumes[0], "degraded", "status", "robustness")
	output := runFailure(t, f, "reboot", []string{"--settle-timeout", "1m"}, "Longhorn volumes are not healthy")
	requireOrder(t, f, `patch settings.longhorn.io concurrent-replica-rebuild-per-node-limit --type=merge -p \{"value":"2"\}`,
		`patch settings.longhorn.io concurrent-replica-rebuild-per-node-limit --type=merge -p \{"value":"0"\}`)
	mustContain(t, output, "temporarily enabling two guarded Longhorn replica rebuilds per node")
	mustContain(t, output, "restoring Longhorn replica rebuild admission to 0")
	if f.settings[rebuildLimitSetting] != "0" {
		t.Fatalf("rebuild limit left at %q", f.settings[rebuildLimitSetting])
	}
	mustNotCall(t, f, `^kubectl cordon`)
	// Progress is rate-limited to one report every five minutes.
	if n := strings.Count(output, "attached Longhorn volume(s) are still reconciling"); n != 1 {
		t.Fatalf("%d progress reports in one minute", n)
	}
}

func TestRebuildLimitRestoredAfterRecovery(t *testing.T) {
	f := newHealthyCluster(t)
	set(f.volumes[0], "degraded", "status", "robustness")
	f.respond = func(c Command, line string) (Result, bool) {
		// Raised admission lets the volume rebuild after a few polls.
		if line == "kubectl -n longhorn-system get volumes.longhorn.io -o json" && f.settings[rebuildLimitSetting] == "2" && f.sleeps >= 3 {
			set(f.volumes[0], "healthy", "status", "robustness")
		}
		return Result{}, false
	}
	output := run(t, f, "reboot")
	requireOrder(t, f, `-p \{"value":"2"\}`, `-p \{"value":"0"\}`, `^kubectl cordon`)
	mustContain(t, output, "==> done")
	if f.settings[rebuildLimitSetting] != "0" {
		t.Fatalf("rebuild limit left at %q", f.settings[rebuildLimitSetting])
	}
}

func TestRebuildLimitRestoredOnInterrupt(t *testing.T) {
	f := newHealthyCluster(t)
	set(f.volumes[0], "degraded", "status", "robustness")
	o, output := newOperation(f, options(t, "reboot", "server-a"))
	ctx, cancel := context.WithCancel(context.Background())
	o.ctx = ctx
	o.sleep = func(ctx context.Context, d time.Duration) error {
		// The operator interrupts during the first wait.
		cancel()
		return errInterrupted
	}
	if status := o.Run(); status == 0 {
		t.Fatal("interrupted run passed")
	}
	mustContain(t, output.String(), "error: interrupted")
	if f.settings[rebuildLimitSetting] != "0" {
		t.Fatalf("rebuild limit left at %q", f.settings[rebuildLimitSetting])
	}
	// Only the restore may start after the interrupt.
	raised := f.callIndex(`-p \{"value":"2"\}`)
	for _, c := range f.calls[raised+1:] {
		if !strings.Contains(c.line, `-p {"value":"0"}`) && !strings.HasSuffix(c.line, "get volumes.longhorn.io -o json") {
			t.Fatalf("command after interrupt: %s", c.line)
		}
	}
}

func TestRebuildLimitRestoreFailureFailsRun(t *testing.T) {
	f := newHealthyCluster(t)
	set(f.volumes[0], "degraded", "status", "robustness")
	f.respond = func(c Command, line string) (Result, bool) {
		if strings.HasSuffix(line, `-p {"value":"0"}`) {
			return Result{Status: 1}, true
		}
		return Result{}, false
	}
	runFailure(t, f, "reboot", []string{"--settle-timeout", "0s"}, "error: failed to restore Longhorn replica rebuild admission to 0")
}

func TestBackupTarget(t *testing.T) {
	f := newHealthyCluster(t)
	f.backup = decode(t, `{"status": {"available": false, "conditions": [{"type": "Unavailable", "status": "True", "reason": "Unreachable"}]}}`)
	output := run(t, f, "reboot", "--check-only")
	mustContain(t, output, "Longhorn backup target is not available (Unreachable); continuing")
	runFailure(t, f, "reboot", []string{"--check-only", "--require-longhorn-backup-target"}, "Longhorn backup target is not available (Unreachable)")
}

func TestPDBBlockerStopsBeforeCordon(t *testing.T) {
	f := newHealthyCluster(t)
	set(f.pdbs[0], 0.0, "status", "disruptionsAllowed")
	runFailure(t, f, "reboot", nil, "default\tweb\tweb-1\t-", "PodDisruptionBudgets block eviction")
	mustNotCall(t, f, `^kubectl cordon`)
}

func TestPodSlotCapacityStopsBeforeCordon(t *testing.T) {
	f := newHealthyCluster(t)
	output := runFailure(t, f, "reboot", []string{"--min-free-pod-slots", "300"}, "remaining stable nodes do not have enough aggregate Pod slots")
	// server-b and server-c: 220 slots, one Pod scheduled, two displaced (the instance-manager counts, as in the Bash tool).
	mustContain(t, output, "server-b,server-c\t1\t2\t220\t217\tinsufficient")
	mustNotCall(t, f, `^kubectl cordon`)
}

// withDatabase adds a CloudNativePG cluster whose primary runs on server-a
// with a healthy replica on server-b.
func withDatabase(t *testing.T, f *fakeCluster) *fakeCluster {
	f.cnpg = true
	f.clusters = []map[string]any{decode(t, `{
	  "metadata": {"namespace": "default", "name": "db"},
	  "spec": {"instances": 2},
	  "status": {"readyInstances": 2, "currentPrimary": "db-1", "phase": "Cluster in healthy state",
	    "instancesStatus": {"healthy": ["db-1", "db-2"]}}
	}`)}
	f.pods = append(f.pods,
		readyPod(t, "default", "db-1", "server-a", "Cluster", "db", `{"cnpg.io/cluster": "db", "cnpg.io/podRole": "instance", "cnpg.io/instanceRole": "primary"}`),
		readyPod(t, "default", "db-2", "server-b", "Cluster", "db", `{"cnpg.io/cluster": "db", "cnpg.io/podRole": "instance", "cnpg.io/instanceRole": "replica"}`),
	)
	return f
}

func TestCNPGPrimarySwitchedBeforeCordon(t *testing.T) {
	f := withDatabase(t, newHealthyCluster(t))
	output := run(t, f, "reboot")
	mustContain(t, output, "switching default/db primary from db-1 to db-2 on server-b")
	requireOrder(t, f, `^kubectl cnpg promote -n default db db-2$`, `^kubectl cordon server-a$`)
}

// A fleet reboot marks the nodes it has still to reboot; a primary moves to
// a replica on a node without the mark, though another comes first by name.
func TestCNPGPrefersRebootedNode(t *testing.T) {
	f := withDatabase(t, newHealthyCluster(t))
	set(f.clusters[0], float64(3), "spec", "instances")
	set(f.clusters[0], float64(3), "status", "readyInstances")
	set(f.clusters[0], []any{"db-1", "db-2", "db-3"}, "status", "instancesStatus", "healthy")
	f.pods = append(f.pods, readyPod(t, "default", "db-3", "server-c", "Cluster", "db",
		`{"cnpg.io/cluster": "db", "cnpg.io/podRole": "instance", "cnpg.io/instanceRole": "replica"}`))
	set(f.nodes[1], []any{map[string]any{"key": RebootPendingTaint, "value": "true", "effect": "PreferNoSchedule"}}, "spec", "taints")
	output := run(t, f, "reboot")
	mustContain(t, output, "switching default/db primary from db-1 to db-3 on server-c")
	requireOrder(t, f, `^kubectl cnpg promote -n default db db-3$`, `^kubectl cordon server-a$`)
}

// A replica CloudNativePG reports healthy but that no longer streams stops
// maintenance before anything changes: promoting it would roll the database
// back, and draining the other replica's node would leave none.
func TestCNPGReplicaNotStreamingStopsBeforeSwitchover(t *testing.T) {
	for _, state := range []string{"", "startup|-1"} {
		f := withDatabase(t, newHealthyCluster(t))
		f.standbys = map[string]string{"db-2": state}
		runFailure(t, f, "reboot", []string{"--settle-timeout", "1m"},
			"default/db\tready=2/2\tstreaming=0/1\tphase=Cluster in healthy state",
			"only a rebuild (kubectl cnpg destroy) brings it back")
		mustNotCall(t, f, `^kubectl (cnpg|cordon)`)
	}
}

// A streaming replica more than a segment behind is passed over, even on a
// rebooted node.
func TestCNPGSkipsReplicaThatIsNotCaughtUp(t *testing.T) {
	f := withDatabase(t, newHealthyCluster(t))
	set(f.clusters[0], float64(3), "spec", "instances")
	set(f.clusters[0], float64(3), "status", "readyInstances")
	set(f.clusters[0], []any{"db-1", "db-2", "db-3"}, "status", "instancesStatus", "healthy")
	f.pods = append(f.pods, readyPod(t, "default", "db-3", "server-c", "Cluster", "db",
		`{"cnpg.io/cluster": "db", "cnpg.io/podRole": "instance", "cnpg.io/instanceRole": "replica"}`))
	set(f.nodes[1], []any{map[string]any{"key": RebootPendingTaint, "value": "true", "effect": "PreferNoSchedule"}}, "spec", "taints")
	f.standbys = map[string]string{"db-3": "streaming|33554432"}
	output := run(t, f, "reboot")
	mustContain(t, output, "switching default/db primary from db-1 to db-2 on server-b")
	mustNotCall(t, f, `^kubectl cnpg promote -n default db db-3$`)
}

func TestCNPGWithoutCaughtUpReplicaStopsBeforeCordon(t *testing.T) {
	f := withDatabase(t, newHealthyCluster(t))
	f.standbys = map[string]string{"db-2": "streaming|33554432"}
	runFailure(t, f, "reboot", []string{"--settle-timeout", "1m"}, "default\tdb\tdb-1\tunavailable\tunavailable",
		"waiting for a streaming, caught-up replica of default/db",
		"has no healthy, streaming and caught-up replica on a surviving stable node (default/db)")
	mustNotCall(t, f, `^kubectl (cnpg|cordon)`)
}

// A replica that trails briefly under load is waited for, not refused.
func TestCNPGWaitsForReplicaToCatchUp(t *testing.T) {
	f := withDatabase(t, newHealthyCluster(t))
	f.standbys = map[string]string{"db-2": "streaming|33554432"}
	asked := 0
	f.respond = func(c Command, line string) (Result, bool) {
		if strings.Contains(line, " exec db-1 ") {
			asked++
			if asked == 3 {
				f.standbys = nil
			}
		}
		return Result{}, false
	}
	output := run(t, f, "reboot")
	mustContain(t, output, "waiting for a streaming, caught-up replica of default/db")
	mustContain(t, output, "switching default/db primary from db-1 to db-2 on server-b")
}

// A primary that is not Ready is not asked, and is reported as unknown
// rather than as replicas that stopped streaming.
func TestCNPGPrimaryNotReadyIsUnknown(t *testing.T) {
	f := withDatabase(t, newHealthyCluster(t))
	set(f.clusters[0], "db-2", "status", "currentPrimary")
	set(f.pods[len(f.pods)-1], "Pending", "status", "phase")
	output := runFailure(t, f, "reboot", []string{"--settle-timeout", "1m"},
		"streaming=unknown (primary db-2 is not Running and Ready)/1")
	if strings.Contains(output, "kubectl cnpg destroy") {
		t.Fatalf("a primary that was not asked got the rebuild hint:\n%s", output)
	}
}

// A primary that cannot be asked is reported as such, without the hint to
// rebuild replicas that may well be streaming.
func TestCNPGUnansweredPrimaryIsNotAStoppedReplica(t *testing.T) {
	f := withDatabase(t, newHealthyCluster(t))
	f.respond = func(c Command, line string) (Result, bool) {
		if strings.Contains(line, " exec db-1 ") {
			return Result{Status: 1, Stderr: "Error from server (Forbidden): pods \"db-1\" is forbidden\nmore\n"}, true
		}
		return Result{}, false
	}
	output := runFailure(t, f, "reboot", []string{"--settle-timeout", "1m"},
		"default/db\tready=2/2\tstreaming=unknown (could not ask db-1: Error from server (Forbidden): pods \"db-1\" is forbidden)/1",
		"error: CloudNativePG clusters did not return to full health\n")
	if strings.Contains(output, "kubectl cnpg destroy") {
		t.Fatalf("a primary that was not asked got the rebuild hint:\n%s", output)
	}
	mustNotCall(t, f, `^kubectl (cnpg|cordon)`)
}

// planSwitchovers passes over a replica the primary does not report, even
// when CloudNativePG lists it healthy.
func TestPlanSwitchoversNeedsStandby(t *testing.T) {
	f := withDatabase(t, newHealthyCluster(t))
	var pods []pod
	if err := jsonUnmarshal(serve(f.pods).Stdout, &pods); err != nil {
		t.Fatal(err)
	}
	var clusters []cnpgCluster
	if err := jsonUnmarshal(serve(f.clusters).Stdout, &clusters); err != nil {
		t.Fatal(err)
	}
	var nodes []node
	if err := jsonUnmarshal(serve(f.nodes).Stdout, &nodes); err != nil {
		t.Fatal(err)
	}
	plan := planSwitchovers("server-a", clusters, pods, nodes, nil)
	if len(plan) != 1 || plan[0].candidate != "" {
		t.Fatalf("got %+v", plan)
	}
	plan = planSwitchovers("server-a", clusters, pods, nodes, map[string]standby{"default/db-2": {state: "streaming"}})
	if len(plan) != 1 || plan[0].candidate != "db-2" {
		t.Fatalf("got %+v", plan)
	}
}

// After maintenance every replica must stream again: a Ready replica that
// does not fails the run instead of being left for the next node's
// switchover.
func TestCNPGHealthWaitsForStreamingReplicas(t *testing.T) {
	f := withDatabase(t, newHealthyCluster(t))
	set(f.clusters[0], "db-2", "status", "currentPrimary")
	// The drain moves the replica; once moved, it does not stream.
	f.standbys = map[string]string{"db-1-moved": "startup|-1"}
	output := runFailure(t, f, "reboot", []string{"--settle-timeout", "1m"},
		"default/db\tready=2/2\tstreaming=0/1\tphase=Cluster in healthy state",
		"only a rebuild (kubectl cnpg destroy) brings it back")
	uncordoned, reported := strings.Index(output, "node/server-a uncordoned"), strings.Index(output, "streaming=0/1")
	if uncordoned < 0 || reported < 0 || uncordoned > reported {
		t.Fatalf("the replica was checked only before the node returned:\n%s", output)
	}
}

// One cluster that cannot get a candidate stops the run at once, even while
// another waits for a lagging replica.
func TestCNPGDoesNotWaitWhenOneClusterCannotCatchUp(t *testing.T) {
	f := withDatabase(t, newHealthyCluster(t))
	f.standbys = map[string]string{"db-2": "streaming|33554432"}
	f.clusters = append(f.clusters, decode(t, `{
	  "metadata": {"namespace": "default", "name": "solo"},
	  "spec": {"instances": 1},
	  "status": {"readyInstances": 1, "currentPrimary": "solo-1", "phase": "Cluster in healthy state",
	    "instancesStatus": {"healthy": ["solo-1"]}}
	}`))
	f.pods = append(f.pods, readyPod(t, "default", "solo-1", "server-a", "Cluster", "solo",
		`{"cnpg.io/cluster": "solo", "cnpg.io/podRole": "instance", "cnpg.io/instanceRole": "primary"}`))
	runFailure(t, f, "reboot", nil, "has no healthy, streaming and caught-up replica on a surviving stable node (default/db; default/solo)")
	if f.sleeps != 0 {
		t.Fatalf("waited %d times though default/solo cannot get a candidate", f.sleeps)
	}
}

func TestCNPGWithoutCandidateStopsBeforeCordon(t *testing.T) {
	f := withDatabase(t, newHealthyCluster(t))
	set(f.clusters[0], []any{"db-1"}, "status", "instancesStatus", "healthy")
	defer func() {
		// Nothing can bring a candidate, so the run does not wait.
		if f.sleeps != 0 {
			t.Fatalf("waited %d times for a candidate that cannot come", f.sleeps)
		}
	}()
	runFailure(t, f, "reboot", nil, "default\tdb\tdb-1\tunavailable\tunavailable",
		"CloudNativePG primary on server-a has no healthy, streaming and caught-up replica on a surviving stable node")
	mustNotCall(t, f, `^kubectl (cnpg|cordon)`)
}

func TestCNPGPromoteNeedsPlugin(t *testing.T) {
	f := withDatabase(t, newHealthyCluster(t))
	f.respond = func(c Command, line string) (Result, bool) {
		if line == "command -v kubectl-cnpg" {
			return Result{Status: 1}, true
		}
		return Result{}, false
	}
	runFailure(t, f, "reboot", nil, "missing required command: kubectl-cnpg")
	mustNotCall(t, f, `^kubectl (cnpg|cordon)`)
}

func TestStorageDetachGatesLeaveNodeCordoned(t *testing.T) {
	cases := []struct {
		name string
		edit func(t *testing.T, f *fakeCluster)
		want string
	}{
		{"volume still attached", func(t *testing.T, f *fakeCluster) {
			f.volumes = append(f.volumes, decode(t, `{"metadata": {"name": "pvc-2"}, "spec": {"numberOfReplicas": 3, "nodeID": "server-a"},
			  "status": {"state": "detaching", "robustness": "healthy", "currentNodeID": "server-a"}}`))
		}, "Longhorn volumes remain attached to server-a"},
		{"VolumeAttachment remains", func(t *testing.T, f *fakeCluster) {
			f.attachments = []map[string]any{decode(t, `{"metadata": {"name": "csi-1"}, "spec": {"nodeName": "server-a",
			  "attacher": "driver.longhorn.io", "source": {"persistentVolumeName": "pvc-1"}}, "status": {"attached": true}}`)}
		}, "Longhorn VolumeAttachments remain on server-a"},
		{"residual host storage", func(t *testing.T, f *fakeCluster) {
			f.storageExit = 1
		}, "residual Longhorn storage is still active on root@server-a"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newHealthyCluster(t)
			c.edit(t, f)
			runFailure(t, f, "reboot", []string{"--drain-timeout", "0s"}, c.want)
			if get(f.targetNode(), "spec", "unschedulable") != true {
				t.Fatal("node not left cordoned")
			}
			mustNotCall(t, f, regexp.QuoteMeta(rebootCommand))
		})
	}
}

// The host check reconciles only validated, detached sessions, and removes
// the Open-iSCSI field newer releases reject before logging out.
func TestStorageDetachScript(t *testing.T) {
	for _, want := range []string{
		"grep -E '^/var/lib/kubelet/plugins/kubernetes.io/csi/driver\\.longhorn\\.io/.+/globalmount$'",
		`if [[ "$cleanup_sessions" != true ]]; then`,
		`sed -i '/^node\.session\.conn_reopen_log_freq[[:space:]]*=/d' "$record"`,
		`iscsiadm -m node -T "$target" -p "$portal_address" --logout`,
		`iscsiadm -m node -T "$target" -p "$portal_address" --op=delete`,
	} {
		if !strings.Contains(storageDetachScript, want) {
			t.Errorf("storage script lacks %q", want)
		}
	}
}

func TestFenceStopFailureBlocksPowerAction(t *testing.T) {
	for _, action := range []string{"reboot", "off"} {
		f := withAgent(t, newHealthyCluster(t))
		f.stopExit = 1
		runFailure(t, f, action, nil, "server-a: failed to stop node-self-fence; server-a remains cordoned")
		mustNotCall(t, f, regexp.QuoteMeta(rebootCommand))
		mustNotCall(t, f, regexp.QuoteMeta(powerOffCommand))
		if get(f.targetNode(), "spec", "unschedulable") != true {
			t.Fatal("node not left cordoned")
		}
	}
}

func TestRebootWithAgent(t *testing.T) {
	f := withAgent(t, newHealthyCluster(t))
	output := run(t, f, "reboot")
	requireOrder(t, f, `^kubectl cordon`, regexp.QuoteMeta(fenceStopScript), regexp.QuoteMeta(rebootCommand), `^kubectl uncordon`)
	mustContain(t, output, "server-a self-fence agent recovered in enforce mode with a fresh heartbeat")
	if strings.Contains(output, "is still stopped") {
		t.Fatal("warned about a stopped agent after a verified reboot")
	}
}

// A failure after the agent stop and before the power action is verified
// tells the operator to restart the agent.
func TestStoppedAgentReportedOnExit(t *testing.T) {
	f := withAgent(t, newHealthyCluster(t))
	f.rebootExit = 1
	runFailure(t, f, "reboot", nil, "remote reboot command failed",
		"node-self-fence is still stopped on server-a; start it with systemctl start node-self-fence before retrying maintenance")
}

func TestReturnedAgentFailureLeavesNodeCordoned(t *testing.T) {
	f := withAgent(t, newHealthyCluster(t))
	f.respond = func(c Command, line string) (Result, bool) {
		if c.Stdin == fenceStatusScript && f.callIndex(regexp.QuoteMeta(rebootCommand)) >= 0 {
			set(f.status, "grace", "states", 0, "classification")
		}
		return Result{}, false
	}
	output := runFailure(t, f, "reboot", []string{"--fence-timeout", "1m"}, "own agent is not healthy", "fence recovery timed out after 1m; server-a remains cordoned")
	assertStillCordoned(t, f, output)
}

func TestAgentModeAndPeerStateBeforePowerAction(t *testing.T) {
	f := withAgent(t, newHealthyCluster(t))
	f.respond = func(c Command, line string) (Result, bool) {
		// A peer fences while the target drains.
		if c.Stdin == fenceStatusScript && f.callIndex(`^kubectl drain`) >= 0 {
			set(f.status, true, "states", 2, "fenced")
		}
		return Result{}, false
	}
	runFailure(t, f, "reboot", nil, "server-c: fenced or missing fenced state")
	mustNotCall(t, f, regexp.QuoteMeta(fenceStopScript))
	mustNotCall(t, f, regexp.QuoteMeta(rebootCommand))
}

func TestUnhealthyPodsBlockPreflight(t *testing.T) {
	f := newHealthyCluster(t)
	p := readyPod(t, "default", "crash", "server-b", "ReplicaSet", "crash-1", `{}`)
	set(p, []any{map[string]any{"ready": false, "state": map[string]any{"waiting": map[string]any{"reason": "CrashLoopBackOff"}}}}, "status", "containerStatuses")
	job := readyPod(t, "default", "job-1", "server-b", "Job", "job", `{}`)
	set(job, "Failed", "status", "phase")
	f.pods = append(f.pods, p, job)
	output := runFailure(t, f, "reboot", []string{"--settle-timeout", "0s"}, "default/crash: Running", "unhealthy pods remained after waiting")
	if strings.Contains(output, "job-1") {
		t.Fatal("a failed Job Pod was reported")
	}
}

func TestCNPGSurvivabilityFailures(t *testing.T) {
	f := withDatabase(t, newHealthyCluster(t))
	pods := []pod{}
	for _, p := range f.pods {
		var decoded pod
		data := serve(p).Stdout
		if err := jsonUnmarshal(data, &decoded); err != nil {
			t.Fatal(err)
		}
		pods = append(pods, decoded)
	}
	var clusters []cnpgCluster
	if err := jsonUnmarshal(serve(f.clusters).Stdout, &clusters); err != nil {
		t.Fatal(err)
	}
	bad := cnpgSurvivabilityFailures("server-a", clusters, pods, replication{})
	if len(bad) != 1 || bad[0] != "default/db\tready=2/2\tminimum=1\tstreaming=0\tprimary=db-1\tprimaryNode=server-a" {
		t.Fatalf("got %q", bad)
	}
	clusters[0].Status.CurrentPrimary = "db-2"
	if bad := cnpgSurvivabilityFailures("server-a", clusters, pods, replication{}); len(bad) != 0 {
		t.Fatalf("got %q", bad)
	}
	clusters[0].Status.CurrentPrimary = "db-9"
	if bad := cnpgSurvivabilityFailures("server-a", clusters, pods, replication{}); !slices.Contains(bad, "default/db\tready=2/2\tminimum=1\tstreaming=0\tprimary=db-9\tprimaryNode=missing") {
		t.Fatalf("got %q", bad)
	}

	// With three instances, one node down needs the primary and one
	// streaming replica away from the node; a Ready replica that does not
	// stream does not count.
	clusters[0].Spec.Instances = new(int)
	*clusters[0].Spec.Instances = 3
	clusters[0].Status.ReadyInstances = 3
	clusters[0].Status.CurrentPrimary = "db-2"
	pods = append(pods, pod{})
	pods[len(pods)-1].Metadata.Namespace = "default"
	pods[len(pods)-1].Metadata.Name = "db-3"
	pods[len(pods)-1].Metadata.Labels = map[string]string{"cnpg.io/cluster": "db"}
	pods[len(pods)-1].Spec.NodeName = "server-c"
	streaming := replication{standbys: map[string]standby{"default/db-1": {state: "streaming"}, "default/db-3": {state: "streaming"}}}
	if bad := cnpgSurvivabilityFailures("server-a", clusters, pods, streaming); len(bad) != 0 {
		t.Fatalf("got %q", bad)
	}
	stopped := replication{standbys: map[string]standby{"default/db-1": {state: "streaming"}, "default/db-3": {state: "startup"}}}
	if bad := cnpgSurvivabilityFailures("server-a", clusters, pods, stopped); len(bad) != 1 ||
		bad[0] != "default/db\tready=3/3\tminimum=2\tstreaming=0\tprimary=db-2\tprimaryNode=server-b" {
		t.Fatalf("got %q", bad)
	}
	unanswered := replication{unanswered: map[string]string{"default/db": "could not ask db-2: forbidden"}}
	if bad := cnpgSurvivabilityFailures("server-a", clusters, pods, unanswered); len(bad) != 1 ||
		bad[0] != "default/db\tready=3/3\tminimum=2\tstreaming=unknown (could not ask db-2: forbidden)\tprimary=db-2\tprimaryNode=server-b" {
		t.Fatalf("got %q", bad)
	}
	// Two instances keep no replica with one node down, but a primary that
	// could not be asked still fails.
	*clusters[0].Spec.Instances = 2
	clusters[0].Status.ReadyInstances = 2
	if bad := cnpgSurvivabilityFailures("server-a", clusters, pods[:len(pods)-1], replication{}); len(bad) != 0 {
		t.Fatalf("got %q", bad)
	}
	if bad := cnpgSurvivabilityFailures("server-a", clusters, pods[:len(pods)-1], unanswered); len(bad) != 1 {
		t.Fatalf("got %q", bad)
	}
}

func TestParseStandbys(t *testing.T) {
	got := parseStandbys("db", "db-2|streaming|0\ndb-3|catchup|33554432\ndb-4|streaming|\n\nnoise\n")
	want := map[string]standby{
		"db/db-2": {state: "streaming", lag: 0},
		"db/db-3": {state: "catchup", lag: 33554432},
		"db/db-4": {state: "streaming", lag: -1},
	}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for name, s := range want {
		if got[name] != s {
			t.Fatalf("%s: got %+v, want %+v", name, got[name], s)
		}
	}
	if !got["db/db-2"].caughtUp() || got["db/db-3"].caughtUp() || got["db/db-4"].caughtUp() {
		t.Fatalf("caughtUp wrong: %v", got)
	}
	if (standby{state: "streaming", lag: maxSwitchoverLag + 1}).caughtUp() {
		t.Fatal("a replica more than one segment behind is caught up")
	}
}

func TestLonghornSurvivabilityFailures(t *testing.T) {
	f := newHealthyCluster(t)
	var volumes []longhornVolume
	var replicas []longhornReplica
	if err := jsonUnmarshal(serve(f.volumes).Stdout, &volumes); err != nil {
		t.Fatal(err)
	}
	if err := jsonUnmarshal(serve(f.replicas).Stdout, &replicas); err != nil {
		t.Fatal(err)
	}
	if bad := survivabilityFailures("server-a", volumes, replicas); len(bad) != 0 {
		t.Fatalf("got %q", bad)
	}
	// With server-c's replica failed, only server-b survives.
	replicas[2].Spec.FailedAt = "2026-01-01T00:00:00Z"
	bad := survivabilityFailures("server-a", volumes, replicas)
	if len(bad) != 1 || bad[0] != "pvc-1\tdefault/data\tstate=attached\trobustness=healthy\tattachment=server-b\tsurvivingNodes=server-b" {
		t.Fatalf("got %q", bad)
	}
	// A volume attached to the target fails even with healthy replicas.
	replicas[2].Spec.FailedAt = ""
	volumes[0].Status.CurrentNodeID = "server-a"
	if bad := survivabilityFailures("server-a", volumes, replicas); len(bad) != 1 {
		t.Fatalf("got %q", bad)
	}
}

// requireContext checks that every kubectl call carried the context.
func requireContext(t *testing.T, f *fakeCluster, want string) {
	t.Helper()
	kubectl := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c.line, "kubectl ") {
			kubectl++
		}
	}
	if kubectl == 0 || len(f.contexts) != kubectl {
		t.Fatalf("%d of %d kubectl calls carried a context:\n%s", len(f.contexts), kubectl, f.callLog())
	}
	for _, got := range f.contexts {
		if got != "--context="+want {
			t.Fatalf("kubectl call with %s, want --context=%s", got, want)
		}
	}
}

// --context reaches every kubectl call: the database switchover through the
// cnpg plugin, which takes it after its name, and the cleanup that restores
// Longhorn's rebuild admission after a failed gate.
func TestContextReachesEveryKubectlCall(t *testing.T) {
	f := withDatabase(t, newHealthyCluster(t))
	run(t, f, "reboot", "--context", "lab")
	requireOrder(t, f, `^kubectl cnpg promote -n default db db-2$`, `^kubectl cordon server-a$`)
	requireContext(t, f, "lab")

	f = newHealthyCluster(t)
	set(f.volumes[0], "degraded", "status", "robustness")
	runFailure(t, f, "reboot", []string{"--context", "lab", "--settle-timeout", "1m"}, "Longhorn volumes are not healthy")
	mustContain(t, f.callLog(), `"value":"0"`)
	requireContext(t, f, "lab")

	f = newHealthyCluster(t)
	run(t, f, "reboot")
	if len(f.contexts) != 0 {
		t.Fatalf("calls carried %v without --context", f.contexts)
	}
}
