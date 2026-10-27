package main

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// sudoRefresh keeps cached local sudo credentials alive while fleet jobs run.
const sudoRefresh = 60 * time.Second

// job is one host's activation in a parallel phase.
type job struct {
	target target
	path   string
	run    func(streams stdio) int
}

// runJobs runs every job at once and records each result and duration. Each
// output line carries the phase and host. One failed host never hides the
// outcome of the others or stops later phases.
func (d *deployer) runJobs(jobs []job, r *report) {
	var lock sync.Mutex
	statuses := make([]int, len(jobs))
	elapsed := make([]time.Duration, len(jobs))
	var wait sync.WaitGroup
	for i, j := range jobs {
		wait.Add(1)
		go func() {
			defer wait.Done()
			tag := d.label(j.target)
			out := &prefixWriter{lock: &lock, dst: os.Stdout, prefix: tag}
			errOut := &prefixWriter{lock: &lock, dst: os.Stderr, prefix: tag}
			started := time.Now()
			// Recorded inside the job, so an interrupted run keeps the
			// results of jobs that already ended.
			if d.failed.begin(j.target, j.path) {
				streams, log := d.logged(stdio{in: bytes.NewReader(nil), out: out, err: errOut}, j.target, "activate")
				statuses[i] = j.run(streams)
				log.Close()
				d.failed.finish(j.target, statuses[i] == 0)
			} else {
				// Without the record a failure could later pass as unchanged.
				statuses[i] = 1
			}
			elapsed[i] = time.Since(started)
			out.flush()
			errOut.flush()
		}()
	}
	wait.Wait()
	for i, j := range jobs {
		r.addTime(j.target, "activate", elapsed[i])
		r.record(j.target.phase, j.target.host, outcome(statuses[i]))
	}
}

// startSudoKeepalive asks for the local password before the slow preflight
// and build steps, so the prompt appears when the command starts, then
// refreshes the credentials until stopped.
func (d *deployer) startSudoKeepalive() (func(), bool) {
	fmt.Printf("deploy: sudo is needed to switch %s locally\n", d.self)
	if (command{argv: []string{"sudo", "-v"}}).run() != 0 {
		return nil, false
	}
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(sudoRefresh)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if _, status := capture(0, "sudo", "-n", "-v"); status != 0 {
					return
				}
			}
		}
	}()
	return func() { close(done) }, true
}

func (d *deployer) deployAll(mode string) int {
	selected := d.inv.fleet(d.self)
	r := newReport()
	status := d.deployFleet(mode, selected, r)
	if status != 0 {
		r.failed = true
	}
	if !d.saveReport(selected, r) && status == 0 {
		status = 1
	}
	d.finishRecord(selected, r, status != 0)
	return status
}

func (d *deployer) deployFleet(mode string, selected []string, r *report) int {
	if d.self != d.inv.OperatorHost && !d.buildHere {
		fmt.Fprintf(os.Stderr, "deploy --all must be run from %s, or pass --here to run from %s\n", d.inv.OperatorHost, d.self)
		return 1
	}
	systems := mode == "nixos" || mode == "full"
	homes := mode == "hm" || mode == "full"

	// Each serial group that cannot switch in this run, with the reason.
	blocked := map[*serialGroup]string{}
	var targets []target

	if systems {
		if contains(selected, d.self) && !d.dryRun {
			stop, ok := d.startSudoKeepalive()
			if !ok {
				return 1
			}
			defer stop()
		}
		available, skipped, ok := d.filterReachable("nixos", selected)
		if !ok {
			if d.failUnreachable {
				return 1
			}
			r.failed = true
		}
		for _, host := range skipped {
			r.record("system", host, "skipped")
			// One unreachable member already reduces a group; switching
			// another could leave too few members, so the whole group waits.
			if group := d.inv.group(host); group != nil && blocked[group] == "" {
				blocked[group] = "serial group blocked: " + host + " failed preflight; switching the others could break quorum"
				r.failed = true
			}
		}
		for _, host := range available {
			targets = append(targets, target{phase: "system", host: host})
		}
	}

	if homes {
		var candidates []string
		for _, host := range selected {
			if d.inv.usesHomeManager(host) {
				candidates = append(candidates, host)
			}
		}
		if len(candidates) == 0 {
			fmt.Println("deploy: no Home Manager-eligible hosts in selected fleet")
		} else {
			available, skipped, ok := d.filterReachable("hm", candidates)
			if !ok {
				if d.failUnreachable {
					return 1
				}
				r.failed = true
			}
			for _, host := range skipped {
				r.record("hm", host, "skipped")
			}
			for _, host := range available {
				targets = append(targets, target{phase: "hm", host: host})
			}
		}
	}

	evaluated := d.evaluate(targets, r)
	changed := d.plan(targets, evaluated, r)
	if d.dryRun {
		for _, t := range changed {
			r.record(t.phase, t.host, "planned")
		}
		d.previewProfile(changed, evaluated, r)
		return d.finish(selected, r)
	}
	built := d.buildTargets(changed, evaluated, r)
	var ready []target
	for _, t := range changed {
		if _, ok := built[t.key()]; ok {
			ready = append(ready, t)
		}
	}
	d.activateSystems(ready, built, blocked, r)
	d.activateHomes(ready, built, r)
	return d.finish(selected, r)
}

