package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// hookTimeout bounds one call of the inventory's rebootHookCommand.
const hookTimeout = 60 * time.Second

// Events of the reboot hook (ADR-0010 amendment).
const (
	hookRunStart   = "run-start"
	hookHostStart  = "host-start"
	hookHostFinish = "host-finish"
	hookRunFinish  = "run-finish"
)

// runHookCommand runs the hook through sh with the event's variables added
// to the environment, and returns its last output line on failure.
func runHookCommand(ctx context.Context, command string, env []string) error {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Env = append(os.Environ(), env...)
	// The hook runs in its own process group, so the deadline stops every
	// process it started, and the group is killed when the hook ends, so
	// no late one acts after its event; the wait for one that keeps the
	// output open ends too, as in the other commands' runners. An interrupt
	// of the terminal does not reach it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = killGrace
	output, err := cmd.CombinedOutput()
	// Whatever the hook left running, even ignoring SIGTERM, ends with it.
	if cmd.Process != nil {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return fmt.Errorf("did not finish within %s", hookTimeout)
	}
	if line := oneLine(lastLine(string(output))); line != "" {
		return fmt.Errorf("%v: %s", err, line)
	}
	return err
}

// callHook tells the inventory's rebootHookCommand about an event of a run
// that changes hosts, with the event's variables (NIXFLEET_EVENT and the
// vars given, each prefixed NIXFLEET_). Calls never overlap. The hook runs
// on its own clock, so an interrupted run still reports; a hook that fails
// or does not finish within hookTimeout is a warning and never stops a
// reboot.
func (f *fleetReboot) callHook(event string, vars map[string]string) {
	command := f.inv.RebootHookCommand
	if command == "" || f.opts.dryRun || f.opts.node.CheckOnly {
		return
	}
	env := []string{"NIXFLEET_EVENT=" + event}
	names := mapsKeys(vars)
	slices.Sort(names)
	for _, name := range names {
		env = append(env, "NIXFLEET_"+name+"="+vars[name])
	}
	f.hookLock.Lock()
	defer f.hookLock.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), hookTimeout)
	defer cancel()
	if err := f.hook(ctx, command, env); err != nil {
		f.outputLock.Lock()
		fmt.Fprintf(f.errOut, "nixfleet reboot: warning: the reboot hook failed on %s: %v\n", event, err)
		f.outputLock.Unlock()
	}
}

func mapsKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	return keys
}

// hostHookVars are the variables of a host's events: its name, its serial
// group (empty for a group of its own) and its kind.
func (h *hostReboot) hostHookVars() map[string]string {
	kind := "host"
	switch {
	case h.target.self:
		kind = "self"
	case h.target.node:
		kind = "node"
	}
	group := ""
	if h.group.serial != nil {
		group = h.group.label()
	}
	return map[string]string{"HOST": h.target.host, "GROUP": group, "KIND": kind}
}

// finishHook reports a host's outcome; its duration counts from its start
// event.
func (h *hostReboot) finishHook(start time.Time, outcome, reason string) {
	vars := h.hostHookVars()
	vars["OUTCOME"] = outcome
	vars["REASON"] = reason
	vars["DURATION"] = strconv.Itoa(int(time.Since(start).Round(time.Second) / time.Second))
	h.f.callHook(hookHostFinish, vars)
}

// rebootingHosts are the hosts the run is about to reboot, in plan order,
// counted as size counts them: not those of a group that fails before it
// starts, nor this machine when one does.
func (p *rebootPlan) rebootingHosts() []string {
	var hosts []string
	for _, g := range p.groups {
		if g.failed() || g.last && p.failsBeforeStart() {
			continue
		}
		for _, t := range g.members {
			if t.skip == "" && t.fail == "" {
				hosts = append(hosts, t.host)
			}
		}
	}
	return hosts
}

// summaryHookVars are the variables of the run-finish event: every host's
// line of the summary, and whether one failed.
func (p *rebootPlan) summaryHookVars() map[string]string {
	var lines []string
	failed := false
	for _, g := range p.groups {
		for _, t := range g.members {
			line := t.host + " " + t.outcome
			if t.duration > 0 {
				line += " " + t.duration.Round(time.Second).String()
			}
			if t.reason != "" {
				line += ": " + t.reason
			}
			lines = append(lines, line)
			failed = failed || t.outcome == outcomeFailed
		}
	}
	return map[string]string{"SUMMARY": strings.Join(lines, "\n"), "FAILED": strconv.FormatBool(failed)}
}
