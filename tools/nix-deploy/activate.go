package main

import (
	"bytes"
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

// systemCommand returns the command that activates host's system with
// action ("switch" or "boot"). nonInteractive makes a local sudo fail rather
// than prompt; fleet jobs rely on the credentials cached at startup.
func (d *deployer) systemCommand(host, action string, nonInteractive bool) []string {
	if host == d.self {
		sudo := []string{"sudo"}
		if nonInteractive {
			sudo = append(sudo, "-n")
		}
		if d.isDarwin() {
			return append(sudo, "/run/current-system/sw/bin/darwin-rebuild", "switch", "--flake", ".#"+host)
		}
		return append(sudo, "nixos-rebuild", action, "--flake", ".#"+host)
	}
	argv := append(rebuildCommand(), action, "--flake", ".#"+host,
		"--target-host", d.inv.systemEndpoint(host), "--use-substitutes")
	if d.inv.usesRemoteSudo(host) {
		argv = append(argv, "--ask-sudo-password")
	}
	return argv
}

// activateSystem switches host's system, or stages it for the next boot when
// the inventory says so. A switch refused by a switch inhibitor is retried
// with 'boot', so the change takes effect after the next reboot.
func (d *deployer) activateSystem(host string, streams stdio, nonInteractive bool) int {
	action := "switch"
	if host != d.self {
		action = d.inv.activationMode(host)
	}
	if action == "boot" || (host == d.self && d.isDarwin()) {
		return streams.command(d.systemCommand(host, action, nonInteractive)).run()
	}

	// The capture sees both streams, so it must accept concurrent writes.
	var output lockedBuffer
	switchRun := streams.command(d.systemCommand(host, "switch", nonInteractive))
	switchRun.stdout = io.MultiWriter(streams.out, &output)
	switchRun.stderr = io.MultiWriter(streams.err, &output)
	status := switchRun.run()
	if status == 0 || !strings.Contains(output.String(), switchInhibited) {
		return status
	}
	fmt.Fprintf(streams.out, "deploy(%s): switch inhibited, retrying with 'boot' so the change can take effect after reboot\n", host)
	return streams.command(d.systemCommand(host, "boot", nonInteractive)).run()
}

// activateHome switches host's Home Manager configuration.
func (d *deployer) activateHome(host string, streams stdio) int {
	output := ".#" + d.inv.HomeOutputPrefix + host
	if host == d.self {
		return streams.command([]string{"home-manager", "switch", "--flake", output}).run()
	}

	var built bytes.Buffer
	build := streams.command([]string{"nix", "build", "--no-link", "--print-out-paths", ".#homeConfigurations." + d.inv.HomeOutputPrefix + host + ".activationPackage"})
	build.stdout = &built
	if status := build.run(); status != 0 {
		return status
	}
	result := strings.TrimSpace(lastLine(built.String()))

	endpoint := d.inv.homeEndpoint(host)
	if status := streams.command([]string{"nix", "copy", "--substitute-on-destination", "--to", "ssh://" + endpoint, result}).run(); status != 0 {
		return status
	}
	// Home Manager creates a tiny profile-link derivation during activation.
	// Permit that one local job even on appliances whose global max-jobs=0
	// policy prevents package compilation on constrained hardware. The
	// complete activation closure has already been built and copied above.
	argv := append([]string{"ssh"}, d.sshArgs...)
	argv = append(argv, endpoint, "env NIX_CONFIG='max-jobs = 1' '"+result+"/activate'")
	return streams.command(argv).run()
}
