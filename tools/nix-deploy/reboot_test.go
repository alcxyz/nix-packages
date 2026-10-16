package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"nix-deploy/k8snode"
)

// nixfleet reboot against a fake fleet: host state served over a fake ssh,
// a fake cluster for kubectl, fake ready commands and a fake node reboot.
// Nothing reaches a real host or cluster.

const rebootInventory = `{
  "schemaVersion": 1, "operatorHost": "op", "homeManagerUser": "user", "homeOutputPrefix": "user-",
  "knownHosts": ["op", "server-a", "server-b", "server-c", "server-d", "dns-a", "dns-b", "host-a", "host-b", "mac"],
  "homeManagerHosts": [],
  "remoteHosts": ["server-a", "server-b", "server-c", "server-d", "dns-a", "dns-b", "host-a", "host-b", "mac"],
  "deployAllHosts": ["op", "server-a", "server-b", "server-c", "server-d", "dns-a", "dns-b", "host-a", "host-b", "mac"],
  "aliases": {"alias-a": "host-a"},
  "sshHosts": {},
  "systemSshUsers": {"host-b": "admin"},
  "systemRemoteSudoHosts": ["host-b"],
  "systemActivationModes": {},
  "serialGroups": [
    {"hosts": ["server-a", "server-b", "server-c"], "readyCommand": "ready {host}", "readyTimeoutSeconds": 25},
    {"hosts": ["dns-a", "dns-b"], "readyCommand": "ready {host}", "readyTimeoutSeconds": 25}
  ],
  "kubernetesNodes": ["server-a", "server-b", "server-c", "server-d"]
}`

// fakeHost is one host's state as the fake serves it.
type fakeHost struct {
	// os is the kernel name; empty for Linux.
	os          string
	unreachable bool
	// verdict is the reboot verdict, yes, no or unknown; after replaces it
	// once the host rebooted.
	verdict string
	after   string
	boots   int
	// down counts the boot ID probes that still fail after a reboot.
	down int
	// node makes the cluster list a node named like the host.
	node     bool
	cordoned bool
	// runners maps runner units to their state.
	runners map[string]string
	// runnersStayDown keeps the runner units inactive after a reboot, and
	// neverReturns the host down.
	runnersStayDown bool
	neverReturns    bool

	notReady    bool
	stopFails   bool
	rebootFails bool
	nodeFails   bool
	// noSystem serves a Linux host without NixOS system links, and
	// rebootLost drops the reboot command before it arrives.
	noSystem   bool
	rebootLost bool
}

type rebootFake struct {
	t       *testing.T
	self    string
	lock    sync.Mutex
	hosts   map[string]*fakeHost
	cluster bool
	// primaries counts the database primaries per node; primariesFail makes
	// their read fail.
	primaries     map[string]int
	primariesFail bool
	// events records every change and ready check, commands every remote
	// command that changes a host.
	events   []string
	commands []string
	// busy is the member of each serial group between its first change and
	// its ready gate after the reboot; overlaps records any other member
	// changing meanwhile, and any two node reboots at once.
	busy        map[string]string
	nodeBusy    bool
	overlaps    []string
	nodeOptions []k8snode.Options
	// stopCancels interrupts the run while runner units stop, and
	// rebootCancels as the reboot command is sent.
	stopCancels   bool
	rebootCancels bool
	cancel        context.CancelFunc

	inv          *inventory
	out          lockedBuffer
	errOut       lockedBuffer
	terminal     bool
	confirmation string
}

func newRebootFake(t *testing.T) *rebootFake {
	t.Helper()
	inv, err := parseInventory([]byte(rebootInventory))
	if err != nil {
		t.Fatal(err)
	}
	f := &rebootFake{t: t, self: "op", inv: inv, cluster: true, hosts: map[string]*fakeHost{}, busy: map[string]string{}}
	for _, host := range inv.KnownHosts {
		f.hosts[host] = &fakeHost{verdict: "yes", after: "no", node: strings.HasPrefix(host, "server-")}
	}
	f.hosts["mac"].os = "Darwin"
	f.hosts["host-a"].runners = map[string]string{"forgejo-actions-runner.service": "active", "gitea-runner-spare.service": "inactive"}
	return f
}

// endpointHost is the host of user@host.
func endpointHost(endpoint string) string {
	_, host, _ := strings.Cut(endpoint, "@")
	return host
}

