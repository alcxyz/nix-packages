package main

import (
	"fmt"
	"os"
	"time"
)

// deployHost deploys one host. mode is "full" (system, then Home Manager),
// "nixos" or "hm". Every changed target is built before anything is
// activated, so a build failure leaves the host untouched. A failed activation ends the run
// with that step's status.
func (d *deployer) deployHost(name, mode string) int {
	host := d.inv.canonical(name)
	r := newReport()
	status := d.deploySingle(host, mode, r)
	if status != 0 {
		r.failed = true
	}
	if !d.saveReport([]string{host}, r) && status == 0 {
		status = 1
	}
	d.finishRecord([]string{host}, r, status != 0)
	return status
}

func (d *deployer) deploySingle(host, mode string, r *report) int {
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

	if d.preflight && host != d.self {
		d.ui.section("preflight")
		if system && !d.preflightHost(host, "nixos") {
			fmt.Fprintf(os.Stderr, "deploy: %s failed system preflight; use --no-preflight to attempt anyway\n", host)
			return 1
		}
		if home && !d.preflightHost(host, "hm") {
			fmt.Fprintf(os.Stderr, "deploy: %s failed Home Manager preflight; use --no-preflight to attempt anyway\n", host)
			return 1
		}
	}

	var targets []target
	if system {
		targets = append(targets, target{phase: "system", host: host})
	}
	if home {
		targets = append(targets, target{phase: "hm", host: host})
	}
	return d.activateHost(host, mode, targets, r)
}

func (d *deployer) activateHost(host, mode string, targets []target, r *report) int {
	evaluated := d.evaluate(targets, r)
	if r.failed {
		fmt.Fprintf(os.Stderr, "deploy: %s did not evaluate; nothing was activated\n", host)
		return 1
	}
	changed := d.plan(targets, evaluated, r)
	if d.dryRun {
		for _, t := range changed {
			r.record(t.phase, t.host, "planned")
		}
		d.previewProfile(changed, evaluated, r)
		return 0
	}
	if len(changed) == 0 {
		fmt.Printf("deploy: %s is up to date; nothing to activate (use --force to activate anyway)\n", host)
		return 0
	}
	built := d.buildTargets(changed, evaluated, r)
	if r.failed {
		fmt.Fprintf(os.Stderr, "deploy: %s did not build; nothing was activated\n", host)
		return 1
	}

	systemSwitched := false
	for _, t := range changed {
		path := built[t.key()]
		if t.phase == "system" {
			if status := d.switchHost(t, path, mode, r); status != 0 {
				return status
			}
			systemSwitched = true
			continue
		}
		d.ui.section(d.label(t) + " switch")
		started := time.Now()
		if !d.failed.begin(t, path) {
			r.record(t.phase, t.host, "failed")
			return 1
		}
		status := d.activateHome(host, path, attached)
		r.addTime(t, "activate", time.Since(started))
		d.recordActivation(r, t, status)
		if status != 0 {
			// The system is now ahead of Home Manager.
			if systemSwitched {
				d.ui.warnPartial([]string{host})
			}
			return status
		}
	}
	return 0
}

// switchHost activates a single host's system, behind its serial group's
// pre-check and ready gate.
func (d *deployer) switchHost(t target, path, mode string, r *report) int {
	group := d.inv.group(t.host)
	if group != nil && group.ReadyCommand != "" {
		if others := group.others(t.host); len(others) > 0 {
			d.ui.section("serial group pre-check")
			if notReady, ok := d.precheck(group, others); !ok {
				fmt.Fprintf(os.Stderr, "deploy: serial group blocked: %s not ready before rollout; switching %s could break quorum\n", notReady, t.host)
				return 1
			}
		}
	}

	if d.needsRebuild(t) && !d.rebuildSupportsStorePaths() {
		r.record(t.phase, t.host, "failed")
		return 1
	}
	d.ui.section(d.label(t) + " switch")
	started := time.Now()
	if !d.failed.begin(t, path) {
		r.record(t.phase, t.host, "failed")
		return 1
	}
	streams, log := d.switchLogged(t)
	status := d.activateSystem(t.host, path, streams, false)
	log.Close()
	r.addTime(t, "activate", time.Since(started))
	if status != 0 || group == nil {
		d.recordActivation(r, t, status)
		return status
	}
	// A member that switched but did not rejoin stays unfinished, so the
	// next run switches and gates it again.
	started = time.Now()
	ready := d.waitReady(group, t.host)
	r.addTime(t, "gate", time.Since(started))
	d.failed.finish(t, ready)
	r.record(t.phase, t.host, "ok")
	if !ready {
		r.record(t.phase, t.host, "failed")
		fmt.Fprintf(os.Stderr, "deploy: %s switched but failed the serial ready gate (serialReadyCommand)\n", t.host)
		if mode == "full" {
			d.ui.warnPartial([]string{t.host})
		}
		return 1
	}
	return 0
}
