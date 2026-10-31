// Command nixfleet deploys and inspects the fleet described by an inventory:
// its deploy subcommand switches NixOS, nix-darwin and Home Manager
// configurations from a flake, for one host or the fleet (ADR-0006,
// ADR-0008); reboot reboots hosts of the fleet by policy and constraint
// group (ADR-0010); off and on power-cycle a Kubernetes node behind the
// cluster gates; status reads what each host runs; triage writes the context
// of a recorded run and launches the operator's agent on it (ADR-0016). Installed as deploy or
// k8s-node-power, it is that command.
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// version is set by the package build (-X main.version); see CHANGELOG.md.
var version = "dev"

const defaultSSHOptions = "-o ServerAliveInterval=10 -o ServerAliveCountMax=30 -o TCPKeepAlive=yes -o ConnectTimeout=30"

// settings are the environment tunables, validated before any work.
type settings struct {
	sshCheckTimeout  string
	remoteMinFreeKiB int64
	remoteGC         bool
	remoteGCTimeout  time.Duration
	readyInterval    time.Duration
	evalJobs         int
	// buildProfile is auto, light or heavy; profileSource says who chose an
	// explicit one.
	buildProfile  string
	profileSource string
	heavyBuilds   int
	heavyMaxJobs  int
	heavyCores    int
	// runKeep is how many run records a run leaves behind.
	runKeep int
}

type deployer struct {
	inv        *inventory
	configFile string
	settings   settings
	ui         *ui
	self       string
	darwin     bool
	sshArgs    []string

	buildHere       bool
	preflight       bool
	failUnreachable bool
	dryRun          bool
	buildOnly       bool
	force           bool
	jsonPath        string

	// rootDir holds garbage-collector roots for built paths during a run.
	rootDir string
	// failed remembers generations whose activation failed on this machine.
	failed *failedActivations
	// rebuildChecked caches whether nixos-rebuild can activate store paths.
	rebuildChecked bool
	rebuildOK      bool
	// hostLimits caches this machine's Nix max-jobs and cores, which cap the
	// heavy build profile.
	hostLimits *hostLimits
	// record keeps this run's evidence on disk (ADR-0016).
	record *runRecord
}

const nixfleetUsage = `Usage: nixfleet deploy [OPTIONS] [<host> | --all]
       nixfleet reboot [OPTIONS] [--all | <host>...]
       nixfleet off|on [OPTIONS] <host>
       nixfleet status [--config FILE] [--all | <host>...] [--json FILE]
       nixfleet triage [--config FILE] [--no-launch] [--warnings] [<run>]

Subcommands:
  deploy   Evaluate, plan, build and activate the NixOS, nix-darwin and Home
           Manager configurations of one host or the fleet. The same as the
           deploy command; see nixfleet deploy --help.
  reboot   Reboot hosts of the fleet, each under the policies the inventory
           gives it: a Kubernetes node behind the cluster, storage, database
           and self-fence gates, serial group members behind their ready
           command, runner hosts after their jobs. Constraint groups run
           concurrently. With --needed, only hosts that need it; see
           nixfleet reboot --help. k8s-node-power reboot reboots one node.
  off      Power off a Kubernetes node behind the same gates, leaving it
           cordoned.
  on       Wait for a node to return, uncordon it and let the cluster settle.
           off and on are the k8s-node-power command; see nixfleet off
           --help.
  status   Read, without changing anything, what each host booted and runs,
           whether it needs a reboot, its Kubernetes cordon state and its CI
           runner units; see nixfleet status --help.
  triage   Write context.md for a recorded deploy run, by default the newest
           that failed: its facts, failed targets, their logs and first
           errors, or with --warnings the evaluation warnings of the newest
           run, then launch the configured agent CLI on it; see
           nixfleet triage --help.

Options:
  -h, --help   Show this help.
  --version    Print the version.

Installed as deploy or k8s-node-power, this program is that command.
`

func main() {
	os.Exit(dispatch(filepath.Base(os.Args[0]), os.Args[1:]))
}