// statusRun answers nixfleet status's reads.
func (f *rebootFake) statusRun(timeout time.Duration, argv ...string) (string, string, int) {
	f.lock.Lock()
	defer f.lock.Unlock()
	switch {
	case slices.Equal(argv[:3], []string{"kubectl", "get", "nodes"}):
		if !f.cluster {
			return "", "The connection to the server was refused\n", 1
		}
		var items []string
		for _, host := range f.inv.KnownHosts {
			if h := f.hosts[host]; h.node {
				items = append(items, fmt.Sprintf(`{"metadata": {"name": %q}, "spec": {"unschedulable": %t}, "status": {"conditions": [{"type": "Ready", "status": "True"}]}}`, host, h.cordoned))
			}
		}
		return `{"items": [` + strings.Join(items, ",") + `]}`, "", 0
	case slices.Equal(argv[:3], []string{"kubectl", "get", "pods"}):
		if !slices.Contains(argv, "cnpg.io/instanceRole=primary") {
			return "", "unexpected read\n", 97
		}
		if f.primariesFail {
			return "", "error: the server is currently unable to handle the request\n", 1
		}
		var items []string
		for node, count := range f.primaries {
			for range count {
				items = append(items, fmt.Sprintf(`{"spec": {"nodeName": %q}}`, node))
			}
		}
		return `{"items": [` + strings.Join(items, ",") + `]}`, "", 0
	case argv[0] == "ssh" && strings.Contains(argv[len(argv)-1], runnerScript):
		h := f.hosts[endpointHost(argv[len(argv)-2])]
		if h == nil {
			return "", "unexpected host\n", 97
		}
		if h.unreachable {
			return "", "ssh: connect to host port 22: No route to host\n", 255
		}
		if h.os != "" {
			return "os=" + h.os + "\n", "", 0
		}
		if h.noSystem {
			return "os=Linux\nbooted=-\ncurrent=-\nprofile=-\n", "", 0
		}
		var output string
		switch h.verdict {
		case "yes":
			output = systems("/nix/store/bbb-nixos-system-new", "k2", "i1")
		case "no":
			output = systems("/nix/store/aaa-nixos-system-booted", "k1", "i1")
		default:
			output = systems("/nix/store/aaa-nixos-system-booted", "k1", "-")
		}
		for _, unit := range sortedUnits(h.runners) {
			output += fmt.Sprintf("runner=%s loaded %s running runner\n", unit, h.runners[unit])
		}
		return output, "", 0
	}
	f.t.Errorf("unexpected status command: %v", argv)
	return "", "unexpected command\n", 97
}

func sortedUnits(runners map[string]string) []string {
	var units []string
	for unit := range runners {
		units = append(units, unit)
	}
	slices.Sort(units)
	return units
}

// begin marks host as changing; the lock is held.
func (f *rebootFake) begin(host string) {
	g := f.inv.group(host)
	if g == nil {
		return
	}
	if other, ok := f.busy[g.Hosts[0]]; ok && other != host {
		f.overlaps = append(f.overlaps, other+" and "+host)
	}
	f.busy[g.Hosts[0]] = host
}

// command answers the host commands and ready checks of a fleet reboot.
func (f *rebootFake) command(c k8snode.Command) k8snode.Result {
	f.lock.Lock()
	defer f.lock.Unlock()
	argv := c.Argv
	switch argv[0] {
	case "bash":
		host := strings.TrimPrefix(argv[2], "ready ")
		f.events = append(f.events, "ready "+host)
		if f.hosts[host].notReady {
			return k8snode.Result{Stderr: host + " is not ready\n", Status: 1}
		}
		if g := f.inv.group(host); g != nil && f.busy[g.Hosts[0]] == host {
			delete(f.busy, g.Hosts[0])
		}
		return k8snode.Result{Stdout: host + " is ready\n"}
	case "ssh":
	default:
		f.t.Errorf("unexpected command: %v", argv)
		return k8snode.Result{Status: 97}
	}
	if !slices.Contains(argv, "BatchMode=yes") {
		f.t.Errorf("ssh without BatchMode: %v", argv)
	}
	host := endpointHost(argv[len(argv)-2])
	h := f.hosts[host]
	remote := argv[len(argv)-1]
	if remote != "cat /proc/sys/kernel/random/boot_id" {
		f.commands = append(f.commands, argv[len(argv)-2]+" "+remote)
	}
	switch {
	case remote == "cat /proc/sys/kernel/random/boot_id":
		if h.unreachable {
			return k8snode.Result{Status: 255}
		}
		if h.down > 0 {
			h.down--
			return k8snode.Result{Status: 255}
		}
		return k8snode.Result{Stdout: fmt.Sprintf("boot-%d\n", h.boots)}
	case strings.Contains(remote, "systemctl stop"):
		f.events = append(f.events, "stop "+host)
		if h.node && f.nodeBusy {
			f.overlaps = append(f.overlaps, "runner stop of node "+host+" during a node reboot")
		}
		f.begin(host)
		if f.stopCancels {
			f.cancel()
		}
		if h.stopFails {
			return k8snode.Result{Status: 1}
		}
		for unit := range h.runners {
			if strings.Contains(remote, "'"+unit+"'") {
				h.runners[unit] = "inactive"
			}
		}
		return k8snode.Result{}
	case strings.Contains(remote, "systemctl start"):
		f.events = append(f.events, "start "+host)
		for unit := range h.runners {
			if strings.Contains(remote, "'"+unit+"'") {
				h.runners[unit] = "active"
			}
		}
		return k8snode.Result{}
	case strings.Contains(remote, "systemctl reboot"):
		f.events = append(f.events, "reboot "+host)
		f.begin(host)
		if h.rebootFails {
			return k8snode.Result{Stderr: "Failed to reboot\n", Status: 1}
		}
		if f.rebootCancels {
			f.cancel()
		}
		if h.rebootLost {
			return k8snode.Result{Status: 255}
		}
		f.boot(h)
		h.down = 1
		if h.neverReturns {
			h.down = 1 << 30
		}
		return k8snode.Result{Status: 255}
	}
	f.t.Errorf("unexpected remote command: %v", argv)
	return k8snode.Result{Status: 97}
}

