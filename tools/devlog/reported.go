package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Each daily entry records the commits it reported on a final comment line,
// invisible when rendered. A push can carry commits reported on earlier days,
// such as a promotion moving a branch over existing work, so later entries
// skip them. Gap fills also skip commits recorded by any later entry, leaving
// them in the first entry written. Commits first pushed now are kept whenever
// they were committed.

const (
	reportedPrefix = "<!-- devlog-commits:"
	reportedSuffix = "-->"
	// reportedLookback covers the catch-up range, so promotions within it are
	// recognised.
	reportedLookback = 30
)

func reportedLine(commits []commitRef) string {
	shas := make([]string, 0, len(commits))
	for _, c := range commits {
		shas = append(shas, c.SHA)
	}
	return reportedPrefix + " " + strings.Join(shas, " ") + " " + reportedSuffix + "\n"
}

// reportedCommits returns SHAs recorded within the backward window or by any
// later daily entry. The date being generated never deduplicates against itself.
func reportedCommits(repoPath string, date time.Time) map[string]bool {
	seen := make(map[string]bool)
	dir := filepath.Join(repoPath, "devlog")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return seen
	}
	current := date.Format("2006-01-02")
	earliest := date.AddDate(0, 0, -reportedLookback).Format("2006-01-02")
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		ds := strings.TrimSuffix(entry.Name(), ".md")
		if _, err := time.Parse("2006-01-02", ds); err != nil || ds == current || ds < earliest {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			if !strings.HasPrefix(line, reportedPrefix) {
				continue
			}
			fields := strings.Fields(strings.TrimSuffix(strings.TrimPrefix(line, reportedPrefix), reportedSuffix))
			for _, sha := range fields {
				seen[sha] = true
			}
		}
	}
	return seen
}

func withoutReported(commits []commitRef, reported map[string]bool) []commitRef {
	var fresh []commitRef
	for _, c := range commits {
		if !reported[c.SHA] {
			fresh = append(fresh, c)
		}
	}
	return fresh
}

func appendReported(path string, commits []commitRef) error {
	if len(commits) == 0 {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	content := strings.TrimRight(string(data), "\n") + "\n\n" + reportedLine(commits)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return fmt.Errorf("record reported commits: %w", err)
	}
	return nil
}

// stripReported removes the record before an entry is summarised or
// stitched into a weekly summary.
func stripReported(content string) string {
	var kept []string
	for _, line := range strings.Split(content, "\n") {
		if !strings.HasPrefix(line, reportedPrefix) {
			kept = append(kept, line)
		}
	}
	return strings.TrimRight(strings.Join(kept, "\n"), "\n") + "\n"
}
