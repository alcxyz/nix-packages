// Package k8snode cordons, drains, powers off or reboots, and returns a
// Kubernetes node behind the cluster, storage, database and self-fence gates
// of nix-config ADR-0068 and ADR-0082. It is a port of the k8s-node-reboot
// Bash tool (kreboot, koff, kon) at behavioural parity.
package k8snode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"strings"
	"time"
)

// errInterrupted stops a wait once the operator interrupted the run.
var errInterrupted = errors.New("interrupted")

// Operation is one run of an action against one node.
type Operation struct {
	opts   Options
	runner *Runner
	ctx    context.Context
	stdout io.Writer
	stderr io.Writer

	// now and sleep are the clock; tests replace them.
	now   func() time.Time
	sleep func(context.Context, time.Duration) error
	// poll is the pause between checks of a condition that is waited for.
	poll time.Duration
	// fenceStopTimeout bounds the wait for the agent to publish mode=stopped.
	fenceStopTimeout time.Duration

	// workloads are the controllers displaced by the drain, or reconstructed
	// from Pending Pods, that must settle once the node returns.
	workloads []workload
	// bootID is the target's boot ID before the reboot.
	bootID string
	// rebuildLimitOriginal is the Longhorn rebuild admission to restore, set
	// while it is temporarily raised.
	rebuildLimitOriginal string

	fence fenceState
}

// New prepares an operation. Commands run through runner and stop starting
// once ctx is done.
func New(ctx context.Context, opts Options, runner *Runner) *Operation {
	return &Operation{
		opts:             opts,
		runner:           runner,
		ctx:              ctx,
		stdout:           runner.Stdout,
		stderr:           runner.Stderr,
		now:              time.Now,
		sleep:            sleepContext,
		poll:             5 * time.Second,
		fenceStopTimeout: 60 * time.Second,
	}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return errInterrupted
	case <-timer.C:
		return nil
	}
}

// Run performs the action and returns the exit status. On every exit path,
// including a failed gate or an interrupt, it restores a temporarily raised
// Longhorn rebuild limit and reports a self-fence agent it left stopped.
func (o *Operation) Run() (status int) {
	defer func() {
		if !o.cleanup() && status == 0 {
			status = 1
		}
	}()
	err := o.run()
	if o.ctx.Err() != nil {
		// Keep a failed step's own diagnosis, such as a node left cordoned.
		if err != nil && !errors.Is(err, errInterrupted) {
			fmt.Fprintf(o.stderr, "error: interrupted: %v\n", err)
		}
		fmt.Fprintf(o.stderr, "error: interrupted; %s may need inspection before retrying maintenance\n", o.opts.Node)
		return 1
	}
	if err != nil {
		fmt.Fprintf(o.stderr, "error: %v\n", err)
		return 1
	}
	return 0
}

func (o *Operation) run() error {
	if o.opts.CheckOnly {
		return o.checkOnly()
	}
	switch o.opts.Action {
	case "reboot":
		return o.reboot()
	case "off":
		return o.powerOff()
	case "on":
		return o.powerOn()
	}
	return fmt.Errorf("unsupported action: %s", o.opts.Action)
}

// cleanup is the Bash tool's exit trap. It reports whether the rebuild
// limit, if raised, was restored.
func (o *Operation) cleanup() bool {
	if o.fence.agentStopped {
		o.log("node-self-fence is still stopped on %s; start it with systemctl start node-self-fence before retrying maintenance", o.opts.Node)
	}
	if err := o.restoreRebuildLimit(); err != nil {
		fmt.Fprintf(o.stderr, "error: %v\n", err)
		return false
	}
	return true
}

func (o *Operation) log(format string, args ...any) {
	fmt.Fprintf(o.stdout, "==> "+format+"\n", args...)
}

// report prints diagnostic lines, such as the objects a gate is waiting for,
// on standard error.
func (o *Operation) report(lines []string) {
	for _, line := range lines {
		fmt.Fprintln(o.stderr, line)
	}
}

// pause waits one poll interval, or fails once the run is interrupted.
func (o *Operation) pause() error {
	return o.sleep(o.ctx, o.poll)
}

// expired reports whether the deadline has passed. Waits check their
// condition before the deadline, so a zero timeout still checks once.
func (o *Operation) expired(deadline time.Time) bool {
	return !o.now().Before(deadline)
}