// boot reboots a fake host; the lock is held.
func (f *rebootFake) boot(h *fakeHost) {
	h.boots++
	h.verdict = h.after
	h.cordoned = false
	for unit := range h.runners {
		if !h.runnersStayDown {
			h.runners[unit] = "active"
		}
	}
}

// nodeReboot stands in for the k8snode reboot, and writes lines in pieces.
func (f *rebootFake) nodeReboot(ctx context.Context, opts k8snode.Options, runner *k8snode.Runner) int {
	f.lock.Lock()
	if f.nodeBusy {
		f.overlaps = append(f.overlaps, "node reboots of "+opts.Node)
	}
	f.nodeBusy = true
	f.nodeOptions = append(f.nodeOptions, opts)
	f.lock.Unlock()
	defer func() {
		f.lock.Lock()
		f.nodeBusy = false
		f.lock.Unlock()
	}()

	fmt.Fprintf(runner.Stdout, "==> checking Kubernetes node %s\n", opts.Node)
	io.WriteString(runner.Stderr, "a line written ")
	io.WriteString(runner.Stderr, "in two parts\nand one without a newline")
	if opts.CheckOnly {
		f.lock.Lock()
		f.events = append(f.events, "check "+opts.Node)
		f.lock.Unlock()
		return 0
	}
	time.Sleep(2 * time.Millisecond)
	f.lock.Lock()
	defer f.lock.Unlock()
	f.events = append(f.events, "node "+opts.Node)
	f.begin(opts.Node)
	h := f.hosts[opts.Node]
	if h.nodeFails {
		return 1
	}
	f.boot(h)
	return 0
}

// fleet prepares a run with the arguments of nixfleet reboot.
func (f *rebootFake) fleet(args ...string) (*fleetReboot, []string) {
	f.t.Helper()
	opts, err := parseRebootArgs(args)
	if err != nil {
		f.t.Fatal(err)
	}
	status := &statusCheck{inv: f.inv, self: f.self, connectTimeout: "5", run: f.statusRun}
	r := newFleetReboot(f.inv, f.self, opts, status)
	r.out = &f.out
	r.errOut = &f.errOut
	r.in = strings.NewReader(f.confirmation)
	r.terminal = f.terminal
	r.sleep = func(ctx context.Context, d time.Duration) error { return ctx.Err() }
	r.fake = f.command
	r.nodeReboot = f.nodeReboot
	hosts, err := selectHosts(f.inv, f.self, statusOptions{all: opts.all, hosts: opts.hosts})
	if err != nil {
		f.t.Fatal(err)
	}
	return r, hosts
}

func (f *rebootFake) run(args ...string) int {
	f.t.Helper()
	r, hosts := f.fleet(args...)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.cancel = cancel
	return r.run(ctx, hosts)
}

// summary reads the outcome of each host from the summary table, and the
// notes below it.
func (f *rebootFake) summary() (map[string]string, map[string]string) {
	f.t.Helper()
	lines := strings.Split(f.out.String(), "\n")
	start := slices.Index(lines, fmt.Sprintf("%-12s %-12s %-12s %s", "host", "group", "outcome", "duration"))
	if start < 0 {
		f.t.Fatalf("no summary in:\n%s", f.out.String())
	}
	outcomes, notes := map[string]string{}, map[string]string{}
	for _, line := range lines[start+1:] {
		if line == "" {
			continue
		}
		if host, note, ok := strings.Cut(line, ": "); ok {
			notes[host] = note
			continue
		}
		outcomes[strings.TrimSpace(line[:12])] = strings.TrimSpace(line[26:39])
	}
	return outcomes, notes
}

func (f *rebootFake) expectOutcomes(want map[string]string) {
	f.t.Helper()
	got, notes := f.summary()
	for host, outcome := range want {
		if got[host] != outcome {
			f.t.Errorf("%s: got %q (%s), want %q", host, got[host], notes[host], outcome)
		}
	}
}

