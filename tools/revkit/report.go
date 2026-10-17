package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"revkit/review"
)

var verdictOrder = map[string]int{"confirmed": 0, "partly": 1, "unverified": 2, "rejected": 3}
var severityOrder = map[string]int{"high": 0, "medium": 1, "low": 2}

func rank(order map[string]int, key string) int {
	if value, ok := order[key]; ok {
		return value
	}
	return len(order)
}

// write saves results.json and report.md in the run directory.
func (r *reviewRun) write() error {
	sort.SliceStable(r.report.Jobs, func(i, j int) bool {
		a, b := r.report.Jobs[i], r.report.Jobs[j]
		if a.Kind != b.Kind {
			return a.Kind == "find"
		}
		return a.Reviewer+a.Lens+a.Author < b.Reviewer+b.Lens+b.Author
	})
	sort.SliceStable(r.report.Findings, func(i, j int) bool {
		a, b := r.report.Findings[i], r.report.Findings[j]
		if rank(verdictOrder, a.Verdict) != rank(verdictOrder, b.Verdict) {
			return rank(verdictOrder, a.Verdict) < rank(verdictOrder, b.Verdict)
		}
		if rank(severityOrder, a.Severity) != rank(severityOrder, b.Severity) {
			return rank(severityOrder, a.Severity) < rank(severityOrder, b.Severity)
		}
		return a.ID < b.ID
	})
	encoded, err := json.MarshalIndent(r.report, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(r.dir, "results.json"), append(encoded, '\n'), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(r.dir, "report.md"), []byte(r.markdown()), 0o644)
}

func (r *reviewRun) markdown() string {
	var b strings.Builder
	rep := r.report
	fmt.Fprintf(&b, "# repo-review: %s at %s\n\n", rep.Repository, rep.Commit[:12])
	complete := "yes"
	if !rep.Complete {
		complete = "no, some reviews are missing"
	}
	fmt.Fprintf(&b, "revkit %s, %s to %s. Complete: %s.\n\n", rep.Version,
		rep.Started.Local().Format("2006-01-02 15:04"), rep.Finished.Local().Format("15:04"), complete)
	b.WriteString("| Job | Reviewer | Model | Status | Findings | Seconds | Input incl. cache | Cache reads | Output |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|\n")
	for _, job := range rep.Jobs {
		subject := job.Lens
		if job.Kind == "verify" {
			subject = "verify " + job.Author
		}
		total, known := job.Usage.TotalInput(job.Client)
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %d | %d | %s | %s | %s |\n", subject, job.Reviewer, job.Model,
			job.Status, job.Findings, job.Seconds, tokens(total, known), tokenPointer(job.Usage.CachedInputTokens),
			tokenPointer(job.Usage.OutputTokens))
	}
	if missing := r.missing(); len(missing) > 0 {
		b.WriteString("\nMissing reviews (a failed reviewer is not a review without findings):\n\n")
		for _, line := range missing {
			fmt.Fprintf(&b, "- %s\n", line)
		}
	}
	if windows := latestWindows(rep.Jobs); len(windows) > 0 {
		b.WriteString("\nUsage windows at the end of the run: " + windowSummary(windows) + "\n")
	}
	fmt.Fprintf(&b, "\n## Findings (%s)\n", verdictCounts(rep.Findings))
	for _, f := range rep.Findings {
		fmt.Fprintf(&b, "\n### %s [%s] %s\n\n", f.ID, f.Verdict, oneLine(f.Title))
		fmt.Fprintf(&b, "*%s*, %s\n\n", f.Severity, strings.Join(f.Paths, ", "))
		fmt.Fprintf(&b, "**Claim:** %s\n\n**Evidence:** %s\n\n**Suggestion:** %s\n", f.Claim, f.Evidence, f.Suggestion)
		if f.Verifier != "" {
			fmt.Fprintf(&b, "\n**Verifier (%s):** %s\n", f.Verifier, f.Reason)
		}
	}
	return b.String()
}

