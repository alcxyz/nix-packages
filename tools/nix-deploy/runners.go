package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const runnersUsage = `Usage: nixfleet runners [status] [--config FILE] [--all | <host>...]
       nixfleet runners restart [--config FILE] [--all | <host>...] [--dry-run] [--yes]

Reads and restarts the CI runner units of the selected hosts (ADR-0017).
With no host it works on this machine; --all selects the hosts deploy --all
would; hosts may be named by alias. Hosts are read as nixfleet status reads
them. Runner hosts report through nixfleet-runner-status, which nix-config's
runner module installs; a host without it shows only its runner units'
states and cannot restart them from here.

The runner units, the engine APIs and the resource policy are not restarted
by a switch (restartIfChanged = false), so they keep the definition they
started with. Each records it when it starts; a unit that runs and started
from an older definition than the deployed one, or whose start was not
recorded, needs a restart.

  status   One line per unit: the host, the unit, its systemd state, whether
           it needs a restart, and why a runner does not run (held by the
           runner hold for a game or low disk, stopped by the Podman store
           reset, refused by its start gate, failed, or stopped by hand,
           which nothing undoes) or why it needs the restart. Then the
           runner hold's reason, when one applies, and the last restart run.
  restart  Restarts, on each selected host, the units that need it, once
           every runner on the host is idle: the host's restart unit
           (forgejo-runner-restart.service, started through sudo -n where
           the inventory says so) waits until no Podman store reset runs,
           no runner is starting or stopping, no job container runs and no
           runner took a task for a few minutes, then restarts them in one
           systemctl restart. Restarting the resource policy or an engine
           API also restarts the runners that require it, so the whole host
           must be idle. When it stays busy for the host's wait limit (60
           minutes by default) the host gives up and nothing is restarted:
           no job is cancelled on purpose. A job taken between the last
           check and the stop gets the runner's graceful stop. The hosts
           run concurrently. The unit goes on when nixfleet is interrupted
           or loses its connection; nixfleet status shows its result.

           The plan comes first: what each host would restart and why, or
           why it is skipped. --dry-run stops there. Otherwise a terminal is
           asked to confirm, and without one the run needs --yes.

Options:
  --config FILE  The inventory; default NIX_DEPLOY_CONFIG.
  --all          Select the hosts deploy --all would.
  --dry-run      restart: print the plan and change nothing.
  --yes          restart: do not ask.
  -h, --help     Show this help.

The exit status is 2 for invalid arguments. status exits 0 however the
hosts answer, and 1 when the inventory or this machine's name fails.
restart exits 0 when every selected host restarted what it needed or needed
nothing, and 1 when one could not be read, cannot restart from here, stayed
busy or failed.
`

// Bounds of a restart run. The host bounds its own wait; nixfleet waits
// that long and for the restart itself: a runner that took a job after the
// last idle check may take up to its stop timeout.
const (
	restartMargin       = 2 * time.Hour
	restartPollInterval = 30 * time.Second
)

type runnersOptions struct {
	command    string
	configFile string
	all        bool
	hosts      []string
	dryRun     bool
	yes        bool
}

func parseRunnersArgs(args []string) (runnersOptions, error) {
	opts := runnersOptions{command: "status", configFile: os.Getenv("NIX_DEPLOY_CONFIG")}
	if len(args) > 0 && (args[0] == "status" || args[0] == "restart") {
		opts.command = args[0]
		args = args[1:]
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--all":
			opts.all = true
		case arg == "--dry-run" || arg == "--yes":
			if opts.command != "restart" {
				return opts, fmt.Errorf("%s is an option of runners restart", arg)
			}
			if arg == "--dry-run" {
				opts.dryRun = true
			} else {
				opts.yes = true
			}
		case arg == "--config":
			if i+1 >= len(args) || args[i+1] == "" {
				return opts, fmt.Errorf("--config requires a file")
			}
			i++
			opts.configFile = args[i]
		case strings.HasPrefix(arg, "--config="):
			opts.configFile = strings.TrimPrefix(arg, "--config=")
			if opts.configFile == "" {
				return opts, fmt.Errorf("--config requires a file")
			}
		case arg == "-h" || arg == "--help":
			return opts, errStatusHelp
		case strings.HasPrefix(arg, "-"):
			return opts, fmt.Errorf("unknown option '%s'", arg)
		default:
			opts.hosts = append(opts.hosts, arg)
		}
	}
	if opts.all && len(opts.hosts) > 0 {
		return opts, fmt.Errorf("--all and host names cannot be combined")
	}
	return opts, nil
}