// dispatch runs the command the program was invoked as: deploy and
// k8s-node-power keep their own command lines, and any other name is
// nixfleet, whose first argument names the subcommand.
func dispatch(name string, args []string) int {
	switch name {
	case "deploy":
		return runDeploy(args)
	case "k8s-node-power":
		return runPower(name, args)
	}
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, nixfleetUsage)
		return 2
	}
	switch args[0] {
	case "-h", "--help":
		fmt.Print(nixfleetUsage)
		return 0
	case "--version":
		fmt.Println("nixfleet " + version)
		return 0
	case "deploy":
		return runDeploy(args[1:])
	case "reboot":
		return runReboot(args[1:])
	case "off", "on":
		return runPower("nixfleet", args)
	case "status":
		return runStatus(args[1:])
	case "triage":
		return runTriage(args[1:])
	}
	fmt.Fprintf(os.Stderr, "nixfleet: unknown subcommand '%s'\n", args[0])
	fmt.Fprint(os.Stderr, nixfleetUsage)
	return 2
}

// runDeploy is the deploy command.
func runDeploy(args []string) int {
	configFile := os.Getenv("NIX_DEPLOY_CONFIG")
	jsonPath := ""
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		// Answered without an inventory, so any installed copy can say what
		// it is.
		case arg == "--version":
			fmt.Println("deploy " + version)
			return 0
		case arg == "--json" && i+1 < len(args):
			jsonPath = args[i+1]
			i++
		case arg == "--config":
			// Its value is a file name, not an option.
			i++
		case strings.HasPrefix(arg, "--json="):
			jsonPath = strings.TrimPrefix(arg, "--json=")
		}
	}
	// A run that cannot start still replaces a previous report.
	failEarly := func(message string) int {
		if jsonPath != "" {
			writeFailedJSON(jsonPath)
		}
		return fail(message)
	}
	var rest []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--json" && i+1 < len(args):
			rest = append(rest, arg, args[i+1])
			i++
		case arg == "--config":
			if i+1 >= len(args) {
				return failEarly("--config requires a file")
			}
			configFile = args[i+1]
			i++
		case strings.HasPrefix(arg, "--config="):
			configFile = strings.TrimPrefix(arg, "--config=")
			if configFile == "" {
				return failEarly("--config requires a file")
			}
		default:
			rest = append(rest, arg)
		}
	}

	if configFile == "" {
		return failEarly("no inventory configured; pass --config FILE or set NIX_DEPLOY_CONFIG")
	}
	inv, err := loadInventory(configFile)
	if err != nil {
		return failEarly(err.Error())
	}

	sshOptions := os.Getenv("NIX_SSHOPTS")
	if sshOptions == "" {
		sshOptions = defaultSSHOptions
	}
	// Child commands, including nixos-rebuild, read these.
	os.Setenv("NIX_SSHOPTS", sshOptions)
	os.Setenv("NIX_DEPLOY_CONFIG", configFile)

	values, err := loadSettings()
	if err != nil {
		return failEarly(err.Error())
	}

	hostname, status := capture(0, "hostname", "-s")
	if status != 0 {
		return failEarly("cannot determine the local host name")
	}

	system, _ := capture(0, "uname")
	d := &deployer{
		inv:        inv,
		configFile: configFile,
		settings:   values,
		ui:         newUI(inv.HostColors),
		self:       inv.canonical(strings.TrimSpace(hostname)),
		darwin:     strings.TrimSpace(system) == "Darwin",
		sshArgs:    strings.Fields(sshOptions),
		preflight:  true,
		failed:     loadFailedActivations(),
	}

	opts, err := d.parseOptions(rest)
	switch {
	case err == errDeployHelp:
		return d.usage()
	case errors.Is(err, errUnknownOption):
		if jsonPath != "" {
			writeFailedJSON(jsonPath)
		}
		fmt.Fprintln(os.Stderr, "deploy: "+err.Error())
		return d.usage()
	case err != nil:
		return failEarly(err.Error())
	}

	// The flake is the one Nix finds for "." from the working directory; an
	// unknown working directory is left empty rather than stopping the run.
	cwd, _ := os.Getwd()
	d.record = startRunRecord(args, findFlakeDir(cwd), d.dryRun, d.buildOnly, d.force, d.settings.runKeep)
	defer d.releaseRoots()
	// Ctrl-C reaches the running commands too; release the roots before
	// exiting so an interrupted run does not pin its closures.
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt, syscall.SIGTERM)
	go func() {
		received := <-interrupts
		d.releaseRoots()
		// The run's results are incomplete; do not leave an older report.
		if d.jsonPath != "" {
			writeFailedJSON(d.jsonPath)
		}
		removeReportTemps()
		// The run record stays as it is: without an end in run.json, it shows
		// the run did not finish.
		fmt.Fprintf(os.Stderr, "deploy: interrupted by %v\n", received)
		status := 130
		if received == syscall.SIGTERM {
			status = 143
		}
		os.Exit(status)
	}()
	var exit int
	if opts.all {
		exit = d.deployAll(opts.mode)
	} else {
		if opts.target == "" {
			opts.target = d.self
		}
		exit = d.deployHost(opts.target, opts.mode)
	}
	// The run and its record are finished: an interrupt no longer rewrites
	// the report, the agent triage may launch takes the terminal's
	// interrupts itself, and the roots need not outlive the run.
	signal.Stop(interrupts)
	d.releaseRoots()
	return d.offerTriage(exit, isTerminal(os.Stdin) && isTerminal(os.Stdout), os.Stdin, os.Stdout, runTriage)
}