// activateSystems switches the changed systems: independent hosts in one
// parallel batch, hosts that prompt for a remote sudo password one at a time
// with this terminal attached, then each serial group.
func (d *deployer) activateSystems(changed []target, built map[string]string, blocked map[*serialGroup]string, r *report) {
	for _, t := range changed {
		if d.needsRebuild(t) && !d.rebuildSupportsStorePaths() {
			for _, t := range changed {
				if t.phase == "system" {
					r.record(t.phase, t.host, "failed")
				}
			}
			return
		}
	}
	var jobs []job
	var interactive []target
	members := map[*serialGroup]map[string]bool{}
	for _, t := range changed {
		if t.phase != "system" {
			continue
		}
		path := built[t.key()]
		switch group := d.inv.group(t.host); {
		case group != nil:
			if members[group] == nil {
				members[group] = map[string]bool{}
			}
			members[group][t.host] = true
		case t.host != d.self && d.inv.usesRemoteSudo(t.host):
			interactive = append(interactive, t)
		default:
			jobs = append(jobs, job{target: t, path: path, run: func(streams stdio) int {
				return d.activateSystem(t.host, path, streams, true)
			}})
		}
	}

	if len(jobs) > 0 || len(interactive) > 0 {
		d.ui.section("system switches")
	}
	if len(jobs) > 0 {
		d.runJobs(jobs, r)
	}
	for _, t := range interactive {
		path := built[t.key()]
		fmt.Printf("%s %s\n", d.label(t), strings.Join(d.systemCommand(t.host, d.systemAction(t.host), path, true), " "))
		if !d.failed.begin(t, path) {
			r.record("system", t.host, "failed")
			continue
		}
		started := time.Now()
		streams, log := d.switchLogged(t)
		status := d.activateSystem(t.host, path, streams, true)
		log.Close()
		r.addTime(t, "activate", time.Since(started))
		d.recordActivation(r, t, status)
	}

	for _, group := range d.inv.SerialGroups {
		failure := blocked[group]
		if len(members[group]) == 0 {
			// Nothing in this group changed; a blocked group still fails
			// the run, which preflight already recorded.
			if failure != "" {
				fmt.Fprintln(os.Stderr, "deploy: "+failure)
			}
			continue
		}
		d.switchGroup(group, members[group], built, failure, r)
	}
}

// switchGroup switches a serial group's changed members one at a time, in
// inventory order. Every member, changed or not, must be ready before the
// first switch, and the other members are checked again before each later
// one. The first switch, gate or check failure stops the group's rollout so
// its remaining members are left untouched.
func (d *deployer) switchGroup(group *serialGroup, changed map[string]bool, built map[string]string, failure string, r *report) {
	if failure == "" && group.ReadyCommand != "" {
		d.ui.section("serial group pre-check")
		if notReady, ok := d.precheck(group, group.Hosts); !ok {
			failure = "serial group blocked: " + notReady + " not ready before rollout; switching the others could break quorum"
		}
	}

	var skipped []string
	switched := false
	for _, host := range group.Hosts {
		if !changed[host] {
			continue
		}
		t := target{phase: "system", host: host}
		if failure == "" && switched && group.ReadyCommand != "" {
			d.ui.section("serial group check before " + host)
			if notReady, ok := d.precheck(group, group.others(host)); !ok {
				failure = "serial rollout stopped: " + notReady + " not ready before switching " + host + "; switching it could break quorum"
			}
		}
		if failure != "" {
			r.record("system", host, "skipped")
			skipped = append(skipped, host)
			continue
		}
		switched = true
		path := built[t.key()]
		d.ui.section(d.label(t) + " serial switch")
		fmt.Printf("%s %s\n", d.label(t), strings.Join(d.systemCommand(host, d.systemAction(host), path, true), " "))
		if !d.failed.begin(t, path) {
			r.record("system", host, "failed")
			failure = "serial rollout stopped: cannot record the activation of " + host
			continue
		}
		started := time.Now()
		streams, log := d.switchLogged(t)
		status := d.activateSystem(host, path, streams, true)
		log.Close()
		r.addTime(t, "activate", time.Since(started))
		if status != 0 {
			d.failed.finish(t, false)
			r.record("system", host, "failed")
			failure = "serial rollout stopped: " + host + " failed its system switch"
			continue
		}
		// A member that switched but did not rejoin stays unfinished, so the
		// next run switches and gates it again.
		started = time.Now()
		ready := d.waitReady(group, host)
		r.addTime(t, "gate", time.Since(started))
		d.failed.finish(t, ready)
		if !ready {
			r.record("system", host, "failed")
			failure = "serial rollout stopped: " + host + " failed the serial ready gate (serialReadyCommand)"
			continue
		}
		r.record("system", host, "ok")
	}
	if failure != "" {
		r.failed = true
		fmt.Fprintln(os.Stderr, "deploy: "+failure)
		if len(skipped) > 0 {
			fmt.Fprintln(os.Stderr, "deploy: skipped serial hosts: "+joinHosts(skipped))
		}
	}
}

func (d *deployer) activateHomes(changed []target, built map[string]string, r *report) {
	var jobs []job
	for _, t := range changed {
		if t.phase != "hm" {
			continue
		}
		path := built[t.key()]
		jobs = append(jobs, job{target: t, path: path, run: func(streams stdio) int {
			return d.activateHome(t.host, path, streams)
		}})
	}
	if len(jobs) > 0 {
		d.ui.section("home-manager switches")
		d.runJobs(jobs, r)
	}
}

// finish prints the summary, warns about hosts whose system switched while
// Home Manager did not, and returns the exit status.
func (d *deployer) finish(selected []string, r *report) int {
	d.printSummary(selected, r)
	var lagging []string
	for _, host := range selected {
		home := r.result("hm", host)
		if r.result("system", host) == "ok" && (home == "failed" || home == "skipped") {
			lagging = append(lagging, host)
		}
	}
	if len(lagging) > 0 {
		d.ui.warnPartial(lagging)
	}
	if r.failed {
		fmt.Fprintln(os.Stderr, "deploy: one or more fleet jobs failed; see the summary above")
		return 1
	}
	return 0
}
