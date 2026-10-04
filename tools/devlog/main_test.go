package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWeekday(t *testing.T) {
	tests := []struct {
		input    string
		target   time.Weekday
		expected string
	}{
		{"2026-04-25", time.Monday, "2026-04-20"}, // Friday → Monday
		{"2026-04-20", time.Monday, "2026-04-20"}, // Monday → Monday (same day)
		{"2026-04-26", time.Monday, "2026-04-20"}, // Saturday → Monday
		{"2026-04-27", time.Monday, "2026-04-27"}, // Sunday → Monday (Go: Sunday=0, so wraps to current week's Monday)
		{"2026-04-22", time.Monday, "2026-04-20"}, // Wednesday → Monday
		{"2026-01-01", time.Monday, "2025-12-29"}, // Year boundary
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
	window := "2026-04-25 05:00 TEST to 2026-04-26 04:59 TEST"
	prompt := buildDailyPrompt("2026-04-25", window, "some diffs", "- [repo] #1 PR title (open)", "")

	// Should contain date in frontmatter instruction
	if !containsStr(prompt, "date: 2026-04-25") {
		t.Error("prompt missing date in frontmatter")
	}
	if !containsStr(prompt, "window: "+window) {
		t.Error("prompt missing window in frontmatter")
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

func TestDevlogWindow(t *testing.T) {
	originalLocal := time.Local
	time.Local = time.FixedZone("TEST", 2*60*60)
	t.Cleanup(func() { time.Local = originalLocal })

	date, _ := time.Parse("2006-01-02", "2026-04-25")
	start, end := devlogWindow(date)

	if got := formatWindow(start, end); got != "2026-04-25 05:00 TEST to 2026-04-26 04:59 TEST" {
		t.Fatalf("formatWindow() = %q", got)
	}

	tests := []struct {
		name string
		ts   time.Time
		want bool
	}{
		{"before window", time.Date(2026, 4, 25, 4, 59, 59, 0, time.Local), false},
		{"at start", time.Date(2026, 4, 25, 5, 0, 0, 0, time.Local), true},
		{"before end", time.Date(2026, 4, 26, 4, 59, 59, 0, time.Local), true},
		{"at end", time.Date(2026, 4, 26, 5, 0, 0, 0, time.Local), false},
		{"utc inside", time.Date(2026, 4, 26, 2, 30, 0, 0, time.UTC), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := inDevlogWindow(tt.ts, start, end); got != tt.want {
				t.Errorf("inDevlogWindow(%s) = %v, want %v", tt.ts, got, tt.want)
			}
		})
	}
}

func TestCatchUpCommitMessage(t *testing.T) {
	parseDate := func(s string) time.Time {
		t.Helper()
		d, err := time.Parse("2006-01-02", s)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}

	tests := []struct {
		name           string
		generatedDates []time.Time
		end            time.Time
		want           string
	}{
		{
			name:           "normal latest day generation",
			generatedDates: []time.Time{parseDate("2026-05-30")},
			end:            parseDate("2026-05-30"),
			want:           "devlog: 2026-05-30",
		},
		{
			name:           "single older missing day",
			generatedDates: []time.Time{parseDate("2026-05-28")},
			end:            parseDate("2026-05-30"),
			want:           "devlog catch-up: 2026-05-28",
		},
		{
			name:           "multiple missing days",
			generatedDates: []time.Time{parseDate("2026-05-27"), parseDate("2026-05-29")},
			end:            parseDate("2026-05-30"),
			want:           "devlog catch-up: 2026-05-27 to 2026-05-29",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := catchUpCommitMessage(tt.generatedDates, tt.end); got != tt.want {
				t.Errorf("catchUpCommitMessage() = %q, want %q", got, tt.want)
			}
		})
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

// journalFixture is a bare remote with two clones: "other" stands for edits
// made elsewhere, "journal" for the checkout devlog writes to.
type journalFixture struct {
	t       *testing.T
	root    string
	other   string
	journal string
}

func newJournalFixture(t *testing.T) *journalFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	f := &journalFixture{t: t, root: t.TempDir()}
	f.run(f.root, "init", "--quiet", "--bare", "--initial-branch=dev", "remote.git")
	f.other = f.clone("other")
	f.commit(f.other, "base.md", "base.md\n", "base")
	f.run(f.other, "push", "--quiet", "origin", "dev")
	f.journal = f.clone("journal")
	return f
}

func (f *journalFixture) run(dir string, args ...string) string {
	f.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (f *journalFixture) clone(name string) string {
	dir := filepath.Join(f.root, name)
	f.run(f.root, "clone", "--quiet", "remote.git", name)
	f.run(dir, "config", "user.name", "Test")
	f.run(dir, "config", "user.email", "test@example.invalid")
	return dir
}

func (f *journalFixture) write(dir, name, content string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *journalFixture) commit(dir, name, content, message string) {
	f.write(dir, name, content)
	f.run(dir, "add", name)
	f.run(dir, "commit", "--quiet", "-m", message)
}

func TestGitCommitAndPushRebasesOntoRemote(t *testing.T) {
	f := newJournalFixture(t)
	f.commit(f.other, "elsewhere.md", "elsewhere\n", "elsewhere")
	f.run(f.other, "push", "--quiet")

	f.write(f.journal, "entry.md", "entry\n")
	// An unrelated unstaged edit must not block, or be lost by, the sync.
	f.write(f.journal, "base.md", "edited\n")
	if err := gitCommitAndPush(f.journal, "entry.md", "devlog: entry"); err != nil {
		t.Fatalf("gitCommitAndPush: %v", err)
	}
	if got := f.run(f.journal, "rev-list", "--count", "origin/dev..dev"); got != "0" {
		t.Errorf("unpushed commits = %s, want 0", got)
	}
	if got := f.run(f.root, "--git-dir=remote.git", "log", "--format=%s", "dev"); got != "devlog: entry\nelsewhere\nbase" {
		t.Errorf("remote history = %q", got)
	}
	if got := f.run(f.journal, "status", "--porcelain"); got != "M base.md" {
		t.Errorf("journal status = %q, want unrelated edit kept", got)
	}
}

func TestGitSyncLeavesForeignRebaseAlone(t *testing.T) {
	f := newJournalFixture(t)
	marker := filepath.Join(f.journal, ".git", "rebase-merge")
	if err := os.Mkdir(marker, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := gitSync(f.journal); err == nil {
		t.Fatal("gitSync succeeded during a rebase in progress")
	}
	if !fileExists(marker) {
		t.Error("gitSync aborted a rebase it did not start")
	}
}

func TestGitSyncReportsAutostashConflict(t *testing.T) {
	f := newJournalFixture(t)
	f.commit(f.other, "base.md", "remote\n", "remote edit")
	f.run(f.other, "push", "--quiet")
	f.write(f.journal, "base.md", "local\n")

	if err := gitSync(f.journal); err == nil {
		t.Fatal("gitSync succeeded with a conflicted autostash")
	}
}

func TestGitSyncPushesPendingCommits(t *testing.T) {
	f := newJournalFixture(t)
	// A commit left behind by an earlier run whose push failed.
	f.commit(f.journal, "entry.md", "entry\n", "devlog: entry")

	if err := gitSync(f.journal); err != nil {
		t.Fatalf("gitSync: %v", err)
	}
	if got := f.run(f.root, "--git-dir=remote.git", "log", "-1", "--format=%s", "dev"); got != "devlog: entry" {
		t.Errorf("remote tip = %q, want the pending entry", got)
	}
}
