// Command deploy switches NixOS, nix-darwin and Home Manager configurations
// from a flake, for one host or the fleet described by an inventory
// (ADR-0006, ADR-0008).
package main

import (
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

const defaultSSHOptions = "-o ServerAliveInterval=10 -o ServerAliveCountMax=30 -o TCPKeepAlive=yes -o ConnectTimeout=30"

// settings are the environment tunables, validated before any work.
type settings struct {
	sshCheckTimeout  string
	remoteMinFreeKiB int64
	remoteGC         bool
	remoteGCTimeout  time.Duration
	readyInterval    time.Duration
	evalJobs         int
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
	force           bool
	jsonPath        string

	// rootDir holds garbage-collector roots for built paths during a run.
	rootDir string
	// failed remembers generations whose activation failed on this machine.
	failed *failedActivations
	// rebuildChecked caches whether nixos-rebuild can activate store paths.
	rebuildChecked bool
	rebuildOK      bool
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	configFile := os.Getenv("NIX_DEPLOY_CONFIG")
	jsonPath := ""
	for i, arg := range args {
		if arg == "--json" && i+1 < len(args) {
			jsonPath = args[i+1]
		} else if strings.HasPrefix(arg, "--json=") {
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
	// Read once so validation and loading use the same inventory snapshot.
	data, err := os.ReadFile(configFile)
	if err != nil {
		return failEarly(fmt.Sprintf("cannot read inventory '%s'", configFile))
	}
	inv, err := parseInventory(data)
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

	mode := "full"
	target := ""
	all := false
	for i := 0; i < len(rest); i++ {
		arg := rest[i]
		switch {
		case arg == "--nixos":
			mode = "nixos"
		case arg == "--hm":
			mode = "hm"
		case arg == "--all":
			all = true
		case arg == "--here":
			d.buildHere = true
		case arg == "--no-preflight":
			d.preflight = false
		case arg == "--fail-unreachable":
			d.failUnreachable = true
		case arg == "--dry-run":
			d.dryRun = true
		case arg == "--force":
			d.force = true
		case arg == "--json":
			if i+1 >= len(rest) {
				return failEarly("--json requires a file")
			}
			d.jsonPath = rest[i+1]
			i++
		case strings.HasPrefix(arg, "--json="):
			d.jsonPath = strings.TrimPrefix(arg, "--json=")
			if d.jsonPath == "" {
				return failEarly("--json requires a file")
			}
		case arg == "--help" || arg == "-h":
			return d.usage()
		case strings.HasPrefix(arg, "-"):
			if jsonPath != "" {
				writeFailedJSON(jsonPath)
			}
			fmt.Fprintf(os.Stderr, "deploy: unknown option '%s'\n", arg)
			return d.usage()
		default:
			target = arg
		}
	}

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
		fmt.Fprintf(os.Stderr, "deploy: interrupted by %v\n", received)
		status := 130
		if received == syscall.SIGTERM {
			status = 143
		}
		os.Exit(status)
	}()
	if all {
		return d.deployAll(mode)
	}
	if target == "" {
		target = d.self
	}
	return d.deployHost(target, mode)
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
	return values, nil
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

Options: [--config FILE] [--here] [--no-preflight] [--dry-run] [--force]
         [--json FILE]

When <host> is omitted, deploys to the current machine.
When <host> differs from the current machine, deploys remotely via SSH.
Known hosts: %s

A run has three steps. Build evaluates every selected system and Home Manager
configuration, up to DEPLOY_EVAL_JOBS (default 4) at a time, and builds them
all in one Nix invocation on this machine. Plan compares each result with what
the host runs: a system is unchanged when its running system and system profile
already point to the built path (only the profile for hosts staged with
'boot'), Home Manager when its profile and its current-home root (moved only
by a completed activation) do. A generation whose last activation from this
machine failed counts as changed. Activate switches only changed targets;
unchanged hosts are neither copied to, activated nor gated. --dry-run stops
after the plan and never starts a host's garbage collection, --force
activates unchanged targets too, and --json FILE writes the results and
per-host timings.

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