func (o *Operation) reboot() error {
	if err := o.prepareNodeForDisruption(); err != nil {
		return err
	}
	if err := o.stopFenceAgent(); err != nil {
		return err
	}
	if err := o.rebootHost(); err != nil {
		return err
	}
	if err := o.waitForSSHDown(); err != nil {
		return err
	}
	// The power action took effect; the agent starts again with the host.
	o.fence.agentStopped = false
	if err := o.verifySurvivorNodeNetworkPaths(); err != nil {
		return err
	}
	if err := o.waitForSSHUp(); err != nil {
		return err
	}
	if err := o.verifyNewBoot(); err != nil {
		return err
	}
	if err := o.waitForNodeReady(); err != nil {
		return err
	}
	if err := o.waitForReturnedNodeNetwork(); err != nil {
		return err
	}
	if err := o.waitForLonghornSurvivability(); err != nil {
		return err
	}
	if err := o.waitForCNPGSurvivability(); err != nil {
		return err
	}

	if o.opts.LeaveCordoned {
		o.log("leaving %s cordoned", o.opts.Node)
	} else {
		if err := o.waitForReturnedFenceAgent(); err != nil {
			return err
		}
		if err := o.uncordon(); err != nil {
			return err
		}
		if err := o.settleCluster(); err != nil {
			return err
		}
	}
	o.log("done")
	return nil
}

func (o *Operation) powerOff() error {
	if err := o.prepareNodeForDisruption(); err != nil {
		return err
	}
	if err := o.stopFenceAgent(); err != nil {
		return err
	}
	if err := o.powerOffHost(); err != nil {
		return err
	}
	if err := o.waitForSSHDown(); err != nil {
		return err
	}
	o.fence.agentStopped = false
	if err := o.verifySurvivorNodeNetworkPaths(); err != nil {
		return err
	}
	o.log("%s is powered off and remains cordoned", o.opts.Node)
	return nil
}

// powerOn finalizes maintenance after the node is powered back on.
func (o *Operation) powerOn() error {
	node := o.opts.Node
	o.log("checking Kubernetes node %s", node)
	if err := o.checkNodeExists(); err != nil {
		return err
	}
	if err := o.waitForSSHUp(); err != nil {
		return err
	}
	if err := o.initializeFenceGate(); err != nil {
		return err
	}
	if err := o.waitForNodeReady(); err != nil {
		return err
	}
	if err := o.verifyNodeIsCordoned(); err != nil {
		return err
	}
	if err := o.waitForReturnedNodeNetwork(); err != nil {
		return err
	}
	if err := o.waitForLonghornSurvivability(); err != nil {
		return err
	}
	if err := o.waitForCNPGSurvivability(); err != nil {
		return err
	}
	workloads, err := o.collectTargetPinnedPendingWorkloads()
	if err != nil {
		return err
	}
	o.workloads = workloads

	if o.fence.hasAgent {
		// This process has no stop timestamp from a previous one. A unique
		// annotation write establishes an API-clock baseline even after an
		// unplanned outage.
		baselineValue := fmt.Sprintf("%d-%d-%d", o.now().UnixNano(), os.Getpid(), rand.IntN(32768))
		o.log("establishing self-fence heartbeat baseline for %s", node)
		write := o.kubectl(Command{Argv: []string{"annotate", "node", node,
			"fence.alc.xyz/maintenance-baseline=" + baselineValue,
			"--overwrite", "--field-manager=k8s-node-reboot", "--request-timeout=5s"}})
		if write.Status != 0 {
			return fmt.Errorf("%s: could not establish self-fence heartbeat baseline (annotation write failed); %s remains cordoned", node, node)
		}
		baseline, ok := o.managerTime("k8s-node-reboot")
		remove := o.kubectl(Command{Argv: []string{"annotate", "node", node,
			"fence.alc.xyz/maintenance-baseline-", "--request-timeout=5s"}})
		if remove.Status != 0 {
			return fmt.Errorf("%s: could not establish self-fence heartbeat baseline (annotation cleanup failed); %s remains cordoned", node, node)
		}
		if !ok {
			return fmt.Errorf("%s: could not establish self-fence heartbeat baseline (API-server timestamp unavailable); %s remains cordoned", node, node)
		}
		o.fence.baseline = baseline
		o.fence.hasBaseline = true
	}

	if err := o.waitForReturnedFenceAgent(); err != nil {
		return err
	}
	if err := o.uncordon(); err != nil {
		return err
	}
	if err := o.waitForWorkloads(); err != nil {
		return err
	}
	if err := o.waitForLonghornHealth(); err != nil {
		return err
	}
	if err := o.waitForCNPGHealth(); err != nil {
		return err
	}
	if err := o.waitForNoBadPods(); err != nil {
		return err
	}
	o.log("done")
	return nil
}

