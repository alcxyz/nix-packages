package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReportedCommitsSkipsEarlierEntries(t *testing.T) {
	repo := t.TempDir()
	os.MkdirAll(filepath.Join(repo, "devlog"), 0755)
	earlier := filepath.Join(repo, "devlog", "2026-10-01.md")
	os.WriteFile(earlier, []byte("---\ndate: 2026-10-01\n---\n# Devlog\n\nWork.\n"), 0644)
	if err := appendReported(earlier, []commitRef{{SHA: "aaa"}, {SHA: "bbb"}}); err != nil {
		t.Fatal(err)
	}
	// Outside the lookback, so not recognised.
	old := filepath.Join(repo, "devlog", "2026-08-01.md")
	os.WriteFile(old, []byte("x\n"+reportedLine([]commitRef{{SHA: "ccc"}})), 0644)

	reported := reportedCommits(repo, time.Date(2026, 10, 3, 0, 0, 0, 0, time.Local))
	fresh := withoutReported([]commitRef{{SHA: "aaa"}, {SHA: "ccc"}, {SHA: "ddd"}}, reported)

	if len(fresh) != 2 || fresh[0].SHA != "ccc" || fresh[1].SHA != "ddd" {
		t.Errorf("fresh = %+v", fresh)
	}
}

func TestStripReported(t *testing.T) {
	entry := "# Devlog\n\nWork.\n\n" + reportedLine([]commitRef{{SHA: "aaa"}})
	got := stripReported(entry)
	if strings.Contains(got, "devlog-commits") || !strings.HasSuffix(got, "Work.\n") {
		t.Errorf("stripReported = %q", got)
	}
}

func TestReportedCommitsGapFill(t *testing.T) {
	repo := t.TempDir()
	dir := filepath.Join(repo, "devlog")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	// Entries already written keep their commits, even when the gap is filled
	// much later. The old backward window stays inclusive at 30 days.
	entries := map[string]string{
		"2026-09-02.md":  "too-old",
		"2026-09-03.md":  "boundary",
		"2026-10-02.md":  "earlier",
		"2026-10-03.md":  "same-day",
		"2026-10-04.md":  "later",
		"2027-01-01.md":  "much-later",
		"2026-W41.md":    "weekly",
		"2026-10-05.txt": "text",
		"2026-11-31.md":  "invalid-date",
	}
	for name, sha := range entries {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("Work.\n"+reportedLine([]commitRef{{SHA: sha}})), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "2027-01-02.md"), 0755); err != nil {
		t.Fatal(err)
	}
	date, _ := time.Parse("2006-01-02", "2026-10-03")
	reported := reportedCommits(repo, date)
	want := map[string]bool{"boundary": true, "earlier": true, "later": true, "much-later": true}
	if len(reported) != len(want) {
		t.Errorf("reported = %v, want %v", reported, want)
	}
	for sha := range want {
		if !reported[sha] {
			t.Errorf("missing reported commit %s", sha)
		}
	}
	fresh := withoutReported([]commitRef{{SHA: "earlier"}, {SHA: "later"}, {SHA: "much-later"}, {SHA: "new"}}, reported)
	if len(fresh) != 1 || fresh[0].SHA != "new" {
		t.Errorf("gap-fill commits = %+v, want only new", fresh)
	}
}

func TestWeeklyMissesDaily(t *testing.T) {
	repo := t.TempDir()
	os.MkdirAll(filepath.Join(repo, "devlog"), 0755)
	os.MkdirAll(filepath.Join(repo, "weekly"), 0755)
	monday := time.Date(2026, 9, 28, 0, 0, 0, 0, time.Local)
	weekly := filepath.Join(repo, "weekly", isoWeekString(monday)+".md")
	daily := func(ds, body string) {
		os.WriteFile(filepath.Join(repo, "devlog", ds+".md"), []byte("# Devlog — "+ds+"\n\n"+body+"\n"), 0644)
	}

	if weeklyMissesDaily(repo, monday) {
		t.Error("no dailies: nothing to refresh")
	}
	daily("2026-09-28", "No activity.")
	os.WriteFile(weekly, []byte("# Week\n\nNo activity.\n"), 0644)
	if weeklyMissesDaily(repo, monday) {
		t.Error("an all-quiet stub weekly is complete")
	}

	daily("2026-09-29", "Work.")
	daily("2026-09-30", "More work.")
	// Tuesday's window line names Wednesday's date, which must not count as
	// Wednesday's entry.
	os.WriteFile(weekly, []byte("# Week\n\n"+weeklyDayMarker("2026-09-29")+"\nwindow: 2026-09-29 05:00 to 2026-09-30 04:59\n# Devlog — 2026-09-29\n"), 0644)
	if !weeklyMissesDaily(repo, monday) {
		t.Error("weekly lacks 2026-09-30")
	}
	os.WriteFile(weekly, []byte(weeklyDayMarker("2026-09-29")+"\n"+weeklyDayMarker("2026-09-30")+"\n"), 0644)
	if weeklyMissesDaily(repo, monday) {
		t.Error("weekly has a marker for every daily with activity")
	}

	// A summary written before the markers is matched by title.
	os.WriteFile(weekly, []byte("# Devlog — 2026-09-29\n# Devlog — 2026-09-30\n"), 0644)
	if weeklyMissesDaily(repo, monday) {
		t.Error("an unmarked weekly with every title is complete")
	}

	os.Remove(weekly)
	if !weeklyMissesDaily(repo, monday) {
		t.Error("a missing weekly with activity needs generating")
	}
}