func (f *rebootFake) expectNote(host, text string) {
	f.t.Helper()
	if _, notes := f.summary(); !strings.Contains(notes[host], text) {
		f.t.Errorf("%s: note %q does not contain %q", host, notes[host], text)
	}
}

// changes are the recorded events that change a host.
func (f *rebootFake) changes() []string {
	var changes []string
	for _, event := range f.events {
		if !strings.HasPrefix(event, "ready ") && !strings.HasPrefix(event, "check ") {
			changes = append(changes, event)
		}
	}
	return changes
}

func TestRebootArguments(t *testing.T) {
	t.Setenv("NIX_DEPLOY_CONFIG", "/env.json")
	opts, err := parseRebootArgs([]string{"host-a", "--drain-timeout", "5m", "--ssh-timeout", "60", "--needed", "--config=/x.json", "alias-a", "--yes"})
	if err != nil || opts.configFile != "/x.json" || !slices.Equal(opts.hosts, []string{"host-a", "alias-a"}) || !opts.needed || !opts.yes {
		t.Fatalf("got %+v, %v", opts, err)
	}
	if opts.node.DrainTimeout != "5m" || opts.node.SSHTimeoutSeconds != 60 || !opts.node.Needed {
		t.Errorf("node options: %+v", opts.node)
	}
	cases := []struct {
		args []string
		want string
	}{
		{nil, "no host given"},
		{[]string{"--dry-run"}, "no host given"},
		{[]string{"--all", "host-a"}, "cannot be combined"},
		{[]string{"--node", "x", "host-a"}, "--node comes from the inventory"},
		{[]string{"--ssh-target", "root@x", "host-a"}, "--ssh-target comes from the inventory"},
		{[]string{"--bogus", "host-a"}, "unknown option: --bogus"},
		{[]string{"host-a", "--drain-timeout"}, "--drain-timeout requires a value"},
		{[]string{"--ready-timeout", "soon", "host-a"}, "unsupported duration: soon"},
		{[]string{"--resume-maintenance", "host-a", "host-b"}, "--resume-maintenance needs exactly one host"},
		{[]string{"--resume-maintenance", "--all"}, "--resume-maintenance needs exactly one host"},
		{[]string{"--resume-maintenance", "--needed", "host-a"}, "cannot be combined"},
		{[]string{"--config"}, "--config requires a file"},
	}
	for _, c := range cases {
		if _, err := parseRebootArgs(c.args); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: got %v, want %q", c.args, err, c.want)
		}
	}
	if _, err := parseRebootArgs([]string{"host-a", "--help"}); err != errRebootHelp {
		t.Errorf("help: %v", err)
	}
	if opts, err := parseRebootArgs([]string{"--resume-maintenance", "--check-only", "server-a"}); err != nil || !opts.node.ResumeMaintenance || !opts.node.CheckOnly {
		t.Errorf("resume one host: %+v, %v", opts, err)
	}
}

func memberNames(g *rebootGroup) []string {
	var names []string
	for _, t := range g.members {
		names = append(names, t.host)
	}
	return names
}

func TestRebootPlanGroupsAndOrder(t *testing.T) {
	cases := []struct {
		name      string
		primaries map[string]int
		want      []string
	}{
		{"no primaries", nil, []string{"server-a", "server-b", "server-c"}},
		{"primaries last", map[string]int{"server-a": 2, "server-b": 0, "server-c": 1}, []string{"server-b", "server-c", "server-a"}},
		{"ties in inventory order", map[string]int{"server-a": 1, "server-c": 1, "server-d": 5}, []string{"server-b", "server-a", "server-c"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newRebootFake(t)
			f.primaries = c.primaries
			r, hosts := f.fleet("--all")
			p := r.plan(hosts)
			var names []string
			for _, g := range p.groups {
				names = append(names, g.name)
			}
			if !slices.Equal(names, []string{"serial-1", "server-d", "serial-2", "host-a", "host-b", "mac"}) {
				t.Errorf("groups: %v", names)
			}
			if got := memberNames(p.groups[0]); !slices.Equal(got, c.want) {
				t.Errorf("serial-1 order: %v", got)
			}
			if got := memberNames(p.groups[2]); !slices.Equal(got, []string{"dns-a", "dns-b"}) {
				t.Errorf("serial-2 order: %v", got)
			}
			if len(p.notSelected) != 1 || p.notSelected[0].host != "op" || !strings.Contains(p.notSelected[0].skip, "operator host") {
				t.Errorf("not selected: %+v", p.notSelected)
			}
			if hosts, groups := p.size(); hosts != 8 || groups != 5 {
				t.Errorf("size: %d hosts in %d groups", hosts, groups)
			}
		})
	}
}

