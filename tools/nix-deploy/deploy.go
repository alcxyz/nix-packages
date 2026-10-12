package main

import (
	"fmt"
	"os"
)

// deployHost deploys one host. mode is "full" (system, then Home Manager),
// "nixos" or "hm". A failed step ends the run with that step's status.
func (d *deployer) deployHost(target, mode string) int {
	host := d.inv.canonical(target)
	if !contains(d.inv.KnownHosts, host) {
		fmt.Fprintf(os.Stderr, "deploy: unknown host '%s'\n", host)
		fmt.Fprintf(os.Stderr, "Known hosts: %s\n", joinHosts(d.inv.KnownHosts))
		return 1
	}
	if mode == "hm" && !d.inv.usesHomeManager(host) {
		fmt.Fprintf(os.Stderr, "deploy: %s is configured as a NixOS-only host\n", host)
		return 1
	}
	if mode == "full" && !d.inv.usesHomeManager(host) {
		mode = "nixos"
	}
	system := mode == "nixos" || mode == "full"
	home := mode == "hm" || mode == "full"
	local := host == d.self
	homeChecked := false

	if d.preflight && !local {
		if system {
			d.ui.section("preflight")
			if !d.preflightHost(host, "nixos") {
				fmt.Fprintf(os.Stderr, "deploy: %s failed system preflight; use --no-preflight to attempt anyway\n", host)
				return 1
			}
		} else {
			d.ui.section("preflight")
			if !d.preflightHost(host, "hm") {
				fmt.Fprintf(os.Stderr, "deploy: %s failed Home Manager preflight; use --no-preflight to attempt anyway\n", host)
				return 1
			}
			homeChecked = true
		}
	}

	if system && d.inv.isSerial(host) && d.inv.SerialReadyCommand != "" {
		if others := d.otherSerialHosts(host); len(others) > 0 {
			d.ui.section("serial group pre-check")
			if notReady, ok := d.precheckSerial(others); !ok {
				fmt.Fprintf(os.Stderr, "deploy: serial group blocked: %s not ready before rollout; switching %s could break quorum\n", notReady, host)
				return 1
			}
		}
	}

	if system {
		d.ui.section(d.ui.phase("system") + " " + d.ui.host(host) + " rebuild")
		if status := d.activateSystem(host, attached, false); status != 0 {
			return status
		}
		if d.inv.isSerial(host) && !d.waitReady(host) {
			fmt.Fprintf(os.Stderr, "deploy: %s switched but failed the serial ready gate (serialReadyCommand)\n", host)
			if mode == "full" {
				d.ui.warnPartial([]string{host})
			}
			return 1
		}
	}

	if !home {
		return 0
	}
	// From here on, a failure in full mode leaves the system ahead of Home
	// Manager.
	status := d.deployHome(host, local, homeChecked)
	if status != 0 && mode == "full" {
		d.ui.warnPartial([]string{host})
	}
	return status
}

func (d *deployer) deployHome(host string, local, checked bool) int {
	if d.preflight && !local && !checked {
		d.ui.section("preflight")
		if !d.preflightHost(host, "hm") {
			fmt.Fprintf(os.Stderr, "deploy: %s failed Home Manager preflight; use --no-preflight to attempt anyway\n", host)
			return 1
		}
	}
	d.ui.section(d.ui.phase("hm") + " " + d.ui.host(host) + " switch")
	return d.activateHome(host, attached)
}
