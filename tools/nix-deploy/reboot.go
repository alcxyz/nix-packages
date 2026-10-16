package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"nix-deploy/k8snode"
)

const rebootUsage = `Usage: nixfleet reboot [--config FILE] [--all | <host>...] [--needed] [--dry-run]
                       [--yes] [node options]

Reboots the selected hosts, each under the policies the inventory gives it
(ADR-0010). Hosts may be named by alias; --all selects the hosts deploy --all
would. A run first reads every selected host as nixfleet status does, the
cluster's nodes and the nodes of its CloudNativePG primaries, and prints the
plan: groups, order, policies, and skips with their reasons. --dry-run stops
there. Otherwise a terminal is asked to confirm, and without one the run
needs --yes.

Policies:
  Kubernetes node   A host in the inventory's kubernetesNodes, whose node is
                    named like the host, reboots behind every gate of
                    k8s-node-power reboot, with the node options below. A
                    host the cluster lists as a node but kubernetesNodes does
                    not is refused. Node reboots never overlap, even across
                    groups.
  serial group      Before a member goes down, every other member of its
                    serial group must pass the group's readyCommand; after it
                    returns, the member itself must. Each gate is retried
                    every DEPLOY_SERIAL_READY_INTERVAL seconds (default 10)
                    within the group's readyTimeoutSeconds.
  runner host       Active Forgejo or Gitea runner units are stopped first,
                    which lets the runner finish its admitted jobs (for up to
                    3700 seconds) and admit no new ones. They are started
                    again when the host then is not rebooted, and must be
                    active within 5 minutes after it is.
  every host        The boot ID must change, and the host must then run the
                    system it was meant to boot (reboot no; unknown warns).
  not rebooted      The operator host and this machine, whose reboot handoff
                    is not available yet: named, they are refused; --all
                    leaves them out. Hosts that are not NixOS are reported.

Each serial group is a constraint group, and every other host a group of its
own. Groups run concurrently and the members of a group one at a time, in
inventory order, except that Kubernetes nodes holding fewer database
primaries go first. A group fails before anything in it changes when a host
cannot be read, a node is refused, or the cluster's nodes (or, for a group
with nodes, its primaries) cannot be read. A failure stops the rest of its
group and leaves the host as it is (a node cordoned and drained); the other
groups go on. Every line a host prints starts with its name, and a summary
of every host ends the run.

Options:
  --config FILE  The inventory; default NIX_DEPLOY_CONFIG.
  --all          Select the hosts deploy --all would.
  --needed       Skip hosts whose reboot verdict is no, and for a node that
                 is not cordoned; an unknown verdict reboots.
  --dry-run      Print the plan and change nothing.
  --yes          Reboot without asking.
  -h, --help     Show this help.

Node options are those of k8s-node-power reboot, applied to every Kubernetes
node: --skip-drain, --force-drain, --bypass-pdb, --leave-cordoned,
--resume-maintenance (with exactly one host), --check-only, --drain-timeout,
--ready-timeout, --fence-timeout, --settle-timeout, --ssh-timeout,
--require-longhorn-backup-target and --min-free-pod-slots. --node,
--ssh-target and --context come from the inventory; every cluster read and
node step uses its kubernetesContext, or the current context without one.
Two node options apply to the other hosts too:
--check-only reads their boot ID and runs their ready gate only, changing
nothing anywhere, and --ssh-timeout (default 900) bounds their return.

The exit status is 0 when no host failed and 1 when one did; 2 for invalid
arguments or a run that was not confirmed, 130 or 143 when interrupted.
`

// Bounds of the host steps of a fleet reboot. A runner's own shutdown
// timeout lets admitted jobs finish; the stop waits a little longer.
const (
	runnerStopTimeout   = 3700 * time.Second
	runnerStartTimeout  = 120 * time.Second
	runnerReturnTimeout = 5 * time.Minute
	bootIDTimeout       = 30 * time.Second
	rebootCommandBound  = 60 * time.Second
	sshDownAttempts     = 30
)

// Outcomes of a host in a fleet reboot.
const (
	outcomeRebooted   = "rebooted"
	outcomeChecked    = "checked"
	outcomeSkipped    = "skipped"
	outcomeFailed     = "failed"
	outcomeNotStarted = "not started"
)

// nodeValueOptions are the node options that take a value.
var nodeValueOptions = []string{"--drain-timeout", "--ready-timeout", "--fence-timeout", "--settle-timeout", "--ssh-timeout", "--min-free-pod-slots"}

// safeUnit is a systemd service name that can be passed to a remote shell in
// single quotes.
var safeUnit = regexp.MustCompile(`^[A-Za-z0-9@._:\\-]+\.service$`)

var (
	errRebootHelp = errors.New("help requested")
	errNoHosts    = errors.New("no host given; name hosts or pass --all")
	errStopped    = errors.New("interrupted")
)

type rebootOptions struct {
	configFile string
	all        bool
	hosts      []string
	needed     bool
	dryRun     bool
	yes        bool
	// node holds the validated node options every Kubernetes node starts
	// from.
	node k8snode.Options
}

