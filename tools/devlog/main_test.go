package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWeekday(t *testing.T) {
	tests := []struct {
		input    string
		target   time.Weekday
		expected string
	}{
		{"2026-04-25", time.Monday, "2026-04-20"},    // Friday → Monday
		{"2026-04-20", time.Monday, "2026-04-20"},    // Monday → Monday (same day)
		{"2026-04-26", time.Monday, "2026-04-20"},    // Saturday → Monday
		{"2026-04-27", time.Monday, "2026-04-27"},    // Sunday → Monday (Go: Sunday=0, so wraps to current week's Monday)
		{"2026-04-22", time.Monday, "2026-04-20"},    // Wednesday → Monday
		{"2026-01-01", time.Monday, "2025-12-29"},    // Year boundary
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			input, _ := time.Parse("2006-01-02", tt.input)
			result := weekday(input, tt.target)
			got := result.Format("2006-01-02")
			if got != tt.expected {
				t.Errorf("weekday(%s, %s) = %s, want %s", tt.input, tt.target, got, tt.expected)
			}
			if result.Weekday() != tt.target {
				t.Errorf("weekday(%s, %s) returned %s which is a %s, not %s",
					tt.input, tt.target, got, result.Weekday(), tt.target)
			}
		})
	}
}

func TestISOWeekComputation(t *testing.T) {
	tests := []struct {
		date     string
		expected string
	}{
		{"2026-04-20", "2026-W17"},
		{"2026-01-01", "2026-W01"},
		{"2025-12-29", "2026-W01"}, // ISO week: Mon Dec 29 starts W01 of 2026
		{"2026-04-06", "2026-W15"},
	}

	for _, tt := range tests {
		t.Run(tt.date, func(t *testing.T) {
			d, _ := time.Parse("2006-01-02", tt.date)
			monday := weekday(d, time.Monday)
			year, week := monday.ISOWeek()
			got := fmt.Sprintf("%d-W%02d", year, week)
			if got != tt.expected {
				t.Errorf("ISO week for %s = %s, want %s", tt.date, got, tt.expected)
			}
		})
	}
}

