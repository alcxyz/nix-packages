package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"nix-deploy/bootcheck"
)

const statusUsage = `Usage: nixfleet status [--config FILE] [--all | <host>...] [--json FILE]

Reads, without changing anything, what each selected host booted and runs.
With no host it reads this machine; --all selects the hosts deploy --all
would (deployAllHosts on the operator host, remoteHosts elsewhere); hosts may
be named by alias. Remote hosts are read over SSH as their system user, with
BatchMode, a DEPLOY_SSH_CHECK_TIMEOUT (default 5) second connect timeout and
a 30 second limit, up to 8 at a time; this machine is read locally.

Columns:
  ssh      ok, or unreachable when the host could not be read.
  system   booted when the host runs the system it booted, switched when it
           has switched to another since, staged when its system profile
           (what the next boot loads) differs from the running system;
           darwin for other systems such as nix-darwin.
  reboot   yes when the kernel, initrd, kernel modules or kernel parameters
           differ between the booted system and the current or next-boot
           system, no when none does, unknown when one cannot be compared.
  node     the Kubernetes node of the host's name or alias, from one
           kubectl get nodes in the inventory's kubernetesContext, or
           the current context without one: cordoned or
           schedulable, and notready when it is not Ready; - when the host
           is no node or the cluster did not answer within 15 seconds.
  runners  active/total Forgejo or Gitea Actions runner service units
           (gitea-runner-*, gitea-*runner and forgejo-*runner); - for none.
  boot     with the inventory's bootCheck, the result of the host's check
           after its current boot: ok, failed, running, or pending before
           it ran; - for a host without the check.

Notes below the table explain each reboot verdict that is not "no", each
failed boot check and each unreachable host. --json FILE writes the same per host. The exit status is 0
however the hosts and the cluster answer; it is 2 for invalid arguments and 1
when the inventory, this machine's name or the JSON report fails.
`

// Bounds of the status reads. Every read only looks, so a slow host is
// reported as unreachable rather than waited for.
const (
	statusJobs        = 8
	statusReadTimeout = 30 * time.Second
	kubectlTimeout    = 15 * time.Second
)

// runnerScript lists the Forgejo and Gitea Actions runner service units
// loaded on a host, one "runner=<list-units line>" each: the NixOS
// gitea-actions-runner module's gitea-runner-<instance> units, and services
// named like forgejo-actions-runner. Helper units such as
// forgejo-runner-docker do not end in "runner" and are left out. It runs
// after bootcheck.Script, which exits early on hosts that are not Linux.
const runnerScript = `systemctl list-units --all --plain --no-legend --type=service "gitea-runner-*.service" "gitea-*runner.service" "forgejo-*runner.service" 2>/dev/null | while read -r line; do echo "runner=$line"; done`

// bootCheckScript prints the result the host's boot check (unit
// nixfleet-boot-check, from the host configuration) left for the current
// boot, each line prefixed with "bootcheck.": status=running|ok|failed,
// time=<unix seconds> and a reason= line per problem, after a present line.
// /run starts empty at every boot, so a result is always this boot's. A
// result it cannot read reads as failed. A host with the unit but no result
// yet prints status=pending; one without the unit prints nothing.
var bootCheckScript = bootCheckScriptFor("/run/nixfleet-boot-check/result", "/etc/systemd/system/nixfleet-boot-check.service")

// bootCheckScriptFor is bootCheckScript for other paths, which tests use.
// The script has no single quote, so it runs through RemoteCommand.
func bootCheckScriptFor(result, unit string) string {
	return `if [ -e ` + result + ` ]; then if [ -r ` + result + ` ]; then echo bootcheck.present=1; ` +
		`while IFS= read -r line || [ -n "$line" ]; do echo "bootcheck.$line"; done <` + result + `; ` +
		`else echo bootcheck.status=failed; echo "bootcheck.reason=the result cannot be read as $(id -un)"; fi; ` +
		`elif [ -e ` + unit + ` ]; then echo bootcheck.status=pending; fi`
}