func parseRebootArgs(args []string) (rebootOptions, error) {
	opts := rebootOptions{configFile: os.Getenv("NIX_DEPLOY_CONFIG")}
	var nodeArgs []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--all":
			opts.all = true
		case arg == "--needed":
			opts.needed = true
			nodeArgs = append(nodeArgs, arg)
		case arg == "--dry-run":
			opts.dryRun = true
		case arg == "--yes":
			opts.yes = true
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
			return opts, errRebootHelp
		case arg == "--node" || arg == "--ssh-target":
			return opts, fmt.Errorf("%s comes from the inventory: each node is named like its host and reached at its system endpoint", arg)
		case arg == "--context":
			return opts, fmt.Errorf("--context comes from the inventory's kubernetesContext, which the ready gates and status also use")
		case contains(nodeValueOptions, arg):
			if i+1 >= len(args) {
				return opts, fmt.Errorf("%s requires a value", arg)
			}
			nodeArgs = append(nodeArgs, arg, args[i+1])
			i++
		case strings.HasPrefix(arg, "-"):
			nodeArgs = append(nodeArgs, arg)
		default:
			opts.hosts = append(opts.hosts, arg)
		}
	}
	if opts.all && len(opts.hosts) > 0 {
		return opts, fmt.Errorf("--all and host names cannot be combined")
	}
	if !opts.all && len(opts.hosts) == 0 {
		return opts, errNoHosts
	}
	// The node options are checked as one node's, before anything is read;
	// each node then gets its own host, node and SSH target.
	node, err := k8snode.ParseArgs("reboot", append(nodeArgs, "host"))
	if err != nil {
		return opts, err
	}
	if node.ResumeMaintenance && (opts.all || len(opts.hosts) != 1) {
		return opts, fmt.Errorf("--resume-maintenance needs exactly one host")
	}
	opts.node = node
	return opts, nil
}

// runReboot is nixfleet reboot.
func runReboot(args []string) int {
	opts, err := parseRebootArgs(args)
	if errors.Is(err, errRebootHelp) {
		fmt.Print(rebootUsage)
		return 0
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "nixfleet reboot: %v\n", err)
		fmt.Fprint(os.Stderr, rebootUsage)
		return 2
	}
	if opts.configFile == "" {
		fmt.Fprintln(os.Stderr, "nixfleet reboot: no inventory configured; pass --config FILE or set NIX_DEPLOY_CONFIG")
		return 1
	}
	inv, err := loadInventory(opts.configFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "nixfleet reboot: %v\n", err)
		return 1
	}
	values, err := loadSettings()
	if err != nil {
		fmt.Fprintf(os.Stderr, "nixfleet reboot: %v\n", err)
		return 1
	}
	hostname, status := capture(0, "hostname", "-s")
	if status != 0 {
		fmt.Fprintln(os.Stderr, "nixfleet reboot: cannot determine the local host name")
		return 1
	}
	self := inv.canonical(strings.TrimSpace(hostname))
	hosts, err := selectHosts(inv, self, statusOptions{all: opts.all, hosts: opts.hosts})
	if err != nil {
		fmt.Fprintf(os.Stderr, "nixfleet reboot: %v\n", err)
		return 2
	}

	sshOptions := os.Getenv("NIX_SSHOPTS")
	if sshOptions == "" {
		sshOptions = defaultSSHOptions
	}
	f := newFleetReboot(inv, self, opts, &statusCheck{
		inv:            inv,
		self:           self,
		sshArgs:        strings.Fields(sshOptions),
		connectTimeout: values.sshCheckTimeout,
		run:            runCommand,
	})
	f.ui = newUI(inv.HostColors)
	f.readyInterval = values.readyInterval
	f.terminal = stdinIsTerminal()

	// An interrupt stops new steps; a node operation in progress cleans up
	// as k8s-node-power does, and runner units stopped for a reboot that
	// did not start are started again.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	received := make(chan os.Signal, 1)
	go func() {
		sig := <-signals
		received <- sig
		cancel()
	}()

	status = f.run(ctx, hosts)
	select {
	case sig := <-received:
		if sig == syscall.SIGTERM {
			return 143
		}
		return 130
	default:
		return status
	}
}

// fleetReboot is one run of nixfleet reboot.
type fleetReboot struct {
	inv    *inventory
	self   string
	opts   rebootOptions
	status *statusCheck
	ui     *ui

	out    io.Writer
	errOut io.Writer
	// in answers the confirmation when terminal says it is a terminal.
	in       io.Reader
	terminal bool

	readyInterval time.Duration
	sshPoll       time.Duration
	runnerPoll    time.Duration
	// sleep pauses between polls; tests make it instant.
	sleep func(context.Context, time.Duration) error
	// fake, when set, answers the host and ready commands instead of
	// running them; tests set it.
	fake func(k8snode.Command) k8snode.Result
	// nodeReboot reboots a Kubernetes node; tests replace it.
	nodeReboot func(context.Context, k8snode.Options, *k8snode.Runner) int

	// outputLock keeps lines from concurrent hosts whole.
	outputLock sync.Mutex
	// nodeLock keeps node reboots from overlapping, even across groups: two
	// nodes drained at once could leave the cluster without quorum or
	// storage replicas.
	nodeLock sync.Mutex
}

