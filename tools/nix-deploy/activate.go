package main

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// switchInhibited is nixos-rebuild's message when a switch inhibitor refuses
// the switch, for example because the new system changes a component that
// cannot be replaced while running.
const switchInhibited = "Pre-switch check 'switchInhibitors' failed"

// stdio is where an activation's input and output go.
type stdio struct {
	in  io.Reader
	out io.Writer
	err io.Writer
}

var attached = stdio{in: os.Stdin, out: os.Stdout, err: os.Stderr}

func (s stdio) command(argv []string) command {
	return command{argv: argv, stdin: s.in, stdout: s.out, stderr: s.err}
}

// systemCommand returns the command that activates the built system path on
// host with action ("switch" or "boot"). nonInteractive makes a local sudo
// fail rather than prompt; fleet jobs rely on the credentials cached at
// startup.
func (d *deployer) systemCommand(host, action, path string, nonInteractive bool) []string {
	if host == d.self {
		sudo := []string{"sudo"}
		if nonInteractive {
			sudo = append(sudo, "-n")
		}
		if d.darwin {
			// darwin-rebuild cannot activate a store path; the flake output
			// it evaluates is the one just built.
			return append(sudo, "/run/current-system/sw/bin/darwin-rebuild", "switch", "--flake", ".#"+host)
		}
		return append(append(sudo, rebuildCommand()...), action, "--store-path", path, "--no-reexec")
	}
	// nixos-rebuild copies the closure, substituting on the target, and
	// activates it there. --no-reexec keeps it from rebuilding itself from a
	// configuration it would have to evaluate.
	argv := append(rebuildCommand(), action, "--store-path", path, "--no-reexec",
		"--target-host", d.inv.systemEndpoint(host), "--use-substitutes")
	if d.inv.usesRemoteSudo(host) {
		argv = append(argv, "--ask-sudo-password")
	}
	return argv
}

// activateSystem switches host's system, or stages it for the next boot when
// the inventory says so. A switch refused by a switch inhibitor is retried
// with 'boot', so the change takes effect after the next reboot.
func (d *deployer) activateSystem(host, path string, streams stdio, nonInteractive bool) int {
	action := d.systemAction(host)
	if action == "boot" || (host == d.self && d.darwin) {
		return streams.command(d.systemCommand(host, action, path, nonInteractive)).run()
	}

	// The capture sees both streams, so it must accept concurrent writes.
	var output lockedBuffer
	switchRun := streams.command(d.systemCommand(host, "switch", path, nonInteractive))
	switchRun.stdout = io.MultiWriter(streams.out, &output)
	switchRun.stderr = io.MultiWriter(streams.err, &output)
	status := switchRun.run()
	if status == 0 || !strings.Contains(output.String(), switchInhibited) {
		return status
	}
	fmt.Fprintf(streams.out, "deploy(%s): switch inhibited, retrying with 'boot' so the change can take effect after reboot\n", host)
	return streams.command(d.systemCommand(host, "boot", path, nonInteractive)).run()
}

// systemAction is "boot" for remote hosts the inventory stages for the next
// boot, and "switch" otherwise.
func (d *deployer) systemAction(host string) string {
	if host == d.self {
		return "switch"
	}
	return d.inv.activationMode(host)
}

// homeActivationScript activates a Home Manager generation the way the
// home-manager command does: it sets the profile itself, then runs the
// generation's activation as driver version 1, so the profile always ends on
// the activated generation. Generations without gen-version predate that
// interface and manage the profile themselves.
func homeActivationScript(path string) string {
	return `g="` + path + `"
s="${XDG_STATE_HOME:-$HOME/.local/state}"
if [ -d "$s/nix/profiles" ]; then d="$s/nix/profiles"; else d="/nix/var/nix/profiles/per-user/$(id -un)"; fi
if [ -e "$g/gen-version" ]; then
  nix-env --profile "$d/home-manager" --set "$g" || exit
  exec "$g/activate" --driver-version 1
fi
exec "$g/activate"`
}

// activateHome activates host's built Home Manager generation.
func (d *deployer) activateHome(host, path string, streams stdio) int {
	script := homeActivationScript(path)
	if host == d.self {
		return streams.command([]string{"bash", "-c", script}).run()
	}
	endpoint := d.inv.homeEndpoint(host)
	if status := streams.command([]string{"nix", "copy", "--substitute-on-destination", "--to", "ssh://" + endpoint, path}).run(); status != 0 {
		return status
	}
	// Home Manager creates a tiny profile-link derivation during activation.
	// Permit that one local job even on appliances whose global max-jobs=0
	// policy prevents package compilation on constrained hardware. The
	// complete activation closure has already been built and copied above.
	argv := append([]string{"ssh"}, d.sshArgs...)
	argv = append(argv, endpoint, posixShell(`NIX_CONFIG="max-jobs = 1"; export NIX_CONFIG; `+script))
	return streams.command(argv).run()
}

// posixShell runs a script under sh whatever the remote login shell is. The
// scripts contain no single quotes.
func posixShell(script string) string {
	return "sh -c '" + script + "'"
}

// rebuildSupportsStorePaths checks once whether nixos-rebuild accepts the
// options a store-path activation needs. Given those options and no action,
// a frontend that knows them only reports the missing action, and does
// nothing; the classic script and older nixos-rebuild-ng releases reject an
// option first. (--help is no probe: nixos-rebuild-ng shows the manual page
// there when one is installed.)
func (d *deployer) rebuildSupportsStorePaths() bool {
	if !d.rebuildChecked {
		d.rebuildChecked = true
		output, status := capture(0, append(rebuildCommand(), "--no-reexec", "--store-path", "/nonexistent")...)
		d.rebuildOK = status != 0 && status != 127 && strings.Contains(output, "No valid subcommands")
		if !d.rebuildOK {
			fmt.Fprintln(os.Stderr, "deploy: this nixos-rebuild cannot activate a built store path (needs --store-path and --no-reexec, as in nixos-rebuild-ng); nothing was switched")
		}
	}
	return d.rebuildOK
}

// needsRebuild reports whether activating the target uses nixos-rebuild.
func (d *deployer) needsRebuild(t target) bool {
	return t.phase == "system" && !(t.host == d.self && d.darwin)
}

// recordActivation records an activation result in the report and in the
// failed-activation memory.
func (d *deployer) recordActivation(r *report, t target, status int) {
	r.record(t.phase, t.host, outcome(status))
	d.failed.finish(t, status == 0)
}