// hostStatus is one host's state, as printed and as written by --json.
type hostStatus struct {
	Host      string `json:"host"`
	Reachable bool   `json:"reachable"`
	// Error says why the host could not be read.
	Error string `json:"error,omitempty"`
	// OS is the host's kernel name (uname -s); empty when unreachable.
	OS string `json:"os"`
	// The system store paths; empty when unknown or not NixOS.
	BootedSystem   string `json:"bootedSystem"`
	CurrentSystem  string `json:"currentSystem"`
	NextBootSystem string `json:"nextBootSystem"`
	// RebootNeeded is null when it cannot be told, including on hosts that
	// are unreachable or not NixOS.
	RebootNeeded  *bool         `json:"rebootNeeded"`
	RebootReason  string        `json:"rebootReason"`
	RebootDiffers []string      `json:"rebootDiffers"`
	Node          *nodeStatus   `json:"node"`
	Runners       *runnerStatus `json:"runners"`
	// BootCheck is null without the inventory's bootCheck or on a host
	// without the check.
	BootCheck *bootCheckStatus `json:"bootCheck"`
}

// bootCheckStatus is the result of a host's check after its current boot.
type bootCheckStatus struct {
	// Status is pending, running, ok or failed.
	Status string `json:"status"`
	// Time is when the check wrote its result, in Unix seconds; 0 when
	// pending.
	Time    int64    `json:"time"`
	Reasons []string `json:"reasons"`
}

type nodeStatus struct {
	Name     string `json:"name"`
	Cordoned bool   `json:"cordoned"`
	Ready    bool   `json:"ready"`
}

type runnerStatus struct {
	Active int          `json:"active"`
	Total  int          `json:"total"`
	Units  []runnerUnit `json:"units"`
}

type runnerUnit struct {
	Unit string `json:"unit"`
	// State is systemd's ActiveState, such as active, inactive or failed.
	State string `json:"state"`
}

type clusterStatus struct {
	Reachable bool   `json:"reachable"`
	Error     string `json:"error,omitempty"`
	Nodes     int    `json:"nodes"`
}

type statusReport struct {
	Version string        `json:"version"`
	Cluster clusterStatus `json:"cluster"`
	Hosts   []hostStatus  `json:"hosts"`
}

// statusCheck reads the state of hosts and of the cluster.
type statusCheck struct {
	inv            *inventory
	self           string
	sshArgs        []string
	connectTimeout string
	// run runs a command without input and returns its standard output,
	// standard error and exit status; tests replace it.
	run func(timeout time.Duration, argv ...string) (string, string, int)
}

func runCommand(timeout time.Duration, argv ...string) (string, string, int) {
	var stdout, stderr bytes.Buffer
	status := command{argv: argv, stdin: bytes.NewReader(nil), stdout: &stdout, stderr: &stderr, timeout: timeout}.run()
	return stdout.String(), stderr.String(), status
}

type statusOptions struct {
	configFile string
	jsonPath   string
	all        bool
	hosts      []string
}

var errStatusHelp = errors.New("help requested")