// deployOptions are what a deploy command line selects besides the
// deployer's own switches.
type deployOptions struct {
	// mode is "full", "nixos" or "hm".
	mode   string
	target string
	all    bool
}

var (
	errDeployHelp    = errors.New("help requested")
	errUnknownOption = errors.New("unknown option")
)

// parseOptions reads the deploy options, with --config already taken out,
// and sets the deployer's switches.
func (d *deployer) parseOptions(rest []string) (deployOptions, error) {
	opts := deployOptions{mode: "full"}
	for i := 0; i < len(rest); i++ {
		arg := rest[i]
		switch {
		case arg == "--nixos":
			opts.mode = "nixos"
		case arg == "--hm":
			opts.mode = "hm"
		case arg == "--all":
			opts.all = true
		case arg == "--here":
			d.buildHere = true
		case arg == "--no-preflight":
			d.preflight = false
		case arg == "--fail-unreachable":
			d.failUnreachable = true
		case arg == "--dry-run":
			d.dryRun = true
		case arg == "--build-only":
			d.buildOnly = true
		case arg == "--force":
			d.force = true
		case arg == "--heavy" || arg == "--light":
			d.settings.buildProfile = strings.TrimPrefix(arg, "--")
			d.settings.profileSource = arg
		case arg == "--json":
			if i+1 >= len(rest) {
				return opts, errors.New("--json requires a file")
			}
			d.jsonPath = rest[i+1]
			i++
		case strings.HasPrefix(arg, "--json="):
			d.jsonPath = strings.TrimPrefix(arg, "--json=")
			if d.jsonPath == "" {
				return opts, errors.New("--json requires a file")
			}
		case arg == "--help" || arg == "-h":
			return opts, errDeployHelp
		case strings.HasPrefix(arg, "-"):
			return opts, fmt.Errorf("%w '%s'", errUnknownOption, arg)
		default:
			opts.target = arg
		}
	}
	// A dry run stops before the build that --build-only is there to run.
	if d.dryRun && d.buildOnly {
		return opts, errors.New("--build-only and --dry-run cannot be combined")
	}
	// A build-only run skips preflight, so no host can fail it.
	if d.failUnreachable && d.buildOnly {
		return opts, errors.New("--build-only and --fail-unreachable cannot be combined")
	}
	return opts, nil
}

// loadInventory reads the inventory once, so validation and loading use the
// same snapshot.
func loadInventory(path string) (*inventory, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read inventory '%s'", path)
	}
	return parseInventory(data)
}

func fail(message string) int {
	fmt.Fprintln(os.Stderr, "deploy: "+message)
	return 1
}

var (
	digits         = regexp.MustCompile(`^[0-9]+$`)
	positiveNumber = regexp.MustCompile(`^[1-9][0-9]*$`)
)