func newFleetReboot(inv *inventory, self string, opts rebootOptions, status *statusCheck) *fleetReboot {
	return &fleetReboot{
		inv:           inv,
		self:          self,
		opts:          opts,
		status:        status,
		ui:            &ui{},
		out:           os.Stdout,
		errOut:        os.Stderr,
		in:            os.Stdin,
		readyInterval: 10 * time.Second,
		sshPoll:       5 * time.Second,
		runnerPoll:    10 * time.Second,
		sleep:         pauseContext,
		nodeReboot: func(ctx context.Context, opts k8snode.Options, runner *k8snode.Runner) int {
			return k8snode.New(ctx, opts, runner).Run()
		},
	}
}

func pauseContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return errStopped
	case <-timer.C:
		return nil
	}
}

// rebootTarget is one host's part in a fleet reboot.
type rebootTarget struct {
	host string
	node bool
	// primaries counts the database primaries on the node; -1 when unread.
	primaries int
	// runners are the active runner units the reboot stops.
	runners []string
	// skip says why the host is not rebooted, fail why its group fails
	// before it starts.
	skip string
	fail string

	outcome  string
	reason   string
	duration time.Duration
}

// rebootGroup is a constraint group: a serial group's selected members, or
// a host of its own.
type rebootGroup struct {
	// name is serial-N for the inventory's Nth serial group, or the host.
	name    string
	serial  *serialGroup
	members []*rebootTarget
}

// failed reports whether the group fails before it starts.
func (g *rebootGroup) failed() bool {
	for _, t := range g.members {
		if t.fail != "" {
			return true
		}
	}
	return false
}

// label names the group in the summary; a host of its own has none.
func (g *rebootGroup) label() string {
	if g.serial == nil {
		return "-"
	}
	return g.name
}

type rebootPlan struct {
	groups []*rebootGroup
	// notSelected are the hosts --all leaves out, with the reason.
	notSelected []*rebootTarget
	cluster     clusterStatus
}

// size counts the hosts that will be rebooted and the groups they are in.
func (p *rebootPlan) size() (int, int) {
	hosts, groups := 0, 0
	for _, g := range p.groups {
		if g.failed() {
			continue
		}
		counted := false
		for _, t := range g.members {
			if t.skip == "" {
				hosts++
				counted = true
			}
		}
		if counted {
			groups++
		}
	}
	return hosts, groups
}

// run plans the reboot, asks for confirmation, runs the groups and prints the
// summary. It returns the exit status.
func (f *fleetReboot) run(ctx context.Context, hosts []string) int {
	p := f.plan(hosts)
	f.printPlan(p)
	if f.opts.dryRun {
		fmt.Fprintln(f.out, "dry run: nothing was changed")
		return 0
	}
	count, groups := p.size()
	if count > 0 && !f.opts.node.CheckOnly {
		if status := f.confirm(ctx, count, groups); status != 0 {
			return status
		}
	}
	verb := "rebooting"
	if f.opts.node.CheckOnly {
		verb = "checking"
	}
	fmt.Fprintf(f.out, "==> %s %d host(s) in %d group(s)\n", verb, count, groups)
	var wait sync.WaitGroup
	for _, g := range p.groups {
		wait.Add(1)
		go func() {
			defer wait.Done()
			f.runGroup(ctx, g)
		}()
	}
	wait.Wait()
	if f.printSummary(p) {
		return 1
	}
	return 0
}

// confirm asks on the terminal unless --yes was given. An interrupt while
// it waits for the answer declines.
func (f *fleetReboot) confirm(ctx context.Context, hosts, groups int) int {
	if f.opts.yes {
		return 0
	}
	if !f.terminal {
		fmt.Fprintln(f.errOut, "nixfleet reboot: standard input is not a terminal to confirm on; pass --yes to reboot without asking")
		return 2
	}
	fmt.Fprintf(f.out, "reboot %d hosts in %d groups? [y/N] ", hosts, groups)
	answers := make(chan string, 1)
	go func() {
		answer, _ := bufio.NewReader(io.LimitReader(f.in, 64)).ReadString('\n')
		answers <- answer
	}()
	select {
	case answer := <-answers:
		switch strings.ToLower(strings.TrimSpace(answer)) {
		case "y", "yes":
			return 0
		}
	case <-ctx.Done():
		fmt.Fprintln(f.out)
	}
	fmt.Fprintln(f.errOut, "nixfleet reboot: not confirmed; nothing was changed")
	return 2
}

// stdinIsTerminal tells whether standard input is a terminal to ask on: a
// character device other than the null device. Any other device can only
// decline, since the answer is read in a few bytes.
func stdinIsTerminal() bool {
	info, err := os.Stdin.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	null, err := os.Stat(os.DevNull)
	return err == nil && !os.SameFile(info, null)
}

// excluded says why a host is never rebooted by the fleet, or "".
func (f *fleetReboot) excluded(host string) string {
	const handoff = "its reboot handoff is not available yet"
	switch {
	case host == f.self && host == f.inv.OperatorHost:
		return "the operator host and this machine; " + handoff
	case host == f.self:
		return "this machine runs nixfleet; " + handoff
	case host == f.inv.OperatorHost:
		return "the operator host; " + handoff
	}
	return ""
}