func parseStatusArgs(args []string) (statusOptions, error) {
	opts := statusOptions{configFile: os.Getenv("NIX_DEPLOY_CONFIG")}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--all":
			opts.all = true
		case arg == "--config" || arg == "--json":
			if i+1 >= len(args) || args[i+1] == "" {
				return opts, fmt.Errorf("%s requires a file", arg)
			}
			i++
			if arg == "--config" {
				opts.configFile = args[i]
			} else {
				opts.jsonPath = args[i]
			}
		case strings.HasPrefix(arg, "--config="):
			opts.configFile = strings.TrimPrefix(arg, "--config=")
			if opts.configFile == "" {
				return opts, fmt.Errorf("--config requires a file")
			}
		case strings.HasPrefix(arg, "--json="):
			opts.jsonPath = strings.TrimPrefix(arg, "--json=")
			if opts.jsonPath == "" {
				return opts, fmt.Errorf("--json requires a file")
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

// selectHosts returns the canonical hosts to read, in order and without
// repeats.
func selectHosts(inv *inventory, self string, opts statusOptions) ([]string, error) {
	if opts.all {
		return inv.fleet(self), nil
	}
	if len(opts.hosts) == 0 {
		return []string{self}, nil
	}
	var hosts []string
	for _, name := range opts.hosts {
		host := inv.canonical(name)
		if !contains(inv.KnownHosts, host) {
			return nil, fmt.Errorf("unknown host '%s'; known hosts: %s", name, joinHosts(inv.KnownHosts))
		}
		if !contains(hosts, host) {
			hosts = append(hosts, host)
		}
	}
	return hosts, nil
}

// runStatus is nixfleet status.
func runStatus(args []string) int {
	opts, err := parseStatusArgs(args)
	if errors.Is(err, errStatusHelp) {
		fmt.Print(statusUsage)
		return 0
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "nixfleet status: %v\n", err)
		fmt.Fprint(os.Stderr, statusUsage)
		return 2
	}
	if opts.configFile == "" {
		fmt.Fprintln(os.Stderr, "nixfleet status: no inventory configured; pass --config FILE or set NIX_DEPLOY_CONFIG")
		return 1
	}
	inv, err := loadInventory(opts.configFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "nixfleet status: %v\n", err)
		return 1
	}
	hostname, status := capture(0, "hostname", "-s")
	if status != 0 {
		fmt.Fprintln(os.Stderr, "nixfleet status: cannot determine the local host name")
		return 1
	}
	self := inv.canonical(strings.TrimSpace(hostname))
	hosts, err := selectHosts(inv, self, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "nixfleet status: %v\n", err)
		return 2
	}

	sshOptions := os.Getenv("NIX_SSHOPTS")
	if sshOptions == "" {
		sshOptions = defaultSSHOptions
	}
	s := &statusCheck{
		inv:            inv,
		self:           self,
		sshArgs:        strings.Fields(sshOptions),
		connectTimeout: envOr("DEPLOY_SSH_CHECK_TIMEOUT", "5"),
		run:            runCommand,
	}
	report := s.collect(hosts)
	printStatus(os.Stdout, report)
	if opts.jsonPath != "" {
		if err := writeStatusJSON(opts.jsonPath, report); err != nil {
			fmt.Fprintf(os.Stderr, "nixfleet status: cannot write %s: %v\n", opts.jsonPath, err)
			return 1
		}
	}
	return 0
}

// collect reads the hosts, at most statusJobs at a time, while one kubectl
// call reads the cluster's nodes.
func (s *statusCheck) collect(hosts []string) statusReport {
	report := statusReport{Version: version, Hosts: make([]hostStatus, len(hosts))}
	var nodes map[string]nodeStatus
	var wait sync.WaitGroup
	wait.Add(1)
	go func() {
		defer wait.Done()
		nodes, report.Cluster = s.readNodes()
	}()
	slots := make(chan struct{}, statusJobs)
	for i, host := range hosts {
		wait.Add(1)
		go func() {
			defer wait.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			report.Hosts[i] = s.readHost(host)
		}()
	}
	wait.Wait()
	for i := range report.Hosts {
		if node, ok := nodes[report.Hosts[i].Host]; ok {
			report.Hosts[i].Node = &node
		}
	}
	return report
}