// runRunners is nixfleet runners.
func runRunners(args []string) int {
	opts, err := parseRunnersArgs(args)
	if errors.Is(err, errStatusHelp) {
		fmt.Print(runnersUsage)
		return 0
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "nixfleet runners: %v\n", err)
		fmt.Fprint(os.Stderr, runnersUsage)
		return 2
	}
	s, code, err := openStatusCheck(opts.configFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "nixfleet runners: %v\n", err)
		return code
	}
	hosts, err := selectHosts(s.inv, s.self, statusOptions{all: opts.all, hosts: opts.hosts})
	if err != nil {
		fmt.Fprintf(os.Stderr, "nixfleet runners: %v\n", err)
		return 2
	}
	states := s.readHosts(hosts)
	if opts.command == "status" {
		printRunners(os.Stdout, s.ui, states)
		return 0
	}
	restarter := &fleetRestart{status: s, out: os.Stdout, poll: restartPollInterval, validateSudo: validateSudo}
	if isTerminal(os.Stdin) {
		restarter.ask = askLine(os.Stdin, os.Stdout)
	}
	return restarter.run(states, opts)
}

// printRunners lists every runner and tracked unit of the hosts, then each
// host's hold reason and last restart run, labeled [host] in its color.
func printRunners(w io.Writer, u *ui, states []hostStatus) {
	format := "%s %-40s %-18s %-8s %s\n"
	fmt.Fprintf(w, format, fmt.Sprintf("%-12s", "host"), "unit", "state", "restart", "why")
	var notes []string
	listed := false
	for _, h := range states {
		if !h.Reachable {
			notes = append(notes, u.host(h.Host)+" unreachable: "+h.Error)
			continue
		}
		r := h.Runners
		if r == nil {
			continue
		}
		listed = true
		if r.Error != "" {
			notes = append(notes, u.host(h.Host)+" runners unknown: "+r.Error)
		}
		for _, unit := range append(append([]runnerUnit{}, r.Units...), r.Services...) {
			state, restart, why := unit.State, "-", unit.Reason
			if unit.SubState != "" {
				state += "/" + unit.SubState
			}
			if r.RestartUnit != "" {
				restart = "no"
			}
			if unit.RestartNeeded {
				restart, why = "yes", unit.RestartReason
			}
			line := fmt.Sprintf(format, u.hostColumn(h.Host, 12), unit.Unit, state, restart, why)
			fmt.Fprintln(w, strings.TrimRight(line, " \n"))
		}
		if r.Hold != nil && r.Hold.Reason != "none" {
			notes = append(notes, fmt.Sprintf("%s runner hold %s: %s", u.host(h.Host), r.Hold.State, r.Hold.Reason))
		}
		if r.LastRestart != nil {
			notes = append(notes, u.host(h.Host)+" last runner restart "+describeRestart(r.LastRestart))
		}
	}
	if !listed {
		notes = append(notes, "no runner units on the selected hosts")
	}
	for _, note := range notes {
		fmt.Fprintln(w, note)
	}
}

// fleetRestart is one nixfleet runners restart run.
type fleetRestart struct {
	status *statusCheck
	out    io.Writer
	// ask puts a question to the operator; nil without a terminal.
	ask func(question string) bool
	// poll is how often the progress of a running restart is read; 0 reads
	// it only at the end.
	poll time.Duration
	// validateSudo asks for this machine's sudo password before the run,
	// on a terminal.
	validateSudo func() error
	lock         sync.Mutex
}

