package main

import (
	"bytes"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// nixfleet runners and the runner part of nixfleet status, against canned
// nixfleet-runner-status reports; nothing reaches a real host.

// runnerReport renders a nixfleet-runner-status report, as captured on a
// runner host, with the given unit and restart records.
func runnerReport(units []string, restart string) string {
	lines := []string{"version\t1", "hold\tactive\tnone", "restart-unit\tforgejo-runner-restart.service\t3600"}
	if restart != "" {
		lines = append(lines, restart)
	}
	lines = append(lines, units...)
	lines = append(lines, "exit\t0")
	var b strings.Builder
	for _, line := range lines {
		b.WriteString("runners." + line + "\n")
	}
	return b.String()
}

const (
	primaryOK  = "unit\trunner\tforgejo-actions-runner.service\tactive\trunning\tno\t-\tforgejo-docker,ubuntu-latest\t-"
	primaryOld = "unit\trunner\tforgejo-actions-runner.service\tactive\trunning\tyes\tstarted from an older definition\tforgejo-docker,ubuntu-latest\t-"
	fastHeld   = "unit\trunner\tforgejo-podman-runner-fast.service\tinactive\tdead\tno\t-\tci-fast\theld by the runner hold (game); it starts when that ends"
	policyNew  = "unit\tservice\tforgejo-runner-resource-policy.service\tactive\texited\tyes\tno start record\t-\t-"
	policyOK   = "unit\tservice\tforgejo-runner-resource-policy.service\tactive\texited\tno\t-\t-\t-"
)

func TestParseRunnerReport(t *testing.T) {
	r := parseRunners("os=Linux\n" + runnerReport([]string{primaryOld, fastHeld, policyNew},
		"restart\tinactive\tbusy\t1762100000\tforgejo-actions-runner.service,forgejo-runner-resource-policy.service\tjob_running unit=forgejo-runner-docker.service"))
	want := &runnerStatus{
		Active: 1, Total: 2,
		Units: []runnerUnit{
			{Unit: "forgejo-actions-runner.service", State: "active", SubState: "running", RestartNeeded: true,
				RestartReason: "started from an older definition", Labels: []string{"forgejo-docker", "ubuntu-latest"}},
			{Unit: "forgejo-podman-runner-fast.service", State: "inactive", SubState: "dead", Labels: []string{"ci-fast"},
				Reason: "held by the runner hold (game); it starts when that ends"},
		},
		Services: []runnerUnit{{Unit: "forgejo-runner-resource-policy.service", State: "active", SubState: "exited",
			RestartNeeded: true, RestartReason: "no start record"}},
		Hold:        &runnerHold{State: "active", Reason: "none"},
		RestartUnit: "forgejo-runner-restart.service", RestartWaitSeconds: 3600,
		LastRestart: &runnerRestart{State: "inactive", Status: "busy", Time: 1762100000,
			Units:  []string{"forgejo-actions-runner.service", "forgejo-runner-resource-policy.service"},
			Reason: "job_running unit=forgejo-runner-docker.service"},
	}
	if !reflect.DeepEqual(r, want) {
		t.Errorf("got  %+v\nwant %+v", r, want)
	}
	if got := r.restartNeeded(); len(got) != 2 || got[0].Unit != "forgejo-actions-runner.service" || got[1].Unit != "forgejo-runner-resource-policy.service" {
		t.Errorf("restart needed: %+v", got)
	}

	// A format nixfleet does not know, or a command that failed or stopped
	// early, never reads as healthy runners or as none.
	for output, message := range map[string]string{
		"runners.version\t2\nrunners." + primaryOK + "\nrunners.exit\t0\n": "nixfleet-runner-status version 2 is not supported",
		"runners." + primaryOK + "\nrunners.exit\t0\n":                     "nixfleet-runner-status printed no version",
		"runners.exit\t1\n": "nixfleet-runner-status exited with status 1",
		"runners.version\t1\nrunners." + primaryOK + "\n": "nixfleet-runner-status's report is incomplete",
	} {
		r := parseRunners(output)
		if r == nil || r.Error != message || r.Total != 0 || r.Units != nil {
			t.Errorf("%q: %+v", output, r)
		}
	}
}

func TestStatusExplainsRunners(t *testing.T) {
	r := parseRunners(runnerReport([]string{primaryOld, fastHeld, policyNew},
		"restart\tactivating\twaiting\t1762100000\tforgejo-actions-runner.service\trecent_job unit=forgejo-podman-runner.service"))
	var out bytes.Buffer
	printStatus(&out, nil, statusReport{Hosts: []hostStatus{{Host: "xyz", Reachable: true, OS: "Linux", Runners: r}}})
	text := out.String()
	for _, want := range []string{
		"[xyz] forgejo-podman-runner-fast.service inactive: held by the runner hold (game); it starts when that ends\n",
		"[xyz] 2 units need a restart (started from an older definition, no start record): nixfleet runners restart xyz\n",
		"[xyz] runner restart waiting at ",
		" (forgejo-actions-runner.service): recent_job unit=forgejo-podman-runner.service\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	if !strings.Contains(text, " 1/2 ") {
		t.Errorf("runner column:\n%s", text)
	}
	// A finished restart that did its work is not news.
	r.LastRestart = &runnerRestart{State: "inactive", Status: "done", Units: []string{}}
	out.Reset()
	printStatus(&out, nil, statusReport{Hosts: []hostStatus{{Host: "xyz", Reachable: true, OS: "Linux", Runners: r}}})
	if strings.Contains(out.String(), "runner restart") {
		t.Errorf("done restart reported:\n%s", out.String())
	}
}

func TestRunnersStatusTable(t *testing.T) {
	states := []hostStatus{
		{Host: "xyz", Reachable: true, Runners: parseRunners(runnerReport([]string{primaryOld, fastHeld, policyOK},
			"restart\tinactive\tfailed\t1762100000\t-\tsystemctl restart exited with status 1"))},
		{Host: "old", Reachable: true, Runners: parseRunners("runner=forgejo-actions-runner.service loaded active running x\n")},
		{Host: "pi", Reachable: true},
		{Host: "gone", Error: "timed out"},
	}
	states[0].Runners.Hold.Reason = "game"
	var out bytes.Buffer
	printRunners(&out, nil, states)
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	fields := func(line string) []string { return strings.Fields(line) }
	if got := fields(lines[1]); !slices.Equal(got[:5], []string{"xyz", "forgejo-actions-runner.service", "active/running", "yes", "started"}) {
		t.Errorf("primary: %q", lines[1])
	}
	if got := fields(lines[2]); !slices.Equal(got[:5], []string{"xyz", "forgejo-podman-runner-fast.service", "inactive/dead", "no", "held"}) {
		t.Errorf("fast: %q", lines[2])
	}
	if got := fields(lines[3]); !slices.Equal(got, []string{"xyz", "forgejo-runner-resource-policy.service", "active/exited", "no"}) {
		t.Errorf("policy: %q", lines[3])
	}
	// Without the report a host cannot tell whether a restart is needed.
	if got := fields(lines[4]); !slices.Equal(got, []string{"old", "forgejo-actions-runner.service", "active/running", "-"}) {
		t.Errorf("old: %q", lines[4])
	}
	for _, want := range []string{"[gone] unreachable: timed out\n", "[xyz] runner hold active: game\n", "[xyz] last runner restart failed at ", ": systemctl restart exited with status 1\n"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "pi") {
		t.Errorf("a host without runners is listed:\n%s", out.String())
	}
}

func TestRunnersArguments(t *testing.T) {
	opts, err := parseRunnersArgs([]string{"restart", "--all", "--dry-run", "--yes", "--config=inv.json"})
	if err != nil || opts.command != "restart" || !opts.all || !opts.dryRun || !opts.yes || opts.configFile != "inv.json" {
		t.Errorf("restart: %+v %v", opts, err)
	}
	opts, err = parseRunnersArgs([]string{"edge", "xyz"})
	if err != nil || opts.command != "status" || !slices.Equal(opts.hosts, []string{"edge", "xyz"}) {
		t.Errorf("status: %+v %v", opts, err)
	}
	for _, args := range [][]string{{"--yes"}, {"status", "--dry-run"}, {"restart", "--all", "xyz"}, {"restart", "--bogus"}, {"restart", "--config"}} {
		if _, err := parseRunnersArgs(args); err == nil {
			t.Errorf("%v: accepted", args)
		}
	}
}

// restartFleet answers status reads with each host's report and the start
// of the restart unit with the host's scripted result.
type restartFleet struct {
	lock    sync.Mutex
	reports map[string]string
	// after replaces a host's report once its restart unit was started.
	after  map[string]string
	status map[string]int
	starts []string
	// block holds the start until released, for the progress test.
	block chan struct{}
}

func (f *restartFleet) run(timeout time.Duration, argv ...string) (string, string, int) {
	host := "xyz"
	if argv[0] == "ssh" {
		host = strings.Split(strings.TrimSuffix(argv[len(argv)-2], ".invalid"), "@")[1]
	}
	last := argv[len(argv)-1]
	if strings.Contains(last, "systemctl start") {
		f.lock.Lock()
		f.starts = append(f.starts, host+": "+last)
		f.lock.Unlock()
		if f.block != nil {
			<-f.block
		}
		f.lock.Lock()
		defer f.lock.Unlock()
		if report, ok := f.after[host]; ok {
			f.reports[host] = report
		}
		if f.status[host] != 0 {
			return "", "ssh: connect to host " + host + ": refused\n", f.status[host]
		}
		return "", "", 0
	}
	f.lock.Lock()
	defer f.lock.Unlock()
	report, ok := f.reports[host]
	if !ok {
		return "", "ssh: unreachable\n", 255
	}
	return systems("/nix/store/aaa-nixos-system-booted", "k1", "i1") + report, "", 0
}

func newFleetRestart(t *testing.T, f *restartFleet, out *bytes.Buffer) *fleetRestart {
	s := newStatusCheck(t, &fakeFleet{})
	s.run = f.run
	return &fleetRestart{status: s, out: out}
}

func TestRunnersRestart(t *testing.T) {
	restarted := "restart\tinactive\tdone\t1762100100\tforgejo-actions-runner.service,forgejo-runner-resource-policy.service\t-"
	f := &restartFleet{
		reports: map[string]string{
			// This machine needs a restart and gets it.
			"xyz": runnerReport([]string{primaryOld, policyNew}, "restart\tinactive\tnothing\t1762000000\t-\t-"),
			// node runs its definitions; it is left alone.
			"node": runnerReport([]string{primaryOK, policyOK}, ""),
			// solo, reached with sudo, stays busy.
			"solo": runnerReport([]string{primaryOld}, ""),
			// k1 runs runners without the report.
			"k1": "runner=forgejo-actions-runner.service loaded active running x\n",
			// spare's report names no restart unit.
			"spare": strings.Replace(runnerReport([]string{primaryOld}, ""), "runners.restart-unit\tforgejo-runner-restart.service\t3600\n", "", 1),
		},
		after: map[string]string{
			"xyz":  runnerReport([]string{primaryOK, policyOK}, restarted),
			"solo": runnerReport([]string{primaryOld}, "restart\tinactive\tbusy\t1762100200\tforgejo-actions-runner.service\tjob_running unit=forgejo-runner-docker.service"),
		},
	}
	var out bytes.Buffer
	r := newFleetRestart(t, f, &out)
	states := r.status.readHosts([]string{"xyz", "node", "solo", "k1", "spare", "mac"})

	if code := r.run(states, runnersOptions{command: "restart", dryRun: true}); code != 1 || len(f.starts) != 0 {
		t.Fatalf("dry run: %d %v", code, f.starts)
	}
	// The units a host restarts follow it, one per line.
	unitLine := func(unit, reason string) string { return fmt.Sprintf("    %-40s %s", unit, reason) }
	for _, want := range []string{
		"[xyz] restart once running jobs finish, waiting up to 1h:\n" +
			unitLine("forgejo-actions-runner.service", "started from an older definition") + "\n" +
			unitLine("forgejo-runner-resource-policy.service", "no start record"),
		"[node] nothing to restart",
		"[solo] restart once running jobs finish, waiting up to 1h:\n" + unitLine("forgejo-actions-runner.service", "started from an older definition"),
		"[k1] no restart unit (the host has no nixfleet-runner-status); restart its runners by hand",
		`[spare] the host reports no usable restart unit (""); restart its runners by hand`,
		"[mac] unreachable: ssh: unreachable",
	} {
		if !strings.Contains(out.String(), want+"\n") {
			t.Errorf("plan lacks %q:\n%s", want, out.String())
		}
	}

	// Without a terminal the run needs --yes; a declined question changes
	// nothing.
	if code := r.run(states, runnersOptions{command: "restart"}); code != 2 || len(f.starts) != 0 {
		t.Fatalf("no terminal: %d %v", code, f.starts)
	}
	r.ask = func(string) bool { return false }
	if code := r.run(states, runnersOptions{command: "restart"}); code != 1 || len(f.starts) != 0 {
		t.Fatalf("declined: %d %v", code, f.starts)
	}

	out.Reset()
	r.ask = nil
	if code := r.run(states, runnersOptions{command: "restart", yes: true}); code != 1 {
		t.Errorf("exit %d", code)
	}
	slices.Sort(f.starts)
	if want := []string{
		"solo: sudo -n systemctl start 'forgejo-runner-restart.service'",
		"xyz: sudo -n systemctl start 'forgejo-runner-restart.service'",
	}; !slices.Equal(f.starts, want) {
		t.Errorf("starts: %q", f.starts)
	}
	for _, want := range []string{
		"[xyz] restarted 2 units\n",
		"[solo] busy for 1h, nothing restarted: job_running unit=forgejo-runner-docker.service\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q:\n%s", want, out.String())
		}
	}
}