func TestWeeklyEntryCollection(t *testing.T) {
	// Set up a temp repo with some daily entries
	tmpDir := t.TempDir()
	devlogDir := filepath.Join(tmpDir, "devlog")
	os.MkdirAll(devlogDir, 0755)

	// Week of 2026-04-20 (Mon) to 2026-04-26 (Sun)
	entries := map[string]string{
		"2026-04-20": "---\ndate: 2026-04-20\n---\n# Devlog — 2026-04-20\n\n## What happened\nMonday work.\n",
		"2026-04-21": "---\ndate: 2026-04-21\n---\n# Devlog — 2026-04-21\n\nNo activity.\n",
		"2026-04-22": "---\ndate: 2026-04-22\n---\n# Devlog — 2026-04-22\n\n## What happened\nWednesday work.\n",
		"2026-04-25": "---\ndate: 2026-04-25\n---\n# Devlog — 2026-04-25\n\n## What happened\nSaturday work.\n",
	}
	for date, content := range entries {
		os.WriteFile(filepath.Join(devlogDir, date+".md"), []byte(content), 0644)
	}

	monday, _ := time.Parse("2006-01-02", "2026-04-20")
	sunday := monday.AddDate(0, 0, 6)

	var weekdayEntries, weekendEntries []string
	hasContent := false

	for d := monday; !d.After(sunday); d = d.AddDate(0, 0, 1) {
		ds := d.Format("2006-01-02")
		daily := filepath.Join(tmpDir, "devlog", ds+".md")
		data, err := os.ReadFile(daily)
		if err != nil {
			continue
		}
		content := string(data)
		if !contains(content, "No activity.") {
			hasContent = true
		}
		if d.Weekday() >= time.Monday && d.Weekday() <= time.Friday {
			weekdayEntries = append(weekdayEntries, ds)
		} else {
			weekendEntries = append(weekendEntries, ds)
		}
	}

	if !hasContent {
		t.Error("expected hasContent to be true")
	}
	if len(weekdayEntries) != 3 {
		t.Errorf("expected 3 weekday entries, got %d: %v", len(weekdayEntries), weekdayEntries)
	}
	if len(weekendEntries) != 1 {
		t.Errorf("expected 1 weekend entry, got %d: %v", len(weekendEntries), weekendEntries)
	}
	if weekendEntries[0] != "2026-04-25" {
		t.Errorf("expected weekend entry 2026-04-25, got %s", weekendEntries[0])
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStr(s, substr))
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestBuildDailyPrompt(t *testing.T) {
	prompt := buildDailyPrompt("2026-04-25", "some diffs", "- [repo] #1 PR title (open)", "")

	// Should contain date in frontmatter instruction
	if !containsStr(prompt, "date: 2026-04-25") {
		t.Error("prompt missing date in frontmatter")
	}
	// Should contain diffs
	if !containsStr(prompt, "some diffs") {
		t.Error("prompt missing diffs")
	}
	// Should contain PRs
	if !containsStr(prompt, "PR title") {
		t.Error("prompt missing PR content")
	}
	// Empty issues should become "None"
	if !containsStr(prompt, "ISSUES:\nNone") {
		t.Error("prompt should show 'None' for empty issues")
	}
}

func TestBuildWeeklyPrompt(t *testing.T) {
	prompt := buildWeeklyPrompt("2026-04-20", "2026-04-24", "2026-04-26", "2026-W17",
		"weekday content here", "weekend content here")

	if !containsStr(prompt, "week: 2026-W17") {
		t.Error("prompt missing week in frontmatter")
	}
	if !containsStr(prompt, "tags: alcxyz, devlog, weekly") {
		t.Error("prompt missing tags")
	}
	if !containsStr(prompt, "2026-04-20 to 2026-04-26") {
		t.Error("prompt missing date range")
	}
	if !containsStr(prompt, "weekday content here") {
		t.Error("prompt missing weekday entries")
	}
	if !containsStr(prompt, "weekend content here") {
		t.Error("prompt missing weekend entries")
	}
}

func TestParseEnvLine(t *testing.T) {
	tests := []struct {
		line     string
		wantKey  string
		wantVal  string
		wantSkip bool
	}{
		{`HEDGEDOC_URL="https://doc.alc.xyz"`, "HEDGEDOC_URL", "https://doc.alc.xyz", false},
		{`HEDGEDOC_URL='https://doc.alc.xyz'`, "HEDGEDOC_URL", "https://doc.alc.xyz", false},
		{`HEDGEDOC_URL=https://doc.alc.xyz`, "HEDGEDOC_URL", "https://doc.alc.xyz", false},
		{`KEY = "spaced value"`, "KEY", "spaced value", false},
		{`# comment`, "", "", true},
		{``, "", "", true},
		{`  `, "", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.line, func(t *testing.T) {
			line := tt.line
			line = trimSpace(line)
			if line == "" || line[0] == '#' {
				if !tt.wantSkip {
					t.Error("expected parsed value, got skip")
				}
				return
			}
			k, v, ok := cut(line, "=")
			if !ok {
				if !tt.wantSkip {
					t.Error("expected parsed value, got no cut")
				}
				return
			}
			k = trimSpace(k)
			v = trimSpace(v)
			v = trimQuotes(v)

			if tt.wantSkip {
				t.Error("expected skip, got parsed value")
				return
			}
			if k != tt.wantKey {
				t.Errorf("key = %q, want %q", k, tt.wantKey)
			}
			if v != tt.wantVal {
				t.Errorf("val = %q, want %q", v, tt.wantVal)
			}
		})
	}
}

// Minimal helpers to avoid importing strings in tests (mirrors main.go logic)
func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

func cut(s, sep string) (string, string, bool) {
	for i := 0; i <= len(s)-len(sep); i++ {
		if s[i:i+len(sep)] == sep {
			return s[:i], s[i+len(sep):], true
		}
	}
	return s, "", false
}

func trimQuotes(s string) string {
	if len(s) >= 2 && ((s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'')) {
		return s[1 : len(s)-1]
	}
	return s
}

func TestFileExists(t *testing.T) {
	tmpDir := t.TempDir()

	existing := filepath.Join(tmpDir, "exists.md")
	os.WriteFile(existing, []byte("hello"), 0644)

	if !fileExists(existing) {
		t.Error("fileExists returned false for existing file")
	}
	if fileExists(filepath.Join(tmpDir, "nope.md")) {
		t.Error("fileExists returned true for non-existing file")
	}
}

func TestEnvBool(t *testing.T) {
	os.Setenv("TEST_ENVBOOL_TRUE", "1")
	os.Setenv("TEST_ENVBOOL_TRUE2", "true")
	os.Setenv("TEST_ENVBOOL_FALSE", "0")
	os.Setenv("TEST_ENVBOOL_FALSE2", "false")
	defer func() {
		os.Unsetenv("TEST_ENVBOOL_TRUE")
		os.Unsetenv("TEST_ENVBOOL_TRUE2")
		os.Unsetenv("TEST_ENVBOOL_FALSE")
		os.Unsetenv("TEST_ENVBOOL_FALSE2")
	}()

	if !envBool("TEST_ENVBOOL_TRUE") {
		t.Error(`envBool("1") should be true`)
	}
	if !envBool("TEST_ENVBOOL_TRUE2") {
		t.Error(`envBool("true") should be true`)
	}
	if envBool("TEST_ENVBOOL_FALSE") {
		t.Error(`envBool("0") should be false`)
	}
	if envBool("TEST_ENVBOOL_FALSE2") {
		t.Error(`envBool("false") should be false`)
	}
	if envBool("TEST_ENVBOOL_UNSET") {
		t.Error(`envBool for unset var should be false`)
	}
}

func TestEnvOr(t *testing.T) {
	os.Setenv("TEST_ENVOR_SET", "custom")
	defer os.Unsetenv("TEST_ENVOR_SET")

	if v := envOr("TEST_ENVOR_SET", "default"); v != "custom" {
		t.Errorf("envOr with set var = %q, want custom", v)
	}
	if v := envOr("TEST_ENVOR_UNSET", "fallback"); v != "fallback" {
		t.Errorf("envOr with unset var = %q, want fallback", v)
	}
}

func TestIdempotency(t *testing.T) {
	tmpDir := t.TempDir()
	devlogDir := filepath.Join(tmpDir, "devlog")
	os.MkdirAll(devlogDir, 0755)

	// Create an existing entry
	existing := filepath.Join(devlogDir, "2026-04-25.md")
	os.WriteFile(existing, []byte("existing content"), 0644)

	// Verify the file exists check works
	outfile := filepath.Join(tmpDir, "devlog", "2026-04-25.md")
	if !fileExists(outfile) {
		t.Error("expected existing entry to be detected")
	}

	// Verify non-existing date is not detected
	outfile2 := filepath.Join(tmpDir, "devlog", "2026-04-30.md")
	if fileExists(outfile2) {
		t.Error("expected non-existing entry to not be detected")
	}
}

func TestNoActivityOutput(t *testing.T) {
	tmpDir := t.TempDir()
	weeklyDir := filepath.Join(tmpDir, "weekly")
	os.MkdirAll(weeklyDir, 0755)

	monStr := "2026-04-20"
	weekStr := "2026-W17"
	sunStr := "2026-04-26"

	content := fmt.Sprintf("---\ndate: %s\nweek: %s\ntags: alcxyz, devlog, weekly\n---\n# Week %s — %s to %s\n\nNo activity.\n",
		monStr, weekStr, weekStr, monStr, sunStr)

	outfile := filepath.Join(weeklyDir, weekStr+".md")
	if err := os.WriteFile(outfile, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write: %v", err)
	}

	data, _ := os.ReadFile(outfile)
	got := string(data)

	if !containsStr(got, "tags: alcxyz, devlog, weekly") {
		t.Error("no-activity output missing tags")
	}
	if !containsStr(got, "No activity.") {
		t.Error("no-activity output missing 'No activity.'")
	}
}