// A serial group's members keep the group's order, whatever order they are
// named in.
func TestRebootPlanNamedHosts(t *testing.T) {
	f := newRebootFake(t)
	r, hosts := f.fleet("dns-b", "alias-a", "dns-a")
	p := r.plan(hosts)
	if len(p.groups) != 2 || !slices.Equal(memberNames(p.groups[0]), []string{"dns-a", "dns-b"}) || !slices.Equal(memberNames(p.groups[1]), []string{"host-a"}) {
		t.Errorf("groups: %+v", p.groups)
	}
	if got := p.groups[1].members[0].policies(p.groups[1]); got != "plain host, runners forgejo-actions-runner.service" {
		t.Errorf("policies: %q", got)
	}
	if got := p.groups[0].members[0].policies(p.groups[0]); got != "plain host, ready gate" {
		t.Errorf("policies: %q", got)
	}
}

func TestRebootPlanDecisions(t *testing.T) {
	f := newRebootFake(t)
	f.hosts["host-a"].verdict = "no"
	f.hosts["host-b"].verdict = "unknown"
	f.hosts["server-a"].verdict = "no"
	f.hosts["server-a"].cordoned = true
	f.hosts["server-b"].verdict = "no"
	r, hosts := f.fleet("--all", "--needed")
	p := r.plan(hosts)
	decisions := map[string]*rebootTarget{}
	for _, g := range p.groups {
		for _, target := range g.members {
			decisions[target.host] = target
		}
	}
	for host, skip := range map[string]string{
		"host-a":   "no reboot needed: the current and next-boot systems use the booted kernel",
		"server-b": "no reboot needed",
		"mac":      "Darwin host, not NixOS",
		// An unknown verdict reboots, and so does a cordoned node.
		"host-b":   "",
		"server-a": "",
		"server-c": "",
	} {
		target := decisions[host]
		if target.fail != "" || skip == "" && target.skip != "" || !strings.HasPrefix(target.skip, skip) {
			t.Errorf("%s: skip %q, fail %q; want skip %q", host, target.skip, target.fail, skip)
		}
	}
	if decisions["server-a"].primaries != 0 || !decisions["server-a"].node || decisions["host-b"].node {
		t.Errorf("node policies: %+v %+v", decisions["server-a"], decisions["host-b"])
	}

	// Without --needed, a clear "no" reboots too.
	r, hosts = f.fleet("host-a")
	if target := r.plan(hosts).groups[0].members[0]; target.skip != "" || target.fail != "" {
		t.Errorf("host-a without --needed: %+v", target)
	}
}

func TestRebootRefusesOperatorAndSelf(t *testing.T) {
	f := newRebootFake(t)
	f.self = "host-a"
	if status := f.run("op", "alias-a", "host-b", "--yes"); status != 0 {
		t.Errorf("status %d\n%s", status, f.out.String())
	}
	f.expectOutcomes(map[string]string{"op": "skipped", "host-a": "skipped", "host-b": "rebooted"})
	f.expectNote("op", "refused: the operator host; its reboot handoff is not available yet")
	f.expectNote("host-a", "refused: this machine runs nixfleet")
	if got := f.changes(); !slices.Equal(got, []string{"reboot host-b"}) {
		t.Errorf("changes: %v", got)
	}
	// host-b logs in as admin and reboots through sudo.
	if !slices.Contains(f.commands, "admin@host-b sudo -n systemctl reboot --no-block") {
		t.Errorf("commands: %v", f.commands)
	}
}

func TestRebootReportsOtherSystems(t *testing.T) {
	f := newRebootFake(t)
	if status := f.run("mac", "--yes"); status != 0 {
		t.Errorf("status %d", status)
	}
	f.expectOutcomes(map[string]string{"mac": "skipped"})
	f.expectNote("mac", "Darwin host, not NixOS")
	if len(f.events) != 0 {
		t.Errorf("events: %v", f.events)
	}
}