func TestRunnersRestartFailures(t *testing.T) {
	f := &restartFleet{
		reports: map[string]string{
			// The start fails before the unit writes anything.
			"node": runnerReport([]string{primaryOld}, "restart\tinactive\tdone\t1762000000\t-\t-"),
			// The unit runs and reports a failure.
			"solo": runnerReport([]string{primaryOld}, ""),
			// A restart already runs; the run follows it.
			"k1": runnerReport([]string{primaryOK}, "restart\tactivating\twaiting\t1762100000\tforgejo-actions-runner.service\tstore_reset_running"),
		},
		after: map[string]string{
			"solo": runnerReport([]string{primaryOld}, "restart\tfailed\tfailed\t1762100300\tforgejo-actions-runner.service\tsystemctl restart exited with status 1"),
			"k1":   runnerReport([]string{primaryOK}, "restart\tinactive\tdone\t1762100000\tforgejo-actions-runner.service\t-"),
		},
		// k1's joined run finished, but the start that joined it failed.
		status: map[string]int{"node": 255, "solo": 1, "k1": 255},
	}
	var out bytes.Buffer
	r := newFleetRestart(t, f, &out)
	states := r.status.readHosts([]string{"node", "solo", "k1"})
	if code := r.run(states, runnersOptions{command: "restart", yes: true}); code != 1 {
		t.Errorf("exit %d", code)
	}
	for _, want := range []string{
		"[k1] a restart already runs (waiting at ",
		" (forgejo-actions-runner.service): store_reset_running); following it\n",
		"[node] could not start forgejo-runner-restart.service: ssh: connect to host node: refused\n",
		"[solo] failed at ",
		" (forgejo-actions-runner.service): systemctl restart exited with status 1; systemctl start: ssh: connect to host solo: refused\n",
		"[k1] restarted forgejo-actions-runner.service; systemctl start: ssh: connect to host k1: refused\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q:\n%s", want, out.String())
		}
	}
	// node is reached as root, solo through sudo.
	slices.Sort(f.starts)
	if !slices.Equal(f.starts, []string{"k1: systemctl start 'forgejo-runner-restart.service'", "node: systemctl start 'forgejo-runner-restart.service'", "solo: sudo -n systemctl start 'forgejo-runner-restart.service'"}) {
		t.Errorf("starts: %q", f.starts)
	}
}

