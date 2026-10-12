// Command deploy switches NixOS, nix-darwin and Home Manager configurations
// from a flake, for one host or the fleet described by an inventory
// (ADR-0006, ADR-0008).
package main

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
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
}

type deployer struct {
	inv        *inventory
	configFile string
	settings   settings
	ui         *ui
	self       string
	sshArgs    []string

	buildHere       bool
	preflight       bool
	failUnreachable bool
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	configFile := os.Getenv("NIX_DEPLOY_CONFIG")
	var rest []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--config":
			if i+1 >= len(args) {
				return fail("--config requires a file")
			}
			configFile = args[i+1]
			i++
		case strings.HasPrefix(arg, "--config="):
			configFile = strings.TrimPrefix(arg, "--config=")
			if configFile == "" {
				return fail("--config requires a file")
			}
		default:
			rest = append(rest, arg)
		}
	}

	if configFile == "" {
		return fail("no inventory configured; pass --config FILE or set NIX_DEPLOY_CONFIG")
	}
	// Read once so validation and loading use the same inventory snapshot.
	data, err := os.ReadFile(configFile)
	if err != nil {
		return fail(fmt.Sprintf("cannot read inventory '%s'", configFile))
	}
	inv, err := parseInventory(data)
	if err != nil {
		return fail(err.Error())
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
		return fail(err.Error())
	}

	hostname, status := capture(0, "hostname", "-s")
	if status != 0 {
		return fail("cannot determine the local host name")
	}

	d := &deployer{
		inv:        inv,
		configFile: configFile,
		settings:   values,
		ui:         newUI(inv.HostColors),
		self:       inv.canonical(strings.TrimSpace(hostname)),
		sshArgs:    strings.Fields(sshOptions),
		preflight:  true,
	}

	mode := "full"
	target := ""
	all := false
	for _, arg := range rest {
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
		case arg == "--help" || arg == "-h":
			return d.usage()
		case strings.HasPrefix(arg, "-"):
			fmt.Fprintf(os.Stderr, "deploy: unknown option '%s'\n", arg)
			return d.usage()
		default:
			target = arg
		}
	}

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

func (d *deployer) isDarwin() bool {
	output, status := capture(0, "uname")
	return status == 0 && strings.TrimSpace(output) == "Darwin"
}

// rebuildCommand falls back to running nixos-rebuild from nixpkgs on hosts,
// such as Darwin operators, that do not have it installed.
func rebuildCommand() []string {
	if _, err := exec.LookPath("nixos-rebuild"); err == nil {
		return []string{"nixos-rebuild"}
	}
	return []string{"nix", "run", "nixpkgs#nixos-rebuild", "--"}
}

func (d *deployer) usage() int {
	fmt.Printf(`Usage: deploy [--config FILE] [--here] [--no-preflight] [<host>]
       deploy [--config FILE] [--here] [--no-preflight] [--fail-unreachable] --all
       deploy [--config FILE] [--here] [--no-preflight] --nixos [<host>]
       deploy [--config FILE] [--no-preflight] --hm [<host>]

When <host> is omitted, deploys to the current machine.
When <host> differs from the current machine, deploys remotely via SSH.
Known hosts: %s

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

Inventory hosts listed in serialSystemHosts (for example members of a quorum
cluster) are not rebuilt in the parallel system batch. They switch one at a
time, in inventory order, after it. When serialReadyCommand is set, it runs on
this machine after each serial host's system switch, with {host} replaced by
the host name, and is retried every DEPLOY_SERIAL_READY_INTERVAL seconds
(default 10) until it succeeds within serialReadyTimeoutSeconds (default 600);
each attempt is killed at that deadline. A failed switch or ready gate stops
the serial rollout and reports the remaining serial hosts as skipped.

Before the first serial switch, every selected serial host must pass preflight,
and every serial host, selected or not, must pass one ready-command attempt
(bounded by min(60, serialReadyTimeoutSeconds)). Otherwise no serial host is
switched in that run (switching the others could break quorum) and the run
fails; other hosts proceed. Before each later serial switch, the other serial
hosts are checked again the same way. serialReadyCommand must be
non-interactive (for example, use ssh -o BatchMode=yes); it runs in the
foreground so Ctrl-C reaches it. A single-host system
deploy of a serial host first checks the other serial hosts the same way, and
runs the ready gate after its switch.
`, joinHosts(d.inv.KnownHosts), d.inv.OperatorHost)
	return 1
}
