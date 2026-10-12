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

// fleet holds the per-host results of one --all run, keyed by phase and host.
type fleet struct {
	results map[string]string
	failed  bool
}

func (f *fleet) record(phase, host, status string) {
	f.results[phase+":"+host] = status
	if status == "failed" {
		f.failed = true
	}
}

func (f *fleet) result(phase, host string) string {
	if status, ok := f.results[phase+":"+host]; ok {
		return status
	}
	return "-"
}

// job is one host's work in a parallel phase.
type job struct {
	host string
	run  func(streams stdio) int
}

// runJobs runs every job at once and records each result. Each output line
// carries the phase and host. One failed host never hides the outcome of the
// others or stops later phases.
func (d *deployer) runJobs(phase string, jobs []job, f *fleet) {
	var lock sync.Mutex
	statuses := make([]int, len(jobs))
	var wait sync.WaitGroup
	for i, j := range jobs {
		wait.Add(1)
		go func(i int, j job) {
			defer wait.Done()
			tag := d.ui.phase(phase) + " " + d.ui.host(j.host)
			out := &prefixWriter{lock: &lock, dst: os.Stdout, prefix: tag}
			errOut := &prefixWriter{lock: &lock, dst: os.Stderr, prefix: tag}
			statuses[i] = j.run(stdio{in: bytes.NewReader(nil), out: out, err: errOut})
			out.flush()
			errOut.flush()
		}(i, j)
	}
	wait.Wait()
	for i, j := range jobs {
		if statuses[i] == 0 {
			f.record(phase, j.host, "ok")
		} else {
			f.record(phase, j.host, "failed")
		}
	}
}

// startSudoKeepalive asks for the local password before the slow preflight
// and evaluation steps, so the prompt appears when the command starts, then
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
	if d.self != d.inv.OperatorHost && !d.buildHere {
		fmt.Fprintf(os.Stderr, "deploy --all must be run from %s, or pass --here to run from %s\n", d.inv.OperatorHost, d.self)
		return 1
	}
	selected := d.inv.DeployAllHosts
	if d.self != d.inv.OperatorHost {
		selected = d.inv.RemoteHosts
	}
	var homeCandidates []string
	for _, host := range selected {
		if d.inv.usesHomeManager(host) {
			homeCandidates = append(homeCandidates, host)
		}
	}

	f := &fleet{results: map[string]string{}}
	if mode == "nixos" || mode == "full" {
		if status := d.deploySystems(selected, f); status != 0 {
			return status
		}
	}

	if mode == "hm" || mode == "full" {
		if len(homeCandidates) == 0 {
			fmt.Println("deploy: no Home Manager-eligible hosts in selected fleet")
		} else {
			available, skipped, ok := d.filterReachable("hm", homeCandidates)
			if !ok {
				if d.failUnreachable {
					return 1
				}
				f.failed = true
			}
			for _, host := range skipped {
				f.record("hm", host, "skipped")
			}
			d.ui.section("home-manager switches")
			var jobs []job
			for _, host := range available {
				jobs = append(jobs, job{host: host, run: func(streams stdio) int { return d.activateHome(host, streams) }})
			}
			if len(jobs) > 0 {
				d.runJobs("hm", jobs, f)
			}
		}
	}

	d.printSummary(selected, f)
	var lagging []string
	for _, host := range selected {
		home := f.result("hm", host)
		if f.result("system", host) == "ok" && (home == "failed" || home == "skipped") {
			lagging = append(lagging, host)
		}
	}
	if len(lagging) > 0 {
		d.ui.warnPartial(lagging)
	}
	if f.failed {
		fmt.Fprintln(os.Stderr, "deploy: one or more fleet jobs failed; see the summary above")
		return 1
	}
	return 0
}