// Plan failures fail their group before anything in it happens; the other
// groups go on.
func TestRebootPlanFailures(t *testing.T) {
	cases := []struct {
		name   string
		change func(*rebootFake)
		args   []string
		want   map[string]string
		host   string
		note   string
	}{
		{"unlisted cluster node", func(f *rebootFake) { f.hosts["dns-a"].node = true },
			[]string{"dns-a", "dns-b", "host-b"},
			map[string]string{"dns-a": "failed", "dns-b": "not started", "host-b": "rebooted"},
			"dns-a", "the cluster lists it as node dns-a, but kubernetesNodes does not"},
		{"unreachable host", func(f *rebootFake) { f.hosts["server-c"].unreachable = true },
			[]string{"server-a", "server-b", "server-c", "host-b"},
			map[string]string{"server-a": "not started", "server-b": "not started", "server-c": "failed", "host-b": "rebooted"},
			"server-c", "unreachable: ssh: connect to host port 22: No route to host"},
		{"primaries not read", func(f *rebootFake) { f.primariesFail = true },
			[]string{"server-a", "dns-a"},
			map[string]string{"server-a": "failed", "dns-a": "rebooted"},
			"server-a", "the database primaries cannot be read: error: the server is currently unable"},
		{"node missing from the cluster", func(f *rebootFake) { f.hosts["server-d"].node = false },
			[]string{"server-d", "host-b"},
			map[string]string{"server-d": "failed", "host-b": "rebooted"},
			"server-d", "kubernetesNodes lists it, but the cluster has no node server-d"},
		// Without the cluster's nodes, a plain host could be an unlisted node.
		{"cluster not read", func(f *rebootFake) { f.cluster = false },
			[]string{"server-d", "host-b", "mac"},
			map[string]string{"server-d": "failed", "host-b": "failed", "mac": "skipped"},
			"host-b", "whether it is a Kubernetes node cannot be told: The connection to the server was refused"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newRebootFake(t)
			c.change(f)
			if status := f.run(append(c.args, "--yes")...); status != 1 {
				t.Errorf("status %d", status)
			}
			f.expectOutcomes(c.want)
			f.expectNote(c.host, c.note)
			for _, event := range f.changes() {
				host := event[strings.Index(event, " ")+1:]
				if c.want[host] != "rebooted" {
					t.Errorf("%s changed: %v", host, f.events)
				}
			}
		})
	}
}