// checkOnly runs the fresh or resumed preflight without changing cluster or
// host state.
func (o *Operation) checkOnly() error {
	node := o.opts.Node
	o.log("checking Kubernetes node %s without changing cluster or host state", node)
	if err := o.checkNodeAccess(); err != nil {
		return err
	}

	if o.opts.ResumeMaintenance {
		if err := o.verifyResumedMaintenanceState(); err != nil {
			return err
		}
	} else {
		cordoned, err := o.nodeIsCordoned()
		if err != nil {
			return err
		}
		if cordoned {
			return fmt.Errorf("%s is already cordoned; use --resume-maintenance for maintenance-state checks", node)
		}
		if err := o.preflightHealth(); err != nil {
			return err
		}
		o.log("planning CloudNativePG primary switchovers away from %s", node)
		if _, err := o.planCNPGSwitchovers(); err != nil {
			return err
		}
		if err := o.checkPDBEvictionBlockers(true); err != nil {
			return err
		}
		if err := o.checkStableNodePodSlots(); err != nil {
			return err
		}
	}
	o.log("checks passed; no changes were made")
	return nil
}

// checkNodeAccess is the start of every reboot, off and check-only run: the
// node exists, the host grants root, the self-fence preflight passes and
// every node's network path is sound.
func (o *Operation) checkNodeAccess() error {
	if err := o.checkNodeExists(); err != nil {
		return err
	}
	if err := o.verifyRemotePrivilege(); err != nil {
		return err
	}
	if err := o.verifyFencePreflight(); err != nil {
		return err
	}
	return o.verifyAllNodeNetworkPaths("")
}

// preflightHealth is the cluster health a fresh maintenance starts from.
func (o *Operation) preflightHealth() error {
	if err := o.waitForLonghornHealth(); err != nil {
		return err
	}
	if err := o.waitForCNPGHealth(); err != nil {
		return err
	}
	return o.waitForNoBadPods()
}

func (o *Operation) prepareNodeForDisruption() error {
	node := o.opts.Node
	o.log("checking Kubernetes node %s", node)
	if err := o.checkNodeAccess(); err != nil {
		return err
	}

	if o.opts.ResumeMaintenance {
		if err := o.verifyResumedMaintenanceState(); err != nil {
			return err
		}
		workloads, err := o.collectTargetPinnedPendingWorkloads()
		if err != nil {
			return err
		}
		o.workloads = workloads
		return nil
	}

	cordoned, err := o.nodeIsCordoned()
	if err != nil {
		return err
	}
	if cordoned {
		return fmt.Errorf("%s is already cordoned; use --resume-maintenance for another power cycle", node)
	}

	o.log("running preflight cluster health checks")
	if err := o.preflightHealth(); err != nil {
		return err
	}
	o.log("planning CloudNativePG primary switchovers away from %s", node)
	if _, err := o.planCNPGSwitchovers(); err != nil {
		return err
	}
	if err := o.checkPDBEvictionBlockers(true); err != nil {
		return err
	}
	if err := o.checkStableNodePodSlots(); err != nil {
		return err
	}
	if err := o.switchCNPGPrimariesOffNode(); err != nil {
		return err
	}
	if err := o.checkPDBEvictionBlockers(false); err != nil {
		return err
	}

	workloads, err := o.collectDisplacedWorkloads()
	if err != nil {
		return err
	}
	o.workloads = workloads

	o.log("cordoning %s", node)
	if r := o.kubectl(Command{Argv: []string{"cordon", node}, Passthrough: true}); r.Status != 0 {
		return fmt.Errorf("could not cordon %s", node)
	}

	if o.opts.SkipDrain {
		o.log("skipping drain by request")
	} else {
		argv := []string{"drain", node,
			"--ignore-daemonsets",
			"--delete-emptydir-data",
			"--pod-selector=longhorn.io/component!=instance-manager",
			"--timeout=" + o.opts.DrainTimeout,
		}
		if o.opts.ForceDrain {
			argv = append(argv, "--force")
		}
		if o.opts.BypassPDB {
			argv = append(argv, "--disable-eviction")
		}
		o.log("draining %s", node)
		if r := o.kubectl(Command{Argv: argv, Passthrough: true}); r.Status != 0 {
			return fmt.Errorf("drain failed; node disruption was not started and %s remains cordoned", node)
		}
		if err := o.waitForDisplacedWorkloadSurvivability(); err != nil {
			return err
		}
		if err := o.waitForNoBadPods(); err != nil {
			return err
		}
	}

	if err := o.waitForNodeStorageDetach(); err != nil {
		return err
	}
	if err := o.waitForLonghornSurvivability(); err != nil {
		return err
	}
	return o.waitForCNPGSurvivability()
}