// summary prints the outcome after the per-job progress lines.
func (r *reviewRun) summary() {
	rep := r.report
	fmt.Fprintf(r.out, "\n%d findings: %s\n", len(rep.Findings), verdictCounts(rep.Findings))
	for _, line := range r.missing() {
		fmt.Fprintf(r.out, "missing review: %s\n", line)
	}
	// Sum what each client reported; a run without counters is named, not
	// counted as zero.
	type tally struct{ runs, unreported, input, output int }
	tallies := map[string]*tally{}
	var clients []string
	for _, job := range rep.Jobs {
		t, ok := tallies[job.Client]
		if !ok {
			t = &tally{}
			tallies[job.Client] = t
			clients = append(clients, job.Client)
		}
		t.runs++
		total, known := job.Usage.TotalInput(job.Client)
		if !known || job.Usage.OutputTokens == nil {
			t.unreported++
			continue
		}
		t.input += int(total)
		t.output += int(*job.Usage.OutputTokens)
	}
	sort.Strings(clients)
	for _, client := range clients {
		t := tallies[client]
		line := fmt.Sprintf("usage: %s, %d runs: %s input including cache reads, %s output", client, t.runs,
			tokens(int64(t.input), true), tokens(int64(t.output), true))
		if t.unreported > 0 {
			line += fmt.Sprintf(" (%d runs reported no usage)", t.unreported)
		}
		fmt.Fprintln(r.out, line)
	}
	if windows := latestWindows(rep.Jobs); len(windows) > 0 {
		fmt.Fprintf(r.out, "windows: %s\n", windowSummary(windows))
	} else {
		fmt.Fprintln(r.out, "windows: unknown")
	}
	for _, job := range rep.Jobs {
		for _, note := range job.Notes {
			fmt.Fprintf(r.out, "note (%s): %s\n", job.Reviewer, note)
		}
	}
	fmt.Fprintf(r.out, "report: %s\n", filepath.Join(r.dir, "report.md"))
}

func (r *reviewRun) missing() []string {
	var lines []string
	for _, job := range r.report.Jobs {
		if job.Status == review.StatusOK {
			continue
		}
		subject := job.Lens
		if job.Kind == "verify" {
			subject = "verification of " + job.Author
		}
		lines = append(lines, fmt.Sprintf("%s %s: %s (%s)", job.Reviewer, subject, job.Status, job.Problem))
	}
	return lines
}

func verdictCounts(findings []finding) string {
	counts := map[string]int{}
	for _, f := range findings {
		counts[f.Verdict]++
	}
	var parts []string
	for _, name := range []string{"confirmed", "partly", "rejected", "unverified"} {
		parts = append(parts, fmt.Sprintf("%d %s", counts[name], name))
	}
	return strings.Join(parts, ", ")
}

// latestWindows keeps the last report of each client's windows.
func latestWindows(jobs []jobRecord) []review.Window {
	type seen struct {
		window review.Window
		at     time.Time
	}
	latest := map[string]seen{}
	for _, job := range jobs {
		at := job.Started.Add(time.Duration(job.Seconds) * time.Second)
		for _, w := range job.Windows {
			key := w.Client + " " + w.Name
			if previous, ok := latest[key]; !ok || at.After(previous.at) {
				latest[key] = seen{window: w, at: at}
			}
		}
	}
	keys := make([]string, 0, len(latest))
	for key := range latest {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	windows := make([]review.Window, len(keys))
	for i, key := range keys {
		windows[i] = latest[key].window
	}
	return windows
}

func windowSummary(windows []review.Window) string {
	parts := make([]string, len(windows))
	for i, w := range windows {
		parts[i] = fmt.Sprintf("%s %s %.0f%% (resets %s)", w.Client, w.Name, w.UsedPercent,
			w.ResetsAt.Local().Format("Mon 15:04"))
	}
	return strings.Join(parts, ", ")
}

func tokens(value int64, known bool) string {
	if !known {
		return "unknown"
	}
	switch {
	case value >= 1_000_000:
		return fmt.Sprintf("%.2fM", float64(value)/1e6)
	case value >= 1_000:
		return fmt.Sprintf("%dk", (value+500)/1000)
	}
	return fmt.Sprintf("%d", value)
}

func tokenPointer(value *int64) string {
	if value == nil {
		return "unknown"
	}
	return tokens(*value, true)
}

func oneLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}