func loadSettings() (settings, error) {
	values := settings{sshCheckTimeout: envOr("DEPLOY_SSH_CHECK_TIMEOUT", "5")}

	minFree := envOr("DEPLOY_REMOTE_MIN_FREE_KIB", "4194304")
	if !digits.MatchString(minFree) {
		return values, fmt.Errorf("DEPLOY_REMOTE_MIN_FREE_KIB must be an integer number of KiB")
	}
	values.remoteMinFreeKiB, _ = strconv.ParseInt(minFree, 10, 64)

	switch envOr("DEPLOY_REMOTE_GC", "1") {
	case "0":
	case "1":
		values.remoteGC = true
	default:
		return values, fmt.Errorf("DEPLOY_REMOTE_GC must be 0 or 1")
	}

	gcTimeout := envOr("DEPLOY_REMOTE_GC_TIMEOUT", "600")
	if !positiveNumber.MatchString(gcTimeout) {
		return values, fmt.Errorf("DEPLOY_REMOTE_GC_TIMEOUT must be a positive integer number of seconds")
	}
	values.remoteGCTimeout = seconds(gcTimeout)

	interval := envOr("DEPLOY_SERIAL_READY_INTERVAL", "10")
	if !positiveNumber.MatchString(interval) {
		return values, fmt.Errorf("DEPLOY_SERIAL_READY_INTERVAL must be a positive integer number of seconds")
	}
	values.readyInterval = seconds(interval)

	evalJobs := envOr("DEPLOY_EVAL_JOBS", "4")
	if !positiveNumber.MatchString(evalJobs) {
		return values, fmt.Errorf("DEPLOY_EVAL_JOBS must be a positive integer")
	}
	values.evalJobs, _ = strconv.Atoi(evalJobs)

	values.buildProfile = envOr("DEPLOY_BUILD_PROFILE", profileAuto)
	values.profileSource = "DEPLOY_BUILD_PROFILE"
	switch values.buildProfile {
	case profileAuto, profileLight, profileHeavy:
	default:
		return values, fmt.Errorf("DEPLOY_BUILD_PROFILE must be auto, light or heavy")
	}
	var err error
	if values.heavyBuilds, err = positiveSetting("DEPLOY_HEAVY_BUILDS", "50"); err != nil {
		return values, err
	}
	if values.heavyMaxJobs, err = positiveSetting("DEPLOY_HEAVY_MAX_JOBS", "6"); err != nil {
		return values, err
	}
	if values.heavyCores, err = positiveSetting("DEPLOY_HEAVY_CORES", "8"); err != nil {
		return values, err
	}
	if values.runKeep, err = positiveSetting("DEPLOY_RUN_KEEP", "30"); err != nil {
		return values, err
	}
	return values, nil
}

