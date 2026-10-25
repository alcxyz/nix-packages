package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// report collects per-host results and timings for one run. Results are
// keyed by phase and host; timings by phase, host and step ("evaluate",
// "activate" or "gate").
type report struct {
	results map[string]string
	times   map[string]time.Duration
	failed  bool
	started time.Time
	build   time.Duration
	// profile is the build profile the run used or, for a dry run, would
	// use; pendingBuilds is how many derivations it counted to choose it.
	profile       string
	pendingBuilds *int
}

func newReport() *report {
	return &report{results: map[string]string{}, times: map[string]time.Duration{}, started: time.Now()}
}

func (r *report) record(phase, host, status string) {
	r.results[phase+":"+host] = status
	if status == "failed" {
		r.failed = true
	}
}

// outcome turns an exit status into a result.
func outcome(status int) string {
	if status == 0 {
		return "ok"
	}
	return "failed"
}

func (r *report) result(phase, host string) string {
	if status, ok := r.results[phase+":"+host]; ok {
		return status
	}
	return "-"
}

func (r *report) addTime(t target, step string, elapsed time.Duration) {
	r.times[t.key()+":"+step] += elapsed
}

func (r *report) time(t target, step string) time.Duration {
	return r.times[t.key()+":"+step]
}

// hostTime is the time spent on one host outside the shared build.
func (r *report) hostTime(host string) time.Duration {
	var total time.Duration
	for _, phase := range []string{"system", "hm"} {
		for _, step := range []string{"evaluate", "activate", "gate"} {
			total += r.time(target{phase: phase, host: host}, step)
		}
	}
	return total
}

func formatDuration(elapsed time.Duration) string {
	if elapsed <= 0 {
		return "-"
	}
	if elapsed < time.Second {
		return "<1s"
	}
	return elapsed.Round(time.Second).String()
}