func (o *Operation) verifyResumedMaintenanceState() error {
	o.log("validating resumed maintenance state for %s", o.opts.Node)
	if err := o.verifyNodeIsCordoned(); err != nil {
		return err
	}
	if err := o.verifyNodeHasOnlyMaintenancePods(); err != nil {
		return err
	}
	if err := o.waitForNodeStorageDetach(); err != nil {
		return err
	}
	if err := o.waitForLonghornSurvivability(); err != nil {
		return err
	}
	if err := o.waitForCNPGSurvivability(); err != nil {
		return err
	}
	return o.waitForNoBadPods()
}

// settleCluster waits for the cluster to recover after the node returned to
// service.
func (o *Operation) settleCluster() error {
	if err := o.waitForWorkloads(); err != nil {
		return err
	}
	if o.opts.Action == "off" {
		if err := o.warnForLonghornHealth(); err != nil {
			return err
		}
	} else if err := o.waitForLonghornHealth(); err != nil {
		return err
	}
	if err := o.waitForCNPGHealth(); err != nil {
		return err
	}
	return o.waitForNoBadPods()
}

func (o *Operation) checkNodeExists() error {
	if r := o.kubectl(Command{Argv: []string{"get", "node", o.opts.Node}}); r.Status != 0 {
		return fmt.Errorf("could not read Kubernetes node %s", o.opts.Node)
	}
	return nil
}

func (o *Operation) waitForNodeReady() error {
	node := o.opts.Node
	o.log("waiting for %s to report Ready", node)
	r := o.kubectl(Command{Argv: []string{"wait", "node/" + node, "--for=condition=Ready", "--timeout=" + o.opts.ReadyTimeout}, Passthrough: true})
	if r.Status != 0 {
		return fmt.Errorf("%s did not report Ready within %s; %s remains cordoned", node, o.opts.ReadyTimeout, node)
	}
	return nil
}

func (o *Operation) uncordon() error {
	node := o.opts.Node
	o.log("uncordoning %s", node)
	if r := o.kubectl(Command{Argv: []string{"uncordon", node}, Passthrough: true}); r.Status != 0 {
		return fmt.Errorf("could not uncordon %s", node)
	}
	return nil
}

// kubectl runs kubectl with the given arguments.
func (o *Operation) kubectl(c Command) Result {
	c.Argv = append([]string{"kubectl"}, c.Argv...)
	return o.runner.Run(o.ctx, c)
}

// getJSON runs a kubectl read and decodes its JSON output into v.
func (o *Operation) getJSON(v any, args ...string) error {
	return o.decodeKubectl(v, false, args)
}

// readQuiet is getJSON without kubectl's error output, for optional reads.
// found is false when kubectl fails, which callers handle as the Bash tool's
// `|| true` fallbacks did. Output kubectl returned but that does not decode
// is an error: falling back on it would weaken a gate on data that exists.
func (o *Operation) readQuiet(v any, args ...string) (found bool, err error) {
	r := o.kubectl(Command{Argv: args, Quiet: true})
	if r.Status != 0 {
		return false, nil
	}
	if err := json.Unmarshal([]byte(r.Stdout), v); err != nil {
		return false, fmt.Errorf("kubectl %s: invalid JSON: %v", strings.Join(args, " "), err)
	}
	return true, nil
}

func (o *Operation) decodeKubectl(v any, quiet bool, args []string) error {
	r := o.kubectl(Command{Argv: args, Quiet: quiet})
	if r.Status != 0 {
		return fmt.Errorf("kubectl %s failed (exit %d)", strings.Join(args, " "), r.Status)
	}
	if err := json.Unmarshal([]byte(r.Stdout), v); err != nil {
		return fmt.Errorf("kubectl %s: invalid JSON: %v", strings.Join(args, " "), err)
	}
	return nil
}

// ssh runs ssh with the given arguments.
func (o *Operation) ssh(c Command) Result {
	c.Argv = append([]string{"ssh"}, c.Argv...)
	return o.runner.Run(o.ctx, c)
}

// remoteRootShellCommand runs its standard input as root on the target,
// directly or through noninteractive sudo.
const remoteRootShellCommand = `
    if [[ $(id -u) -eq 0 ]]; then
      exec bash -s
    fi
    exec sudo -n bash -s
  `

// remoteRootShell runs script as root on the SSH target, with extra ssh
// options before the target.
func (o *Operation) remoteRootShell(script string, passthrough bool, options ...string) Result {
	argv := []string{"-o", "BatchMode=yes"}
	argv = append(argv, options...)
	argv = append(argv, o.opts.SSHTarget, remoteRootShellCommand)
	return o.ssh(Command{Argv: argv, Stdin: script, Passthrough: passthrough})
}