// plan reads the selected hosts and the cluster and decides, for each host,
// its group, its place in it and its policies, or why it is skipped or its
// group fails.
func (f *fleetReboot) plan(hosts []string) *rebootPlan {
	p := &rebootPlan{}
	var targets []*rebootTarget
	var read []string
	for _, host := range hosts {
		t := &rebootTarget{host: host, node: f.inv.isKubernetesNode(host), primaries: -1}
		if reason := f.excluded(host); reason != "" {
			if f.opts.all {
				t.skip = reason
				p.notSelected = append(p.notSelected, t)
				continue
			}
			t.skip = "refused: " + reason
		} else {
			read = append(read, host)
		}
		targets = append(targets, t)
	}

	report := f.status.collect(read)
	p.cluster = report.Cluster
	states := map[string]hostStatus{}
	for _, h := range report.Hosts {
		states[h.Host] = h
	}
	for _, t := range targets {
		if t.skip == "" {
			f.decide(t, states[t.host], report.Cluster)
		}
	}

	bySerial := map[*serialGroup]*rebootGroup{}
	for _, t := range targets {
		serial := f.inv.group(t.host)
		if serial == nil {
			p.groups = append(p.groups, &rebootGroup{name: t.host, members: []*rebootTarget{t}})
			continue
		}
		g, ok := bySerial[serial]
		if !ok {
			g = &rebootGroup{name: f.serialName(serial), serial: serial}
			bySerial[serial] = g
			p.groups = append(p.groups, g)
		}
		g.members = append(g.members, t)
	}
	for _, g := range p.groups {
		if g.serial != nil {
			hosts := g.serial.Hosts
			sort.SliceStable(g.members, func(i, j int) bool {
				return indexOf(hosts, g.members[i].host) < indexOf(hosts, g.members[j].host)
			})
		}
	}
	f.orderNodes(p)
	return p
}

// decide sets a read host's policies, or why it is skipped or its group
// fails. A clear "no" verdict skips only with --needed, and a node only when
// it is not cordoned: a cordoned node is in unfinished maintenance.
func (f *fleetReboot) decide(t *rebootTarget, h hostStatus, cluster clusterStatus) {
	switch {
	case !h.Reachable:
		t.fail = "unreachable: " + h.Error
		return
	case h.OS == "":
		t.fail = h.RebootReason
		return
	case h.OS != "Linux":
		t.skip = h.RebootReason
		return
	case h.BootedSystem == "" || h.CurrentSystem == "":
		// A Linux host without the booted and current system links is not
		// NixOS, or not one the policies can verify after a reboot.
		t.skip = "no NixOS booted and current system to read; reported only"
		return
	case !t.node && h.Node != nil:
		t.fail = fmt.Sprintf("the cluster lists it as node %s, but kubernetesNodes does not; refusing a reboot without the node gates", h.Node.Name)
		return
	case f.opts.needed && h.RebootNeeded != nil && !*h.RebootNeeded && (!t.node || h.Node != nil && !h.Node.Cordoned):
		t.skip = "no reboot needed: " + h.RebootReason
		return
	case !cluster.Reachable && t.node:
		t.fail = "the cluster's nodes cannot be read: " + cluster.Error
		return
	case !cluster.Reachable:
		t.fail = "the cluster's nodes cannot be read, so whether it is a Kubernetes node cannot be told: " + cluster.Error
		return
	case t.node && h.Node == nil:
		t.fail = "kubernetesNodes lists it, but the cluster has no node " + t.host
		return
	case t.node && h.Node.Name != t.host:
		t.fail = fmt.Sprintf("its node is named %s; kubernetesNodes needs node names equal to host names", h.Node.Name)
		return
	}
	if h.Runners == nil {
		return
	}
	for _, unit := range h.Runners.Units {
		if unit.State != "active" {
			continue
		}
		if !safeUnit.MatchString(unit.Unit) {
			t.fail = fmt.Sprintf("runner unit name %q cannot be passed to systemctl safely", unit.Unit)
			return
		}
		t.runners = append(t.runners, unit.Unit)
	}
}

// orderNodes puts the Kubernetes nodes of each group that holds fewer
// database primaries first, so a primary moves once, to a node that already
// rebooted. Nodes with equal counts, and the other hosts, keep the inventory
// order. A group whose primaries cannot be read fails.
func (f *fleetReboot) orderNodes(p *rebootPlan) {
	var counts map[string]int
	var readErr error
	read := false
	for _, g := range p.groups {
		var slots []int
		var nodes []*rebootTarget
		for i, t := range g.members {
			if t.node && t.skip == "" && t.fail == "" {
				slots = append(slots, i)
				nodes = append(nodes, t)
			}
		}
		if len(nodes) == 0 {
			continue
		}
		if !read {
			counts, readErr = f.readPrimaries()
			read = true
		}
		if readErr != nil {
			for _, t := range nodes {
				t.fail = "the database primaries cannot be read: " + readErr.Error()
			}
			continue
		}
		for _, t := range nodes {
			t.primaries = counts[t.host]
		}
		sort.SliceStable(nodes, func(i, j int) bool { return nodes[i].primaries < nodes[j].primaries })
		for i, slot := range slots {
			g.members[slot] = nodes[i]
		}
	}
}