// readHost reads one host's systems and runner units, over SSH or, for this
// machine, locally.
func (s *statusCheck) readHost(host string) hostStatus {
	h := hostStatus{Host: host, RebootDiffers: []string{}}
	script := bootcheck.Script + "; " + runnerScript
	if s.inv.BootCheck {
		script += "; " + bootCheckScript
	}
	argv := []string{"sh", "-c", script}
	if host != s.self {
		argv = []string{"ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=" + s.connectTimeout, "-o", "ConnectionAttempts=1"}
		argv = append(argv, s.sshArgs...)
		argv = append(argv, s.inv.systemEndpoint(host), bootcheck.RemoteCommand(script))
	}
	stdout, stderr, status := s.run(statusReadTimeout, argv...)
	switch {
	case status == exitTimedOut:
		h.Error = fmt.Sprintf("no answer within %s", statusReadTimeout)
		return h
	case status != 0:
		h.Error = oneLine(stderr)
		if h.Error == "" {
			h.Error = fmt.Sprintf("%s exited with status %d", argv[0], status)
		}
		return h
	}

	h.Reachable = true
	state := bootcheck.Parse(stdout)
	h.OS = state.OS
	h.BootedSystem = state.Booted
	h.CurrentSystem = state.Current
	h.NextBootSystem = state.Profile
	h.RebootReason = state.Reason()
	if state.OS == "Linux" {
		h.RebootDiffers = append(h.RebootDiffers, state.Differs...)
		switch state.Reboot {
		case bootcheck.Needed:
			needed := true
			h.RebootNeeded = &needed
		case bootcheck.NotNeeded:
			needed := false
			h.RebootNeeded = &needed
		}
	}
	h.Runners = parseRunners(stdout)
	h.BootCheck = parseBootCheck(stdout)
	return h
}

// parseBootCheck reads bootCheckScript's lines. A missing status, or one
// other than the four it knows, reads as failed, so an empty or damaged
// result never passes.
func parseBootCheck(output string) *bootCheckStatus {
	var check *bootCheckStatus
	for _, line := range strings.Split(output, "\n") {
		rest, ok := strings.CutPrefix(line, "bootcheck.")
		if !ok {
			continue
		}
		if check == nil {
			check = &bootCheckStatus{Reasons: []string{}}
		}
		key, value, _ := strings.Cut(rest, "=")
		switch key {
		case "status":
			check.Status = value
		case "time":
			check.Time, _ = strconv.ParseInt(value, 10, 64)
		case "reason":
			if value != "" {
				check.Reasons = append(check.Reasons, value)
			}
		}
	}
	if check == nil {
		return nil
	}
	switch check.Status {
	case "pending", "running", "ok", "failed":
	case "":
		check.Reasons = append(check.Reasons, "the result has no status")
		check.Status = "failed"
	default:
		check.Reasons = append(check.Reasons, fmt.Sprintf("the result has status %q", check.Status))
		check.Status = "failed"
	}
	return check
}

// parseRunners reads runnerScript's lines: systemctl list-units columns
// UNIT LOAD ACTIVE SUB DESCRIPTION, possibly after a status mark.
func parseRunners(output string) *runnerStatus {
	var runners *runnerStatus
	for _, line := range strings.Split(output, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "runner=")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) > 0 && !strings.HasSuffix(fields[0], ".service") {
			fields = fields[1:]
		}
		if len(fields) < 3 {
			continue
		}
		if runners == nil {
			runners = &runnerStatus{}
		}
		runners.Total++
		if fields[2] == "active" {
			runners.Active++
		}
		runners.Units = append(runners.Units, runnerUnit{Unit: fields[0], State: fields[2]})
	}
	return runners
}

// kubectl is the command line of a kubectl read in the inventory's context.
func (s *statusCheck) kubectl(args ...string) []string {
	argv := []string{"kubectl"}
	if s.inv.KubernetesContext != "" {
		argv = append(argv, "--context="+s.inv.KubernetesContext)
	}
	return append(argv, args...)
}