// restartTarget is a host's part of the plan.
type restartTarget struct {
	host   string
	unit   string
	wait   time.Duration
	units  []runnerUnit
	before *runnerRestart
	// joins is set when a restart already runs on the host.
	joins bool
	// skip says why the host is left out; failed when that fails the run.
	skip   string
	failed bool
}

func (f *fleetRestart) log(format string, args ...any) {
	f.lock.Lock()
	defer f.lock.Unlock()
	fmt.Fprintf(f.out, format+"\n", args...)
}

// say prints a line about a host, labeled [host] in its color.
func (f *fleetRestart) say(host, text string) {
	f.log("%s %s", f.status.ui.host(host), text)
}

// plan decides, for each host, what it restarts or why it is skipped.
func (f *fleetRestart) plan(states []hostStatus, all bool) []*restartTarget {
	var targets []*restartTarget
	for _, h := range states {
		t := &restartTarget{host: h.Host}
		r := h.Runners
		switch {
		case !h.Reachable:
			t.skip, t.failed = "unreachable: "+h.Error, true
		case r == nil && all:
			continue
		case r == nil:
			t.skip = "no runner units"
		case r.Error != "":
			t.skip, t.failed = "runners unknown: "+r.Error, true
		case r.Hold == nil && r.RestartUnit == "":
			t.skip, t.failed = "no restart unit (the host has no nixfleet-runner-status); restart its runners by hand", true
		case !safeUnit.MatchString(r.RestartUnit):
			t.skip, t.failed = fmt.Sprintf("the host reports no usable restart unit (%q); restart its runners by hand", r.RestartUnit), true
		default:
			t.unit = r.RestartUnit
			t.wait = time.Duration(r.RestartWaitSeconds) * time.Second
			t.units = r.restartNeeded()
			t.before = r.LastRestart
			t.joins = r.LastRestart.running()
			if len(t.units) == 0 && !t.joins {
				t.skip = "nothing to restart"
			}
		}
		targets = append(targets, t)
	}
	return targets
}

// describe says what the plan does on the host; the units it restarts
// follow, one per line.
func (t *restartTarget) describe() string {
	switch {
	case t.skip != "":
		return t.skip
	case t.joins:
		return "a restart already runs (" + describeRestart(t.before) + "); following it"
	}
	return fmt.Sprintf("restart once idle, waiting up to %s:", formatWait(t.wait))
}

func (f *fleetRestart) run(states []hostStatus, opts runnersOptions) int {
	targets := f.plan(states, opts.all)
	exit := 0
	var work []*restartTarget
	for _, t := range targets {
		f.say(t.host, t.describe())
		if t.skip == "" && !t.joins {
			for _, u := range t.units {
				f.log("    %-40s %s", u.Unit, u.RestartReason)
			}
		}
		if t.failed {
			exit = 1
		}
		if t.skip == "" {
			work = append(work, t)
		}
	}
	if len(targets) == 0 {
		f.log("no runner hosts among the selected hosts")
	}
	if opts.dryRun || len(work) == 0 {
		return exit
	}
	if !opts.yes {
		if f.ask == nil {
			fmt.Fprintln(os.Stderr, "nixfleet runners restart: no terminal to confirm on; pass --yes")
			return 2
		}
		hosts := "1 host"
		if len(work) > 1 {
			hosts = fmt.Sprintf("%d hosts", len(work))
		}
		if !f.ask("restart the runner units of " + hosts + " once idle? [y/N] ") {
			f.log("nothing restarted")
			return 1
		}
	}
	for _, t := range work {
		if t.host == f.status.self && f.ask != nil && f.validateSudo != nil {
			if err := f.validateSudo(); err != nil {
				f.say(t.host, "sudo: "+err.Error())
			}
		}
	}
	f.log("Interrupting nixfleet leaves the restarts running on the hosts; nixfleet status shows their results.")
	outcomes := make([]string, len(work))
	failed := make([]bool, len(work))
	var wait sync.WaitGroup
	for i, t := range work {
		wait.Add(1)
		go func() {
			defer wait.Done()
			outcomes[i], failed[i] = f.restart(t)
		}()
	}
	wait.Wait()
	for i, t := range work {
		f.say(t.host, outcomes[i])
		if failed[i] {
			exit = 1
		}
	}
	return exit
}