func (d *deployer) printSummary(selected []string, r *report) {
	d.ui.section("summary")
	fmt.Printf("%-12s %-10s %-13s %s\n", "host", "system", "home-manager", "time")
	for _, host := range selected {
		fmt.Printf("%-12s %-10s %-13s %s\n", host, r.result("system", host), r.result("hm", host), formatDuration(r.hostTime(host)))
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
	fmt.Printf("shared build: %s, total: %s\n", formatDuration(r.build), formatDuration(time.Since(r.started)))
}

type phaseReport struct {
	Status          string  `json:"status"`
	EvaluateSeconds float64 `json:"evaluateSeconds"`
	ActivateSeconds float64 `json:"activateSeconds"`
	GateSeconds     float64 `json:"gateSeconds,omitempty"`
}

type hostReport struct {
	Host        string      `json:"host"`
	System      phaseReport `json:"system"`
	HomeManager phaseReport `json:"homeManager"`
}

type runReport struct {
	Version       string       `json:"version"`
	BuildProfile  string       `json:"buildProfile,omitempty"`
	PendingBuilds *int         `json:"pendingBuilds,omitempty"`
	BuildSeconds  float64      `json:"buildSeconds"`
	TotalSeconds  float64      `json:"totalSeconds"`
	Failed        bool         `json:"failed"`
	Hosts         []hostReport `json:"hosts"`
}

// saveReport writes the JSON report when one was requested, on every exit
// path, so a failed run never leaves a previous run's report in place.
func (d *deployer) saveReport(selected []string, r *report) bool {
	if d.jsonPath == "" {
		return true
	}
	if err := d.writeJSON(d.jsonPath, selected, r); err != nil {
		fmt.Fprintf(os.Stderr, "deploy: cannot write %s: %v\n", d.jsonPath, err)
		return false
	}
	return true
}

// writeFailedJSON replaces a report when a run fails before it has hosts.
func writeFailedJSON(path string) {
	data, _ := json.MarshalIndent(runReport{Version: version, Failed: true, Hosts: []hostReport{}}, "", "  ")
	if err := writeReportFile(path, append(data, '\n')); err != nil {
		fmt.Fprintf(os.Stderr, "deploy: cannot write %s: %v\n", path, err)
	}
}

// writeJSON writes the results and timings in machine-readable form, so
// runs can be compared over time.
func (d *deployer) writeJSON(path string, selected []string, r *report) error {
	out := runReport{
		Version:       version,
		BuildProfile:  r.profile,
		PendingBuilds: r.pendingBuilds,
		BuildSeconds:  r.build.Seconds(),
		TotalSeconds:  time.Since(r.started).Seconds(),
		Failed:        r.failed,
	}
	for _, host := range selected {
		phase := func(name string) phaseReport {
			t := target{phase: name, host: host}
			return phaseReport{
				Status:          r.result(name, host),
				EvaluateSeconds: r.time(t, "evaluate").Seconds(),
				ActivateSeconds: r.time(t, "activate").Seconds(),
				GateSeconds:     r.time(t, "gate").Seconds(),
			}
		}
		out.Hosts = append(out.Hosts, hostReport{Host: host, System: phase("system"), HomeManager: phase("hm")})
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	return writeReportFile(path, append(data, '\n'))
}

// The package pins GNU ls so ACL checks do not depend on an ambient ls variant.
var reportACLInspector = "ls"

// checkReportACL refuses permissions that cannot be preserved with Chmod.
// GNU ls (a runtime dependency) uses the platform ACL API on Linux and Darwin.
// Its long format appends '+' for an ACL, '.' for only a security context, and
// '?' when ACL inspection fails. Inspect only the mode field, never filenames.
func checkReportACL(path string) error {
	output, err := exec.Command(reportACLInspector, "--directory", "--format=long", "--numeric-uid-gid", "--color=never", "--quoting-style=escape", "--", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("inspect report ACL for %s: %w: %s", path, err, strings.TrimSpace(string(output)))
	}
	fields := strings.Fields(string(output))
	if len(fields) == 0 || (len(fields[0]) != 10 && !(len(fields[0]) == 11 && fields[0][10] == '.')) {
		return fmt.Errorf("report %s has an ACL or its ACL could not be verified; refusing atomic replacement", path)
	}
	return nil
}

// reportTemps holds the temporary files writeReportFile has not yet renamed or
// removed, so the interrupt handler can remove them before os.Exit skips the
// writers' deferred cleanup.
var reportTemps sync.Map

func removeReportTemps() {
	reportTemps.Range(func(name, _ any) bool {
		os.Remove(name.(string))
		return true
	})
}

// writeReportFile publishes only complete reports. An interrupt can exit while
// another writer is active, so writers must never truncate the published file.
// Replacing the report by rename needs write access to its directory.
func writeReportFile(path string, data []byte) error {
	mode := os.FileMode(0o600)
	var owner *syscall.Stat_t
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("report destination %s is not a regular file", path)
		}
		if err := checkReportACL(path); err != nil {
			return err
		}
		mode = info.Mode().Perm()
		owner = info.Sys().(*syscall.Stat_t)
	} else if !os.IsNotExist(err) {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".nixfleet-report-*")
	if err != nil {
		return err
	}
	reportTemps.Store(temp.Name(), struct{}{})
	defer func() {
		os.Remove(temp.Name())
		reportTemps.Delete(temp.Name())
	}()
	// Inherited ACLs can grant access independently of mode bits on Darwin,
	// or become effective when Chmod restores a group mask on Linux. Refuse
	// them before writing any report data to the temporary file.
	if err := checkReportACL(temp.Name()); err != nil {
		temp.Close()
		return err
	}
	_, err = temp.Write(data)
	if err == nil && owner != nil {
		var info os.FileInfo
		info, err = temp.Stat()
		if err == nil {
			current := info.Sys().(*syscall.Stat_t)
			if current.Uid != owner.Uid || current.Gid != owner.Gid {
				err = temp.Chown(int(owner.Uid), int(owner.Gid))
				if err != nil {
					err = fmt.Errorf("preserve ownership of report %s (uid %d, gid %d): %w", path, owner.Uid, owner.Gid, err)
				}
			}
		}
	}
	if err == nil {
		err = temp.Chmod(mode)
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(temp.Name(), path)
}