// Groups run concurrently: a failure stops its own group only, members of a
// serial group never overlap, and node reboots take turns across groups.
func TestRebootGroupsRunConcurrently(t *testing.T) {
	f := newRebootFake(t)
	f.hosts["server-b"].nodeFails = true
	if status := f.run("--all", "--yes"); status != 1 {
		t.Errorf("status %d", status)
	}
	f.expectOutcomes(map[string]string{
		"server-a": "rebooted", "server-b": "failed", "server-c": "not started", "server-d": "rebooted",
		"dns-a": "rebooted", "dns-b": "rebooted", "host-a": "rebooted", "host-b": "rebooted", "mac": "skipped",
	})
	f.expectNote("server-b", "the node reboot failed (status 1); the host is left as it is")
	f.expectNote("server-c", "not started: group stopped after server-b failed")
	if len(f.overlaps) != 0 {
		t.Errorf("overlaps: %v", f.overlaps)
	}
	for _, opts := range f.nodeOptions {
		if opts.Node != opts.Host || opts.SSHTarget != "root@"+opts.Host || opts.Needed || opts.CheckOnly || opts.SettleTimeout != "90m" {
			t.Errorf("node options: %+v", opts)
		}
	}
	// Each serial member waits for the others before it goes down and for
	// itself after it returns.
	var serial []string
	for _, event := range f.events {
		if strings.HasSuffix(event, "server-a") || strings.HasSuffix(event, "server-b") || strings.HasSuffix(event, "server-c") {
			serial = append(serial, event)
		}
	}
	want := []string{"ready server-b", "ready server-c", "node server-a", "ready server-a", "ready server-a", "ready server-c", "node server-b"}
	if !slices.Equal(serial, want) {
		t.Errorf("serial-1 events: %v", serial)
	}

	// Every line a host printed starts with its name.
	output := f.out.String()
	start := strings.Index(output, "==> rebooting 8 host(s) in 5 group(s)\n")
	end := strings.Index(output, "\nhost ")
	if start < 0 || end < start {
		t.Fatalf("output:\n%s", output)
	}
	prefixed := regexp.MustCompile(`^\[(server-[abcd]|dns-[ab]|host-[ab])\]\t`)
	lines := strings.Split(strings.TrimSpace(output[start:end]), "\n")[1:]
	for _, line := range lines {
		if !prefixed.MatchString(line) {
			t.Errorf("unprefixed line %q", line)
		}
	}
	for _, want := range []string{
		"[server-a]\ta line written in two parts\n",
		"[server-a]\tand one without a newline\n",
		"[dns-b]\tdns-a is ready\n",
		"[host-b]\t==> rebooting admin@host-b\n",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestRebootPreGateFailureStopsTheGroup(t *testing.T) {
	f := newRebootFake(t)
	f.hosts["dns-b"].notReady = true
	if status := f.run("dns-a", "dns-b", "--yes"); status != 1 {
		t.Errorf("status %d", status)
	}
	f.expectOutcomes(map[string]string{"dns-a": "failed", "dns-b": "not started"})
	f.expectNote("dns-a", "dns-b did not pass the ready gate within 25s (3 attempt(s)); dns-a was not rebooted")
	if len(f.changes()) != 0 {
		t.Errorf("changes: %v", f.changes())
	}
}

func TestRebootRunners(t *testing.T) {
	cases := []struct {
		name    string
		change  func(*rebootFake, *fakeHost)
		outcome string
		note    string
		changes []string
	}{
		{"stopped before the reboot", func(*rebootFake, *fakeHost) {}, "rebooted", "",
			[]string{"stop host-a", "reboot host-a"}},
		{"reboot command fails", func(_ *rebootFake, h *fakeHost) { h.rebootFails = true }, "failed",
			"the reboot command failed (ssh exited with status 1); the host was not rebooted",
			[]string{"stop host-a", "reboot host-a", "start host-a"}},
		{"stop fails", func(_ *rebootFake, h *fakeHost) { h.stopFails = true }, "failed",
			"could not stop the runner units (ssh exited with status 1)",
			[]string{"stop host-a", "start host-a"}},
		{"interrupted while stopping", func(f *rebootFake, _ *fakeHost) { f.stopCancels = true }, "failed",
			"interrupted", []string{"stop host-a", "start host-a"}},
		{"runners do not return", func(_ *rebootFake, h *fakeHost) { h.runnersStayDown = true }, "failed",
			"runner units not active within 5m0s after the reboot: forgejo-actions-runner.service",
			[]string{"stop host-a", "reboot host-a"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newRebootFake(t)
			h := f.hosts["host-a"]
			c.change(f, h)
			f.run("host-a", "--yes")
			f.expectOutcomes(map[string]string{"host-a": c.outcome})
			if c.note != "" {
				f.expectNote("host-a", c.note)
			}
			if got := f.changes(); !slices.Equal(got, c.changes) {
				t.Errorf("changes: %v", got)
			}
			// Only the active unit is stopped and started.
			for _, command := range f.commands {
				if strings.Contains(command, "systemctl stop") && command != "root@host-a systemctl stop 'forgejo-actions-runner.service'" ||
					strings.Contains(command, "systemctl start") && command != "root@host-a systemctl start 'forgejo-actions-runner.service'" {
					t.Errorf("command: %s", command)
				}
			}
		})
	}
}

// A node whose runners were stopped gets them back when the node reboot
// fails before the host went down.
func TestRebootNodeFailureStartsRunners(t *testing.T) {
	f := newRebootFake(t)
	h := f.hosts["server-d"]
	h.runners = map[string]string{"gitea-runner-ci.service": "active"}
	h.nodeFails = true
	f.run("server-d", "--yes")
	if got := f.changes(); !slices.Equal(got, []string{"stop server-d", "node server-d", "start server-d"}) {
		t.Errorf("changes: %v", got)
	}
}

func TestRebootChecksTheBootedSystem(t *testing.T) {
	f := newRebootFake(t)
	f.hosts["host-b"].after = "yes"
	f.hosts["dns-a"].after = "unknown"
	if status := f.run("host-b", "dns-a", "--yes"); status != 1 {
		t.Errorf("status %d", status)
	}
	f.expectOutcomes(map[string]string{"host-b": "failed", "dns-a": "rebooted"})
	f.expectNote("host-b", "did not boot the intended system: kernel differs")
	if !strings.Contains(f.out.String(), "[dns-a]\t==> warning: cannot tell whether dns-a booted the intended system: cannot compare initrd") {
		t.Errorf("no warning in:\n%s", f.out.String())
	}

	// A host that does not come back with a new boot ID fails.
	f = newRebootFake(t)
	f.hosts["host-b"].neverReturns = true
	f.run("host-b", "--yes", "--ssh-timeout", "30")
	f.expectNote("host-b", "admin@host-b did not return with a new boot ID within 30s")
}

func TestRebootDryRunChangesNothing(t *testing.T) {
	f := newRebootFake(t)
	if status := f.run("--all", "--dry-run"); status != 0 {
		t.Errorf("status %d", status)
	}
	if len(f.events) != 0 || len(f.commands) != 0 {
		t.Errorf("events %v, commands %v", f.events, f.commands)
	}
	output := f.out.String()
	for _, want := range []string{
		"plan: 8 host(s) to reboot in 5 group(s)",
		"group serial-1 (serial: server-a server-b server-c; ready gate: ready {host}):\n  1. server-a: Kubernetes node with 0 database primaries, ready gate\n",
		"group mac:\n  -  mac: skip: Darwin host, not NixOS\n",
		"not selected: op: the operator host and this machine; its reboot handoff is not available yet\n",
		"cluster: 4 nodes\n",
		"dry run: nothing was changed\n",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("missing %q in:\n%s", want, output)
		}
	}
}

func TestRebootConfirmation(t *testing.T) {
	cases := []struct {
		name     string
		terminal bool
		answer   string
		status   int
		changes  int
	}{
		{"no terminal", false, "", 2, 0},
		{"declined", true, "n\n", 2, 0},
		{"no answer", true, "", 2, 0},
		{"confirmed", true, "y\n", 0, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newRebootFake(t)
			f.terminal = c.terminal
			f.confirmation = c.answer
			if status := f.run("host-b"); status != c.status {
				t.Errorf("status %d", status)
			}
			if len(f.changes()) != c.changes {
				t.Errorf("changes: %v", f.changes())
			}
			if c.terminal && !strings.Contains(f.out.String(), "reboot 1 hosts in 1 groups? [y/N] ") {
				t.Errorf("no question in:\n%s", f.out.String())
			}
		})
	}
	f := newRebootFake(t)
	f.run("host-b")
	if !strings.Contains(f.errOut.String(), "pass --yes") {
		t.Errorf("stderr: %s", f.errOut.String())
	}
}

