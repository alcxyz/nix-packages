package main

import (
	"encoding/json"
	"fmt"
	"os"
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
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
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
	return os.WriteFile(path, append(data, '\n'), 0o644)
}