// While the restart unit runs, its progress is printed as it changes.
func TestRunnersRestartProgress(t *testing.T) {
	f := &restartFleet{
		reports: map[string]string{"node": runnerReport([]string{primaryOld}, "")},
		after:   map[string]string{"node": runnerReport([]string{primaryOK}, "restart\tinactive\tdone\t1762100100\tforgejo-actions-runner.service\t-")},
		block:   make(chan struct{}),
	}
	var out bytes.Buffer
	r := newFleetRestart(t, f, &out)
	r.poll = 5 * time.Millisecond
	states := r.status.readHosts([]string{"node"})
	// The unit reports a wait once started.
	go func() {
		for {
			f.lock.Lock()
			started := len(f.starts) > 0
			if started {
				f.reports["node"] = runnerReport([]string{primaryOld}, "restart\tactivating\twaiting\t1762100050\tforgejo-actions-runner.service\tjob_running unit=forgejo-runner-podman.service")
			}
			f.lock.Unlock()
			if started {
				break
			}
			time.Sleep(time.Millisecond)
		}
		time.Sleep(50 * time.Millisecond)
		close(f.block)
	}()
	code := r.run(states, runnersOptions{command: "restart", yes: true})
	if code != 0 {
		t.Errorf("exit %d:\n%s", code, out.String())
	}
	text := out.String()
	if strings.Count(text, "[node] waiting: job_running unit=forgejo-runner-podman.service\n") != 1 {
		t.Errorf("progress:\n%s", text)
	}
	if !strings.HasSuffix(text, "[node] restarted forgejo-actions-runner.service\n") {
		t.Errorf("outcome:\n%s", text)
	}
}