// readNodes reads every node's cordon and readiness, by canonical host name.
// A cluster that cannot be read leaves every node unknown.
func (s *statusCheck) readNodes() (map[string]nodeStatus, clusterStatus) {
	stdout, stderr, status := s.run(kubectlTimeout, s.kubectl("get", "nodes", "-o", "json", "--request-timeout=10s")...)
	switch {
	case status == 127:
		return nil, clusterStatus{Error: "kubectl is not available"}
	case status == exitTimedOut:
		return nil, clusterStatus{Error: fmt.Sprintf("kubectl did not answer within %s", kubectlTimeout)}
	case status != 0:
		message := oneLine(lastLine(stderr))
		if message == "" {
			message = fmt.Sprintf("kubectl exited with status %d", status)
		}
		return nil, clusterStatus{Error: message}
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Unschedulable bool `json:"unschedulable"`
			} `json:"spec"`
			Status struct {
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(stdout), &list); err != nil {
		return nil, clusterStatus{Error: "kubectl output does not decode: " + err.Error()}
	}
	nodes := map[string]nodeStatus{}
	for _, item := range list.Items {
		node := nodeStatus{Name: item.Metadata.Name, Cordoned: item.Spec.Unschedulable}
		for _, condition := range item.Status.Conditions {
			if condition.Type == "Ready" {
				node.Ready = condition.Status == "True"
			}
		}
		nodes[s.inv.canonical(node.Name)] = node
	}
	return nodes, clusterStatus{Reachable: true, Nodes: len(list.Items)}
}

func printStatus(w io.Writer, report statusReport) {
	format := "%-12s %-12s %-16s %-8s %-21s %-8s %s\n"
	fmt.Fprintf(w, format, "host", "ssh", "system", "reboot", "node", "runners", "boot")
	var notes []string
	for _, h := range report.Hosts {
		ssh, system, reboot, node, runners, boot := "unreachable", "-", "-", "-", "-", "-"
		if h.Reachable {
			ssh = "ok"
			system = systemState(h)
		}
		switch {
		case h.RebootNeeded != nil && *h.RebootNeeded:
			reboot = "yes"
			notes = append(notes, h.Host+": reboot needed: "+h.RebootReason)
		case h.RebootNeeded != nil:
			reboot = "no"
		case !h.Reachable || h.OS != "" && h.OS != "Linux":
			// Nothing to tell: unreachable, or not NixOS.
		default:
			reboot = "unknown"
			notes = append(notes, h.Host+": reboot unknown: "+h.RebootReason)
		}
		if !h.Reachable {
			notes = append(notes, h.Host+": unreachable: "+h.Error)
		}
		if h.Node != nil {
			node = "schedulable"
			if h.Node.Cordoned {
				node = "cordoned"
			}
			if !h.Node.Ready {
				node += ",notready"
			}
		}
		if h.Runners != nil {
			runners = fmt.Sprintf("%d/%d", h.Runners.Active, h.Runners.Total)
		}
		if h.BootCheck != nil {
			boot = h.BootCheck.Status
			if boot == "failed" {
				notes = append(notes, h.Host+": boot check failed: "+strings.Join(h.BootCheck.Reasons, "; "))
			}
		}
		fmt.Fprintf(w, format, h.Host, ssh, system, reboot, node, runners, boot)
	}
	for _, note := range notes {
		fmt.Fprintln(w, note)
	}
	if report.Cluster.Reachable {
		fmt.Fprintf(w, "cluster: %d nodes\n", report.Cluster.Nodes)
	} else {
		fmt.Fprintf(w, "cluster: not read: %s\n", report.Cluster.Error)
	}
}

// systemState names how a reachable host's running system relates to the
// one it booted and the one its next boot loads.
func systemState(h hostStatus) string {
	if h.OS != "" && h.OS != "Linux" {
		return strings.ToLower(h.OS)
	}
	if h.BootedSystem == "" || h.CurrentSystem == "" {
		return "unknown"
	}
	var states []string
	if h.BootedSystem == h.CurrentSystem {
		states = append(states, "booted")
	} else {
		states = append(states, "switched")
	}
	if h.NextBootSystem != "" && h.NextBootSystem != h.CurrentSystem {
		states = append(states, "staged")
	}
	return strings.Join(states, ",")
}

func writeStatusJSON(path string, report statusReport) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}