// startCommand starts the host's restart unit and waits for it to finish.
func (f *fleetRestart) startCommand(t *restartTarget) []string {
	command := "systemctl start '" + t.unit + "'"
	if t.host == f.status.self {
		return []string{"sh", "-c", "sudo -n " + command}
	}
	if f.status.inv.usesRemoteSudo(t.host) {
		command = "sudo -n " + command
	}
	argv := []string{"ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=" + f.status.connectTimeout}
	argv = append(argv, f.status.sshArgs...)
	return append(argv, f.status.inv.systemEndpoint(t.host), command)
}

// restart runs the host's restart unit, reports its progress, and returns
// its outcome and whether it failed.
func (f *fleetRestart) restart(t *restartTarget) (string, bool) {
	f.say(t.host, "starting "+t.unit)
	done := make(chan struct{})
	var follow sync.WaitGroup
	if f.poll > 0 {
		follow.Add(1)
		go func() {
			defer follow.Done()
			f.follow(t, done)
		}()
	}
	limit := t.wait + restartMargin
	_, stderr, status := f.status.run(limit, f.startCommand(t)...)
	close(done)
	follow.Wait()

	final := f.status.readHost(t.host)
	var last *runnerRestart
	if final.Runners != nil {
		last = final.Runners.LastRestart
	}
	fresh := last != nil && (t.before == nil || t.joins || last.Time > t.before.Time)
	switch {
	case status == exitTimedOut:
		return fmt.Sprintf("no result within %s; the restart goes on on the host", formatWait(limit)), true
	case !fresh && status != 0:
		message := oneLine(stderr)
		if message == "" {
			message = fmt.Sprintf("exit status %d", status)
		}
		return "could not start " + t.unit + ": " + message, true
	case !fresh:
		return t.unit + " left no result", true
	}
	// The run's result comes first; a start that failed as well (a lost
	// connection, a sudo refusal while joining a run) is said after it.
	outcome, failed := describeOutcome(t, last)
	if status != 0 {
		message := oneLine(stderr)
		if message == "" {
			message = fmt.Sprintf("exit status %d", status)
		}
		outcome += "; systemctl start: " + message
		failed = true
	}
	return outcome, failed
}

// describeOutcome says what a finished restart run did, and whether that
// fails the nixfleet run.
func describeOutcome(t *restartTarget, last *runnerRestart) (string, bool) {
	switch last.Status {
	case "done":
		if len(last.Units) == 0 {
			return "done", false
		}
		return "restarted " + unitCount(last.Units), false
	case "nothing":
		return "nothing needed a restart", false
	case "busy":
		return "busy for " + formatWait(t.wait) + ", nothing restarted: " + last.Reason, true
	}
	return describeRestart(last), true
}

// follow prints each change of a running restart until done closes.
func (f *fleetRestart) follow(t *restartTarget, done <-chan struct{}) {
	seen := ""
	ticker := time.NewTicker(f.poll)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
		}
		h := f.status.readHost(t.host)
		if h.Runners == nil || h.Runners.LastRestart == nil || !h.Runners.LastRestart.running() {
			continue
		}
		last := h.Runners.LastRestart
		if t.before != nil && !t.joins && last.Time <= t.before.Time {
			continue
		}
		now := last.Status
		if last.Reason != "" {
			now += ": " + last.Reason
		}
		if now != seen {
			seen = now
			f.say(t.host, now)
		}
	}
}

// askLine asks on a terminal and reads one line; only y or yes agrees.
func askLine(in io.Reader, out io.Writer) func(string) bool {
	reader := bufio.NewReader(in)
	return func(question string) bool {
		fmt.Fprint(out, question)
		answer, _ := reader.ReadString('\n')
		answer = strings.ToLower(strings.TrimSpace(answer))
		return answer == "y" || answer == "yes"
	}
}

// validateSudo lets sudo ask for this machine's password once, so the
// restart's sudo -n succeeds.
func validateSudo() error {
	cmd := exec.Command("sudo", "-v")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}