// readPrimaries counts the CloudNativePG primary Pods on each node.
func (f *fleetReboot) readPrimaries() (map[string]int, error) {
	stdout, stderr, status := f.status.run(kubectlTimeout, f.status.kubectl("get", "pods", "-A", "-l", "cnpg.io/instanceRole=primary", "-o", "json", "--request-timeout=10s")...)
	switch {
	case status == exitTimedOut:
		return nil, fmt.Errorf("kubectl did not answer within %s", kubectlTimeout)
	case status != 0:
		message := oneLine(lastLine(stderr))
		if message == "" {
			message = fmt.Sprintf("kubectl exited with status %d", status)
		}
		return nil, errors.New(message)
	}
	var list struct {
		Items []struct {
			Spec struct {
				NodeName string `json:"nodeName"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(stdout), &list); err != nil {
		return nil, fmt.Errorf("kubectl output does not decode: %v", err)
	}
	counts := map[string]int{}
	for _, item := range list.Items {
		counts[item.Spec.NodeName]++
	}
	return counts, nil
}

func (f *fleetReboot) serialName(serial *serialGroup) string {
	for i, g := range f.inv.SerialGroups {
		if g == serial {
			return fmt.Sprintf("serial-%d", i+1)
		}
	}
	return "serial"
}

func indexOf(values []string, value string) int {
	for i, item := range values {
		if item == value {
			return i
		}
	}
	return len(values)
}

// policies describes what a host's reboot involves.
func (t *rebootTarget) policies(g *rebootGroup) string {
	parts := []string{"plain host"}
	if t.node {
		parts[0] = "Kubernetes node"
		if t.primaries >= 0 {
			parts[0] = fmt.Sprintf("Kubernetes node with %d database primaries", t.primaries)
		}
	}
	if g.serial != nil && g.serial.ReadyCommand != "" {
		parts = append(parts, "ready gate")
	}
	if len(t.runners) > 0 {
		parts = append(parts, "runners "+strings.Join(t.runners, " "))
	}
	return strings.Join(parts, ", ")
}

func (f *fleetReboot) printPlan(p *rebootPlan) {
	count, groups := p.size()
	fmt.Fprintf(f.out, "plan: %d host(s) to reboot in %d group(s); groups run concurrently, members of a group one at a time\n", count, groups)
	for _, g := range p.groups {
		header := "group " + g.name
		if g.serial != nil {
			header += " (serial: " + strings.Join(g.serial.Hosts, " ")
			if g.serial.ReadyCommand != "" {
				header += "; ready gate: " + g.serial.ReadyCommand
			}
			header += ")"
		}
		if g.failed() {
			header += ", fails before it starts"
		}
		fmt.Fprintln(f.out, header+":")
		step := 0
		for _, t := range g.members {
			switch {
			case t.skip != "":
				fmt.Fprintf(f.out, "  -  %s: skip: %s\n", t.host, t.skip)
			case t.fail != "":
				fmt.Fprintf(f.out, "  -  %s: fail: %s\n", t.host, t.fail)
			case g.failed():
				fmt.Fprintf(f.out, "  -  %s: not started: %s\n", t.host, t.policies(g))
			default:
				step++
				fmt.Fprintf(f.out, "  %d. %s: %s\n", step, t.host, t.policies(g))
			}
		}
	}
	for _, t := range p.notSelected {
		fmt.Fprintf(f.out, "not selected: %s: %s\n", t.host, t.skip)
	}
	if p.cluster.Reachable {
		fmt.Fprintf(f.out, "cluster: %d nodes\n", p.cluster.Nodes)
	} else {
		fmt.Fprintf(f.out, "cluster: not read: %s\n", p.cluster.Error)
	}
}

// runGroup runs a group's members one at a time; a failure stops the rest.
func (f *fleetReboot) runGroup(ctx context.Context, g *rebootGroup) {
	stopped := ""
	if g.failed() {
		stopped = "group failed before it started"
	}
	for _, t := range g.members {
		switch {
		case t.skip != "":
			t.outcome, t.reason = outcomeSkipped, t.skip
		case t.fail != "":
			t.outcome, t.reason = outcomeFailed, t.fail
		case stopped != "":
			t.outcome, t.reason = outcomeNotStarted, stopped
		case ctx.Err() != nil:
			t.outcome, t.reason = outcomeNotStarted, "interrupted"
		default:
			start := time.Now()
			h := f.newHostReboot(ctx, g, t)
			t.outcome, t.reason = h.run()
			h.out.flush()
			t.duration = time.Since(start)
			if t.outcome == outcomeFailed {
				stopped = "group stopped after " + t.host + " failed"
			}
		}
	}
}

// printSummary prints every host's outcome and reports whether one failed.
func (f *fleetReboot) printSummary(p *rebootPlan) bool {
	format := "%-12s %-12s %-12s %s\n"
	fmt.Fprintln(f.out)
	fmt.Fprintf(f.out, format, "host", "group", "outcome", "duration")
	failed := false
	var notes []string
	for _, g := range p.groups {
		for _, t := range g.members {
			duration := "-"
			if t.duration > 0 {
				duration = t.duration.Round(time.Second).String()
			}
			fmt.Fprintf(f.out, format, t.host, g.label(), t.outcome, duration)
			if t.reason != "" {
				notes = append(notes, t.host+": "+t.outcome+": "+t.reason)
			}
			if t.outcome == outcomeFailed {
				failed = true
			}
		}
	}
	for _, note := range notes {
		fmt.Fprintln(f.out, note)
	}
	return failed
}

// hostReboot is one host's reboot sequence. Its output goes through a writer
// that starts every line with the host's name.
type hostReboot struct {
	f      *fleetReboot
	ctx    context.Context
	group  *rebootGroup
	target *rebootTarget
	out    *prefixWriter
	runner *k8snode.Runner
	// bootID is the host's boot ID before the reboot.
	bootID string
	// stopped are the runner units this run stopped.
	stopped []string
}

func (f *fleetReboot) newHostReboot(ctx context.Context, g *rebootGroup, t *rebootTarget) *hostReboot {
	out := &prefixWriter{lock: &f.outputLock, dst: f.out, prefix: f.ui.host(t.host)}
	return &hostReboot{
		f:      f,
		ctx:    ctx,
		group:  g,
		target: t,
		out:    out,
		runner: &k8snode.Runner{Stdout: out, Stderr: out, Fake: f.fake},
	}
}

func (h *hostReboot) log(format string, args ...any) {
	fmt.Fprintf(h.out, "==> "+format+"\n", args...)
}

// run is the host's sequence: boot ID, ready gate of the other members,
// runner stop, reboot and the checks after it. It returns the outcome and
// its reason.
func (h *hostReboot) run() (string, string) {
	t := h.target
	h.log("%s: %s", t.host, t.policies(h.group))
	if t.node {
		// Node reboots take turns across groups. The turn is taken before
		// the ready gate and the runner stop, so neither waits on another
		// node, and held until this node passed its checks after the boot.
		h.f.nodeLock.Lock()
		defer h.f.nodeLock.Unlock()
		if h.ctx.Err() != nil {
			return h.failure(errStopped)
		}
	}
	h.bootID = h.readBootID(h.ctx)
	if h.bootID == "" {
		return h.failure(errors.New("cannot read the boot ID over SSH"))
	}
	if serial := h.group.serial; serial != nil && serial.ReadyCommand != "" {
		for _, other := range serial.others(t.host) {
			if err := h.waitReady(other); err != nil {
				return h.failure(fmt.Errorf("%v; %s was not rebooted", err, t.host))
			}
		}
	}
	if h.f.opts.node.CheckOnly {
		if t.node {
			if status := h.runNode(); status != 0 {
				return h.failure(fmt.Errorf("the node checks failed (status %d)", status))
			}
		}
		h.log("checks passed; nothing was changed")
		return outcomeChecked, ""
	}
	if err := h.stopRunners(); err != nil {
		h.startRunners()
		return h.failure(err)
	}
	if t.node {
		rebooted, err := h.rebootNode()
		if err != nil {
			return h.failure(err)
		}
		if !rebooted {
			return outcomeSkipped, "the node reboot found no reboot needed"
		}
	} else if err := h.rebootPlain(); err != nil {
		return h.failure(err)
	}
	if err := h.verify(); err != nil {
		return h.failure(err)
	}
	h.log("%s rebooted", t.host)
	return outcomeRebooted, ""
}

func (h *hostReboot) failure(err error) (string, string) {
	if h.ctx.Err() != nil && !errors.Is(err, errStopped) {
		err = fmt.Errorf("interrupted: %v", err)
	}
	h.log("error: %v", err)
	return outcomeFailed, err.Error()
}

// sshCommand runs remote on the host as its system user, with BatchMode.
func (h *hostReboot) sshCommand(connectTimeout, remote string) []string {
	argv := []string{"ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=" + connectTimeout}
	argv = append(argv, h.f.status.sshArgs...)
	return append(argv, h.f.inv.systemEndpoint(h.target.host), remote)
}

// privileged runs a systemctl command line through noninteractive sudo on
// hosts whose system user is not root.
func (h *hostReboot) privileged(command string) string {
	if h.f.inv.usesRemoteSudo(h.target.host) {
		return "sudo -n " + command
	}
	return command
}

// probeBootID reads the boot ID once and returns it with ssh's status.
func (h *hostReboot) probeBootID(ctx context.Context, connectTimeout string) (string, int) {
	r := h.runner.Run(ctx, k8snode.Command{
		Argv:    h.sshCommand(connectTimeout, "cat /proc/sys/kernel/random/boot_id"),
		Quiet:   true,
		Timeout: bootIDTimeout,
	})
	return strings.TrimSpace(r.Stdout), r.Status
}

// readBootID returns the boot ID, or "" when it cannot be read.
func (h *hostReboot) readBootID(ctx context.Context) string {
	id, status := h.probeBootID(ctx, "10")
	if status != 0 {
		return ""
	}
	return id
}

// waitClock measures a wait as the larger of the wall-clock time and the
// pauses it asked for, as deploy's ready gate does, so its deadline also
// holds when tests make pauses instant.
type waitClock struct {
	start time.Time
	spent time.Duration
}

func (c *waitClock) elapsed() time.Duration {
	if wall := time.Since(c.start); wall > c.spent {
		c.spent = wall
	}
	return c.spent
}

// pause waits d, or fails once the run is interrupted.
func (h *hostReboot) pause(clock *waitClock, d time.Duration) error {
	if err := h.f.sleep(h.ctx, d); err != nil {
		return errStopped
	}
	clock.spent += d
	return nil
}

// waitReady runs the group's ready command for host on this machine until
// it passes or the group's timeout elapses, as deploy's ready gate does.
func (h *hostReboot) waitReady(host string) error {
	serial := h.group.serial
	timeout := time.Duration(serial.ReadyTimeout) * time.Second
	command := serial.readyCommand(host)
	h.log("ready gate for %s: %s", host, command)
	clock := waitClock{start: time.Now()}
	attempts := 0
	for {
		remaining := timeout - clock.elapsed()
		if remaining <= 0 {
			break
		}
		attempts++
		r := h.runner.Run(h.ctx, k8snode.Command{Argv: []string{"bash", "-c", command}, Timeout: remaining, Passthrough: true})
		if h.ctx.Err() != nil {
			return errStopped
		}
		if r.Status == 0 && clock.elapsed() <= timeout {
			h.log("%s passed the ready gate after %d attempt(s)", host, attempts)
			return nil
		}
		if timeout-clock.elapsed() <= h.f.readyInterval {
			break
		}
		if err := h.pause(&clock, h.f.readyInterval); err != nil {
			return err
		}
	}
	return fmt.Errorf("%s did not pass the ready gate within %ds (%d attempt(s))", host, serial.ReadyTimeout, attempts)
}

// quoteUnits quotes unit names that safeUnit accepted.
func quoteUnits(units []string) string {
	quoted := make([]string, len(units))
	for i, unit := range units {
		quoted[i] = "'" + unit + "'"
	}
	return strings.Join(quoted, " ")
}

// stopRunners stops the active runner units; a runner admits no new jobs
// once stopped and finishes those it admitted within its own timeout.
func (h *hostReboot) stopRunners() error {
	units := h.target.runners
	if len(units) == 0 {
		return nil
	}
	h.log("stopping %s; admitted jobs finish first (waiting up to %s)", strings.Join(units, " "), runnerStopTimeout)
	h.stopped = units
	r := h.runner.Run(h.ctx, k8snode.Command{
		Argv:    h.sshCommand("10", h.privileged("systemctl stop "+quoteUnits(units))),
		Timeout: runnerStopTimeout,
	})
	switch {
	case h.ctx.Err() != nil:
		return errStopped
	case r.Status == exitTimedOut:
		return fmt.Errorf("the runner units did not stop within %s", runnerStopTimeout)
	case r.Status != 0:
		return fmt.Errorf("could not stop the runner units (ssh exited with status %d)", r.Status)
	}
	return nil
}

// startRunners starts the runner units this run stopped again, when the
// host is not rebooted after all. It runs after an interrupt too.
func (h *hostReboot) startRunners() {
	if len(h.stopped) == 0 {
		return
	}
	units := h.stopped
	h.stopped = nil
	h.log("starting %s again", strings.Join(units, " "))
	r := h.runner.Run(context.WithoutCancel(h.ctx), k8snode.Command{
		Argv:    h.sshCommand("10", h.privileged("systemctl start "+quoteUnits(units))),
		Timeout: runnerStartTimeout,
	})
	if r.Status != 0 {
		h.log("warning: could not start %s again (ssh exited with status %d); start them by hand", strings.Join(units, " "), r.Status)
	}
}

// runNode runs the node reboot, or its checks, with this host's node, SSH
// target and output.
func (h *hostReboot) runNode() int {
	opts := h.f.opts.node
	opts.Host = h.target.host
	opts.Node = h.target.host
	opts.SSHTarget = h.f.inv.systemEndpoint(h.target.host)
	opts.Context = h.f.inv.KubernetesContext
	opts.Needed = h.f.opts.needed
	status := h.f.nodeReboot(h.ctx, opts, h.runner)
	h.out.flush()
	return status
}

// rebootNode reboots a Kubernetes node behind its gates. It reports false,
// without an error, when the node reboot skipped a reboot that was not
// needed after all.
func (h *hostReboot) rebootNode() (bool, error) {
	status := h.runNode()
	// A failed or skipped node reboot may have left the host running; then
	// its runner units start again. A host that went down does not need it.
	current := h.readBootID(context.WithoutCancel(h.ctx))
	if status != 0 {
		if current == h.bootID {
			h.startRunners()
		}
		return false, fmt.Errorf("the node reboot failed (status %d); the host is left as it is", status)
	}
	switch {
	case current == "":
		return false, errors.New("cannot read the boot ID after the node reboot")
	case current == h.bootID && h.f.opts.needed:
		h.startRunners()
		return false, nil
	case current == h.bootID:
		return false, errors.New("the boot ID did not change; the host did not reboot")
	}
	return true, nil
}

// rebootPlain reboots a host that is not a Kubernetes node and waits for it
// to come back with a new boot ID.
func (h *hostReboot) rebootPlain() error {
	endpoint := h.f.inv.systemEndpoint(h.target.host)
	h.log("rebooting %s", endpoint)
	r := h.runner.Run(h.ctx, k8snode.Command{
		Argv:    h.sshCommand("10", h.privileged("systemctl reboot --no-block")),
		Timeout: rebootCommandBound,
	})
	// ssh's status 255 is accepted: the connection may drop as the host
	// goes down.
	if r.Status != 0 && r.Status != 255 {
		h.startRunners()
		if h.ctx.Err() != nil {
			return errStopped
		}
		return fmt.Errorf("the reboot command failed (ssh exited with status %d); the host was not rebooted", r.Status)
	}

	h.log("waiting for %s to go down", endpoint)
	down := false
	for attempt := 0; attempt < sshDownAttempts && !down; attempt++ {
		if attempt > 0 {
			if err := h.f.sleep(h.ctx, h.f.sshPoll); err != nil {
				return h.interrupted()
			}
		}
		id, status := h.probeBootID(h.ctx, "2")
		if h.ctx.Err() != nil {
			return h.interrupted()
		}
		down = status != 0 || id != "" && id != h.bootID
	}
	if !down {
		// ssh's 255 may also mean the reboot command never arrived; the host
		// is still on its old boot, so its runners start again.
		h.startRunners()
		return errors.New("SSH did not drop and the boot ID did not change; the host was not rebooted")
	}

	h.log("waiting for %s to return", endpoint)
	limit := time.Duration(h.f.opts.node.SSHTimeoutSeconds) * time.Second
	clock := waitClock{start: time.Now()}
	sameBoot := false
	for {
		id, status := h.probeBootID(h.ctx, "5")
		if h.ctx.Err() != nil {
			return h.interrupted()
		}
		if status == 0 && id != "" && id != h.bootID {
			h.log("%s is back with a new boot ID", endpoint)
			return nil
		}
		sameBoot = status == 0 && id == h.bootID
		if limit-clock.elapsed() <= h.f.sshPoll {
			break
		}
		if err := h.pause(&clock, h.f.sshPoll); err != nil {
			return h.interrupted()
		}
	}
	if sameBoot {
		// The host answers on its old boot: the reboot never happened.
		h.startRunners()
		return fmt.Errorf("%s still runs its old boot after %ds; the host was not rebooted", endpoint, h.f.opts.node.SSHTimeoutSeconds)
	}
	return fmt.Errorf("%s did not return with a new boot ID within %ds", endpoint, h.f.opts.node.SSHTimeoutSeconds)
}

// interrupted ends a reboot wait that an interrupt stopped. A host that
// still answers on its old boot did not reboot, so its runners start again.
func (h *hostReboot) interrupted() error {
	if id, status := h.probeBootID(context.WithoutCancel(h.ctx), "5"); status == 0 && id == h.bootID {
		h.startRunners()
	}
	return errStopped
}

// verify checks a rebooted host: it runs the system it was meant to boot,
// the runner units that were active are again, and it passes its ready gate.
func (h *hostReboot) verify() error {
	host := h.target.host
	state := h.f.status.readHost(host)
	if !state.Reachable {
		return fmt.Errorf("cannot read the host after the reboot: %s", state.Error)
	}
	switch {
	case state.RebootNeeded == nil:
		h.log("warning: cannot tell whether %s booted the intended system: %s", host, state.RebootReason)
	case *state.RebootNeeded:
		return fmt.Errorf("did not boot the intended system: %s", state.RebootReason)
	}

	if len(h.target.runners) > 0 {
		h.log("waiting for %s to be active", strings.Join(h.target.runners, " "))
		clock := waitClock{start: time.Now()}
		for {
			inactive := inactiveUnits(h.target.runners, state)
			if len(inactive) == 0 {
				break
			}
			if runnerReturnTimeout-clock.elapsed() <= h.f.runnerPoll {
				return fmt.Errorf("runner units not active within %s after the reboot: %s", runnerReturnTimeout, strings.Join(inactive, " "))
			}
			if err := h.pause(&clock, h.f.runnerPoll); err != nil {
				return err
			}
			state = h.f.status.readHost(host)
		}
	}

	if serial := h.group.serial; serial != nil && serial.ReadyCommand != "" {
		return h.waitReady(host)
	}
	return nil
}

// inactiveUnits lists the units that a host's state does not show active.
func inactiveUnits(units []string, state hostStatus) []string {
	active := map[string]bool{}
	if state.Runners != nil {
		for _, unit := range state.Runners.Units {
			active[unit.Unit] = unit.State == "active"
		}
	}
	var inactive []string
	for _, unit := range units {
		if !active[unit] {
			inactive = append(inactive, unit)
		}
	}
	return inactive
}