func positiveSetting(name, fallback string) (int, error) {
	value := envOr(name, fallback)
	if !positiveNumber.MatchString(value) {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	n, _ := strconv.Atoi(value)
	return n, nil
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func seconds(value string) time.Duration {
	n, _ := strconv.Atoi(value)
	return time.Duration(n) * time.Second
}

// rebuildCommand falls back to running nixos-rebuild from nixpkgs on hosts,
// such as Darwin operators, that do not have it installed. The installed
// one is named by its absolute path, so a sudo with its own search path runs
// the same frontend that was checked.
func rebuildCommand() []string {
	if path, err := exec.LookPath("nixos-rebuild"); err == nil {
		if absolute, err := filepath.Abs(path); err == nil {
			return []string{absolute}
		}
		return []string{path}
	}
	return []string{"nix", "run", "nixpkgs#nixos-rebuild", "--"}
}

func (d *deployer) usage() int {
	fmt.Printf(`Usage: deploy [OPTIONS] [<host>]
       deploy [OPTIONS] [--fail-unreachable] --all
       deploy [OPTIONS] --nixos [<host>]
       deploy [OPTIONS] --hm [<host>]

Options: [--config FILE] [--here] [--no-preflight] [--dry-run | --build-only]
         [--force] [--heavy | --light] [--json FILE] [--version]

When <host> is omitted, deploys to the current machine.
When <host> differs from the current machine, deploys remotely via SSH.
Known hosts: %s

A run has four steps. Evaluate finds the store path of every selected system
and Home Manager configuration, up to DEPLOY_EVAL_JOBS (default 4) at a time,
without building it. Plan compares each path with what the host runs: a system
is unchanged when its running system and system profile already point to it
(only the profile for hosts staged with 'boot'), Home Manager when its profile
and its current-home root (moved only by a completed activation) do. A
generation whose last activation from this machine failed counts as changed.
Build builds only the changed targets, in one Nix invocation on this machine.
Activate switches them; unchanged hosts are neither built, copied to,
activated nor gated. --dry-run stops after the plan, before building, and
never starts a host's garbage collection, --force builds and activates
unchanged targets too, and --json FILE writes the results and per-host
timings.

--build-only evaluates, plans and builds both phases like a real run and
stops before activation: it skips preflight, the sudo prompt, the remote
garbage collection, switches, serial group checks and ready gates. A target
that built shows as built, and an unchanged one as unchanged; with --force,
unchanged targets are built too. It exits non-zero when any selected target
fails to evaluate or build. It cannot be combined with --dry-run or
--fail-unreachable.

Every run leaves a record in $XDG_STATE_HOME/nix-deploy/runs (by default
~/.local/state): run.json, the flake.lock it deployed, the --json report
as report.json, under logs/ the output of each evaluation, fleet job and
system switch that deploy already reads (commands on the terminal keep it and
are not logged), and for a failed build the build logs Nix kept. The newest DEPLOY_RUN_KEEP
(default 30) records are kept, and those of runs still going on. A failed run
prints the directory of its record.

When a run fails with standard input and output on a terminal (with or
without --json), an agentCommand in the triage object of the inventory and
NIXFLEET_TRIAGE not set, deploy asks whether to start nixfleet triage on that
run; y or yes starts it, anything else declines. deploy then exits with its
own failure status, not the agent's. Triage sets NIXFLEET_TRIAGE=1 for the
agent, so a deploy the agent runs does not ask again.

Before the build, the run counts the derivations it would build (nix build
--dry-run; downloads do not count) and picks a build profile. Light keeps this
machine's Nix settings. Heavy, from DEPLOY_HEAVY_BUILDS (default 50)
derivations, passes max-jobs DEPLOY_HEAVY_MAX_JOBS (default 6) and cores
DEPLOY_HEAVY_CORES (default 8), each capped at this machine's own setting (one
that cannot be read is left to the host), so
many large builds, typically after a flake update, cannot overlap until the
machine runs out of memory. A count that fails picks heavy. --heavy or
--light, or DEPLOY_BUILD_PROFILE=heavy|light, skips the count; --dry-run
prints the profile a run would use.

Remote phases are checked over SSH before building. A single failed preflight
phase fails early. For --all, hosts that fail preflight are skipped for that
phase unless --fail-unreachable is set. Set DEPLOY_SSH_CHECK_TIMEOUT to tune the
SSH preflight timeout.

Remote NixOS phases also require free space on the target filesystem backing
/nix. Set DEPLOY_REMOTE_MIN_FREE_KIB to tune the threshold, or 0 to disable it.
When a target logged in as root is below the threshold and has nix-gc.timer,
preflight starts its nix-gc.service once, waits up to DEPLOY_REMOTE_GC_TIMEOUT
seconds (default 600) per host, and checks again. Set DEPLOY_REMOTE_GC=0 to
skip that.

A local system switch asks for the sudo password before preflight.

By default, --all runs from %s for the canonical fleet workflow.
Use --here to run --all from the current machine and build locally before
copying closures to the target hosts. On Darwin this requires a working Linux
builder for Linux targets.

A system switch that a switch inhibitor refuses is retried with 'boot', so the
change takes effect after the next reboot.

Hosts in a serial group (serialGroups, or the serialSystemHosts shorthand;
for example members of a quorum cluster) are not switched in the parallel
system batch. Each group switches its changed members one at a time, in
inventory order, after it; groups run one after another. When the group's
readyCommand is set, it runs on this machine after each member's system
switch, with {host} replaced by the host name, and is retried every
DEPLOY_SERIAL_READY_INTERVAL seconds (default 10) until it succeeds within the
group's readyTimeoutSeconds (default 600); each attempt is killed at that
deadline. A failed switch or ready gate stops that group's rollout and reports
its remaining members as skipped.

Before a group's first switch, every selected member must pass preflight, and
every member, selected or not, must pass one ready-command attempt (bounded by
min(60, readyTimeoutSeconds)). Otherwise no member of that group is switched in
that run (switching the others could break quorum) and the run fails; other
hosts and groups proceed. Before each later switch, the other members are
checked again the same way. The ready command must be non-interactive (for
example, use ssh -o BatchMode=yes); it runs in the foreground so Ctrl-C
reaches it. A single-host system deploy of a group member first checks the
other members the same way, and runs the ready gate after its switch.
`, joinHosts(d.inv.KnownHosts), d.inv.OperatorHost)
	return 1
}