// deploySystems runs the system phase of a fleet run. It returns a non-zero
// status only when the run must stop before the Home Manager phase.
func (d *deployer) deploySystems(candidates []string, f *fleet) int {
	if contains(candidates, d.self) {
		stop, ok := d.startSudoKeepalive()
		if !ok {
			return 1
		}
		defer stop()
	}

	available, skipped, ok := d.filterReachable("nixos", candidates)
	if !ok {
		if d.failUnreachable {
			return 1
		}
		f.failed = true
	}
	serialFailure := ""
	for _, host := range skipped {
		f.record("system", host, "skipped")
		// One unreachable member already reduces a quorum group; switching
		// another could leave too few members, so the whole group waits.
		if serialFailure == "" && d.inv.isSerial(host) {
			serialFailure = "serial group blocked: " + host + " failed preflight; switching the others could break quorum"
			f.failed = true
		}
	}

	d.ui.section("system rebuilds")
	var jobs []job
	var interactive []string
	serialSelected := map[string]bool{}
	for _, host := range available {
		switch {
		case d.inv.isSerial(host):
			serialSelected[host] = true
		case host != d.self && d.inv.usesRemoteSudo(host):
			interactive = append(interactive, host)
		default:
			jobs = append(jobs, job{host: host, run: func(streams stdio) int { return d.activateSystem(host, streams, true) }})
		}
	}
	if len(jobs) > 0 {
		d.runJobs("system", jobs, f)
	}

	// Hosts that prompt for a remote sudo password switch one at a time with
	// this terminal attached.
	for _, host := range interactive {
		fmt.Printf("%s %s %s\n", d.ui.phase("system"), d.ui.host(host), strings.Join(d.systemCommand(host, d.inv.activationMode(host), true), " "))
		if d.activateSystem(host, attached, true) == 0 {
			f.record("system", host, "ok")
		} else {
			f.record("system", host, "failed")
		}
	}

	d.deploySerial(serialSelected, serialFailure, f)
	return 0
}

// deploySerial switches the selected serial hosts one at a time, in
// inventory order, after the parallel batch. Every serial host, selected or
// not, must be ready before the first switch, and the other members are
// checked again before each later one. The first switch, gate or check
// failure stops the rollout so the remaining members are left untouched.
func (d *deployer) deploySerial(selected map[string]bool, failure string, f *fleet) {
	if failure == "" && len(selected) > 0 && d.inv.SerialReadyCommand != "" {
		d.ui.section("serial group pre-check")
		if notReady, ok := d.precheckSerial(d.inv.SerialSystemHosts); !ok {
			failure = "serial group blocked: " + notReady + " not ready before rollout; switching the others could break quorum"
			f.failed = true
		}
	}

	var skipped []string
	switched := false
	for _, host := range d.inv.SerialSystemHosts {
		if !selected[host] {
			continue
		}
		if failure == "" && switched && d.inv.SerialReadyCommand != "" {
			d.ui.section("serial group check before " + host)
			if notReady, ok := d.precheckSerial(d.otherSerialHosts(host)); !ok {
				failure = "serial rollout stopped: " + notReady + " not ready before switching " + host + "; switching it could break quorum"
			}
		}
		if failure != "" {
			f.record("system", host, "skipped")
			skipped = append(skipped, host)
			continue
		}
		switched = true
		d.ui.section(d.ui.phase("system") + " " + d.ui.host(host) + " serial rebuild")
		fmt.Printf("%s %s %s\n", d.ui.phase("system"), d.ui.host(host), strings.Join(d.systemCommand(host, d.inv.activationMode(host), true), " "))
		switch {
		case d.activateSystem(host, attached, true) != 0:
			f.record("system", host, "failed")
			failure = "serial rollout stopped: " + host + " failed its system switch"
		case !d.waitReady(host):
			f.record("system", host, "failed")
			failure = "serial rollout stopped: " + host + " failed the serial ready gate (serialReadyCommand)"
		default:
			f.record("system", host, "ok")
		}
	}
	if failure != "" {
		f.failed = true
		fmt.Fprintln(os.Stderr, "deploy: "+failure)
		if len(skipped) > 0 {
			fmt.Fprintln(os.Stderr, "deploy: skipped serial hosts: "+joinHosts(skipped))
		}
	}
}

func (d *deployer) printSummary(selected []string, f *fleet) {
	d.ui.section("summary")
	fmt.Printf("%-12s %-10s %s\n", "host", "system", "home-manager")
	for _, host := range selected {
		fmt.Printf("%-12s %-10s %s\n", host, f.result("system", host), f.result("hm", host))
	}
	var excluded []string
	for _, host := range d.inv.KnownHosts {
		if !contains(selected, host) {
			excluded = append(excluded, host)
		}
	}
	if len(excluded) > 0 {
		fmt.Println("not selected by --all: " + joinHosts(excluded))
	}
}