// --check-only changes nothing anywhere and needs no confirmation.
func TestRebootCheckOnly(t *testing.T) {
	f := newRebootFake(t)
	if status := f.run("server-a", "server-b", "host-a", "--check-only"); status != 0 {
		t.Errorf("status %d\n%s", status, f.out.String())
	}
	f.expectOutcomes(map[string]string{"server-a": "checked", "server-b": "checked", "host-a": "checked"})
	if got := f.changes(); len(got) != 0 {
		t.Errorf("changes: %v", got)
	}
	if !slices.Contains(f.events, "check server-a") || len(f.commands) != 0 {
		t.Errorf("events %v, commands %v", f.events, f.commands)
	}
}

// An interrupt before a member starts leaves it not started.
func TestRebootInterrupted(t *testing.T) {
	f := newRebootFake(t)
	r, hosts := f.fleet("host-a", "host-b", "--yes")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.run(ctx, hosts)
	f.expectOutcomes(map[string]string{"host-a": "not started", "host-b": "not started"})
	f.expectNote("host-a", "not started: interrupted")
	if len(f.events) != 0 {
		t.Errorf("events: %v", f.events)
	}
	if !errors.Is(pauseContext(ctx, time.Hour), errStopped) {
		t.Error("an interrupted pause must stop")
	}
}

// A Linux host without NixOS system links is reported, never rebooted.
func TestRebootReportsLinuxWithoutNixOS(t *testing.T) {
	f := newRebootFake(t)
	f.hosts["host-b"].noSystem = true
	if status := f.run("host-a", "host-b", "--yes"); status != 0 {
		t.Errorf("status %d\n%s", status, f.out.String())
	}
	f.expectOutcomes(map[string]string{"host-a": "rebooted", "host-b": "skipped"})
	f.expectNote("host-b", "no NixOS booted and current system to read")
	for _, event := range f.changes() {
		if strings.HasSuffix(event, " host-b") {
			t.Errorf("host-b changed: %v", f.changes())
		}
	}
}

// ssh's 255 can mean the reboot command never arrived. A host still on its
// old boot gets its runners back.
func TestRebootLostCommandStartsRunners(t *testing.T) {
	f := newRebootFake(t)
	f.hosts["host-a"].rebootLost = true
	if status := f.run("host-a", "--yes"); status != 1 {
		t.Errorf("status %d", status)
	}
	f.expectOutcomes(map[string]string{"host-a": "failed"})
	f.expectNote("host-a", "the host was not rebooted")
	if got := f.changes(); !slices.Equal(got, []string{"stop host-a", "reboot host-a", "start host-a"}) {
		t.Errorf("changes: %v", got)
	}
	if f.hosts["host-a"].runners["forgejo-actions-runner.service"] != "active" {
		t.Errorf("runners: %v", f.hosts["host-a"].runners)
	}
}

// A node's runners stop only once it has its turn among the node reboots.
func TestRebootNodeRunnersStopInTurn(t *testing.T) {
	f := newRebootFake(t)
	for _, host := range []string{"server-a", "server-d"} {
		f.hosts[host].runners = map[string]string{"forgejo-actions-runner.service": "active"}
	}
	if status := f.run("server-a", "server-d", "--yes"); status != 0 {
		t.Errorf("status %d\n%s", status, f.out.String())
	}
	if len(f.overlaps) != 0 {
		t.Errorf("overlaps: %v", f.overlaps)
	}
}

// An interrupt while the question waits for an answer declines it.
func TestRebootConfirmationInterrupted(t *testing.T) {
	f := newRebootFake(t)
	f.terminal = true
	r, hosts := f.fleet("host-b")
	answer, _ := io.Pipe()
	r.in = answer
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(10*time.Millisecond, cancel)
	if status := r.run(ctx, hosts); status != 2 {
		t.Errorf("status %d", status)
	}
	if len(f.changes()) != 0 {
		t.Errorf("changes: %v", f.changes())
	}
}

// An interrupt during the reboot wait restarts the runners of a host that
// is still on its old boot.
func TestRebootInterruptedWaitStartsRunners(t *testing.T) {
	f := newRebootFake(t)
	f.hosts["host-a"].rebootLost = true
	f.rebootCancels = true
	if status := f.run("host-a", "--yes"); status != 1 {
		t.Errorf("status %d", status)
	}
	if got := f.changes(); !slices.Equal(got, []string{"stop host-a", "reboot host-a", "start host-a"}) {
		t.Errorf("changes: %v", got)
	}
}
