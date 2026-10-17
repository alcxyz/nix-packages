package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"revkit/review"
)

// withMatcher adds a matcher reviewer to the fixture's configuration.
func withMatcher(t *testing.T, f reviewFixture) {
	t.Helper()
	content, err := os.ReadFile(f.config)
	if err != nil {
		t.Fatal(err)
	}
	updated := strings.Replace(string(content), `"timeoutSeconds"`,
		`"matcher": {"name": "matcher", "client": "claude", "model": "haiku", "effort": "low"}, "timeoutSeconds"`, 1)
	if err := os.WriteFile(f.config, []byte(updated), 0o644); err != nil {
		t.Fatal(err)
	}
}

// runs returns every run's results, oldest first.
func (f reviewFixture) runs(t *testing.T) []runReport {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(f.state, "repo-review", "example", "*", "runs", "*", "results.json"))
	sort.Strings(matches)
	var reports []runReport
	for _, match := range matches {
		content, err := os.ReadFile(match)
		if err != nil {
			t.Fatal(err)
		}
		var report runReport
		if err := json.Unmarshal(content, &report); err != nil {
			t.Fatal(err)
		}
		reports = append(reports, report)
	}
	return reports
}

func (f reviewFixture) store(t *testing.T) *findingStore {
	t.Helper()
	s, err := readStore(storePath(f.state, "example"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (f reviewFixture) findings(t *testing.T, args ...string) (string, int) {
	t.Helper()
	var out bytes.Buffer
	code := findingsCommand(append(args, "--config", f.config, "--repository", "example"), &out)
	return out.String(), code
}

func TestRepeatRunsKnowTheirFindings(t *testing.T) {
	f := newReviewFixture(t, "")
	withMatcher(t, f)
	review := func() {
		t.Helper()
		// Run directories are named by the second they start in.
		time.Sleep(1100 * time.Millisecond)
		if code := repoReview(context.Background(), []string{"--config", f.config, f.repo}); code != 0 {
			t.Fatalf("exit %d", code)
		}
	}
	review()
	s := f.store(t)
	// Both reviewers reported the same problem: one stored finding.
	if len(s.Findings) != 1 || s.Findings[0].ID != "F1" || s.Findings[0].Status != storeOpen ||
		len(s.Findings[0].Reviewers) != 2 || s.Findings[0].Sightings != 1 {
		t.Fatalf("store = %+v", s.Findings)
	}
	first := f.runs(t)[0]
	for _, finding := range first.Findings {
		if finding.StoreID != "F1" || finding.StoreStatus != "new" {
			t.Fatalf("finding = %+v", finding)
		}
	}

	review()
	s = f.store(t)
	if len(s.Findings) != 1 || s.Findings[0].Sightings != 2 {
		t.Fatalf("store after a repeat = %+v", s.Findings)
	}
	second := f.runs(t)[1]
	for _, finding := range second.Findings {
		if finding.StoreID != "F1" || finding.StoreStatus != storeOpen {
			t.Fatalf("repeated finding = %+v", finding)
		}
	}

	if _, code := f.findings(t, "dismiss", "F1"); code != 2 {
		t.Fatal("dismissed without a reason")
	}
	if out, code := f.findings(t, "dismiss", "--reason", "intended", "F1"); code != 0 {
		t.Fatalf("dismiss: %d %s", code, out)
	}
	review()
	s = f.store(t)
	if s.Findings[0].Status != storeDismissed || s.Findings[0].Note != "intended" || len(s.Findings) != 1 {
		t.Fatalf("store after dismissal = %+v", s.Findings)
	}
	if out, _ := f.findings(t, "list"); !strings.Contains(out, "no findings") {
		t.Fatalf("list shows a dismissed finding:\n%s", out)
	}
	if out, _ := f.findings(t, "list", "--all"); !strings.Contains(out, "F1") || !strings.Contains(out, "dismissed") {
		t.Fatalf("list --all:\n%s", out)
	}
	if _, code := f.findings(t, "reopen", "F1"); code != 0 {
		t.Fatal("reopen failed")
	}
	if out, _ := f.findings(t, "show", "F1"); !strings.Contains(out, "F1 Stale") || !strings.Contains(out, "seen 3 times") {
		t.Fatalf("show:\n%s", out)
	}
	if _, code := f.findings(t, "show", "F9"); code != 2 {
		t.Fatal("showed a missing finding")
	}
}

func TestWithoutMatcherFindingsMatchByTitle(t *testing.T) {
	f := newReviewFixture(t, "")
	if code := repoReview(context.Background(), []string{"--config", f.config, f.repo}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	// The fake reviewers word their titles differently: two problems.
	if s := f.store(t); len(s.Findings) != 2 {
		t.Fatalf("store = %+v", s.Findings)
	}
	report := f.results(t)
	for _, job := range report.Jobs {
		if job.Kind == "match" {
			t.Fatalf("a matcher ran without one configured: %+v", job)
		}
	}
	if !strings.Contains(report.Matching, "no matcher") {
		t.Fatalf("matching = %q", report.Matching)
	}
}

func TestFailedMatcherFallsBackToTitles(t *testing.T) {
	f := newReviewFixture(t, "")
	withMatcher(t, f)
	// The matcher is a Claude reviewer too; break only its reply.
	content, _ := os.ReadFile(f.config)
	os.WriteFile(f.config, []byte(strings.Replace(string(content), `"model": "haiku"`, `"model": "missing-model"`, 1)), 0o644)
	bin := t.TempDir()
	self, _ := os.Executable()
	script := fmt.Sprintf("#!/bin/sh\ncase \"$*\" in *missing-model*) exit 1;; esac\nREVKIT_FAKE_CLIENT=claude exec %q \"$@\"\n", self)
	os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0o755)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	if code := repoReview(context.Background(), []string{"--config", f.config, f.repo}); code != 0 {
		t.Fatalf("a failed matcher failed the run: exit %d", code)
	}
	report := f.results(t)
	var match *jobRecord
	for i := range report.Jobs {
		if report.Jobs[i].Kind == "match" {
			match = &report.Jobs[i]
		}
	}
	if match == nil || match.Status == "ok" || !strings.Contains(report.Matching, "the matcher failed") {
		t.Fatalf("match job = %+v, matching %q", match, report.Matching)
	}
	if s := f.store(t); len(s.Findings) != 2 {
		t.Fatalf("store = %+v", s.Findings)
	}
}

func TestResumeReusesCompletedJobs(t *testing.T) {
	failing := filepath.Join(t.TempDir(), "failing")
	os.WriteFile(failing, []byte("codex"), 0o644)
	f := newReviewFixture(t, "REVKIT_FAKE_FAIL_FILE="+failing)
	if code := repoReview(context.Background(), []string{"--config", f.config, f.repo}); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	// Codex recovers: only its find, and the verifications, run again.
	os.Remove(failing)
	time.Sleep(1100 * time.Millisecond)
	if code := repoReview(context.Background(), []string{"--config", f.config, "--resume", f.repo}); code != 0 {
		t.Fatalf("resumed run: exit %d", code)
	}
	runs := f.runs(t)
	if len(runs) != 2 {
		t.Fatalf("runs = %d", len(runs))
	}
	reused := map[string]string{}
	for _, job := range runs[1].Jobs {
		reused[job.Kind+" "+job.Reviewer] = job.ReusedFrom
	}
	if reused["find opus"] == "" || reused["find gpt"] != "" || reused["verify gpt"] != "" {
		t.Fatalf("reuse = %v", reused)
	}
	if !runs[1].Complete || len(runs[1].Findings) != 2 || runs[1].Review != runs[0].Review {
		t.Fatalf("resumed run = %+v", runs[1])
	}
	// One review completed in two parts: its problems are new and seen once.
	for _, finding := range runs[1].Findings {
		if finding.StoreStatus != "new" {
			t.Fatalf("resumed finding = %+v", finding)
		}
	}
	for _, sf := range f.store(t).Findings {
		if sf.Sightings != 1 {
			t.Fatalf("stored = %+v", sf)
		}
	}
	for _, finding := range runs[1].Findings {
		if finding.Verdict != "confirmed" {
			t.Fatalf("finding = %+v", finding)
		}
	}
	// A third resume reuses everything, verifications included.
	time.Sleep(1100 * time.Millisecond)
	if code := repoReview(context.Background(), []string{"--config", f.config, "--resume", f.repo}); code != 0 {
		t.Fatalf("second resume: exit %d", code)
	}
	for _, job := range f.runs(t)[2].Jobs {
		if job.ReusedFrom == "" {
			t.Fatalf("job ran again: %+v", job)
		}
	}
}

func TestStoreKeepsConcurrentUpdates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "findings.json")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := withStore(path, func(s *findingStore) error {
				s.add(finding{Lens: "l", Title: fmt.Sprint(i), Reviewer: "r", Verdict: "confirmed"}, sighting{Run: fmt.Sprint(i)})
				return nil
			})
			if err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	s, err := readStore(path)
	if err != nil || len(s.Findings) != 8 || s.NextID != 9 {
		t.Fatalf("store = %+v, %v", s, err)
	}
}

func TestParseMatches(t *testing.T) {
	findings := []finding{{ID: "a.l.1"}, {ID: "b.l.1"}, {ID: "b.l.2"}}
	known := []storedFinding{{ID: "F4"}}
	targets, problem := parseMatches(`{"matches": [
		{"id": "a.l.1", "same": "b.l.1"},
		{"id": "b.l.1", "same": "f4"},
		{"id": "b.l.2", "same": "a.l.1"}]}`, findings, known)
	if problem != "" {
		t.Fatal(problem)
	}
	// A forward reference is no match; known IDs ignore case.
	want := map[string]string{"b.l.1": "F4", "b.l.2": "a.l.1"}
	if fmt.Sprint(targets) != fmt.Sprint(want) {
		t.Fatalf("targets = %v, want %v", targets, want)
	}
	if _, problem := parseMatches(`{"matches": [{"id": "a.l.1", "same": null}]}`, findings, known); problem == "" {
		t.Fatal("accepted a reply without every finding")
	}
}

// rememberRun is a run with the given findings, ready to remember them.
func rememberRun(t *testing.T, storePath, review string, findings ...finding) *reviewRun {
	t.Helper()
	return &reviewRun{cfg: &config{}, storePath: storePath, dir: filepath.Join(t.TempDir(), review), out: &bytes.Buffer{},
		report: runReport{Commit: "c0ffee", Review: review, Findings: findings}}
}

func TestTitleMatchingSeesTheWholeStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "findings.json")
	withStore(path, func(s *findingStore) error {
		s.add(finding{Lens: "l", Title: "Old problem", Reviewer: "r", Verdict: "confirmed"}, sighting{Run: "old", Time: time.Unix(1, 0)})
		s.find("F1").Status, s.find("F1").Note = storeDismissed, "intended"
		for i := 0; i < knownLimit+5; i++ {
			s.add(finding{Lens: "l", Title: fmt.Sprint("newer ", i), Reviewer: "r", Verdict: "confirmed"}, sighting{Run: "newer", Time: time.Unix(int64(100+i), 0)})
		}
		return nil
	})
	r := rememberRun(t, path, "now", finding{ID: "r.l.1", Lens: "l", Title: "old  PROBLEM", Reviewer: "r", Verdict: "confirmed"})
	if err := r.remember(context.Background(), []lens{{Name: "l"}}); err != nil {
		t.Fatal(err)
	}
	if f := r.report.Findings[0]; f.StoreID != "F1" || f.StoreStatus != storeDismissed {
		t.Fatalf("finding = %+v", f)
	}
}

func TestBestVerdictOfAReviewDescribesTheProblem(t *testing.T) {
	path := filepath.Join(t.TempDir(), "findings.json")
	r := rememberRun(t, path, "20261017T010000Z-1",
		finding{ID: "a.l.1", Lens: "l", Title: "Same", Claim: "confirmed wording", Reviewer: "a", Verdict: "confirmed"},
		finding{ID: "b.m.1", Lens: "m", Title: "same", Claim: "rejected wording", Reviewer: "b", Verdict: "rejected"})
	if err := r.remember(context.Background(), []lens{{Name: "l"}, {Name: "m"}}); err != nil {
		t.Fatal(err)
	}
	s, _ := readStore(path)
	// Titles match only within a lens, so these are two problems; join the
	// second into the first the way the matcher would and remember again.
	if len(s.Findings) != 2 {
		t.Fatalf("store = %+v", s.Findings)
	}
	sf := s.Findings[0]
	sf.sight(finding{Lens: "m", Title: "Same", Claim: "rejected wording", Reviewer: "b", Verdict: "rejected"}, sf.LastSeen)
	if sf.Verdict != "confirmed" || sf.Claim != "confirmed wording" || sf.Sightings != 1 || len(sf.lensSet()) != 2 {
		t.Fatalf("problem = %+v", sf)
	}
	later := sighting{Run: "20261017T020000Z-2"}
	sf.sight(finding{Lens: "l", Title: "Same", Claim: "now rejected", Reviewer: "a", Verdict: "rejected"}, later)
	if sf.Verdict != "rejected" || sf.Sightings != 2 {
		t.Fatalf("a later review did not replace the description: %+v", sf)
	}
}

func TestStoreFailureKeepsReportIDsOut(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	os.WriteFile(blocker, []byte("x"), 0o644)
	r := rememberRun(t, filepath.Join(blocker, "findings.json"), "review",
		finding{ID: "a.l.1", Lens: "l", Title: "x", Reviewer: "a", Verdict: "confirmed"})
	if err := r.remember(context.Background(), []lens{{Name: "l"}}); err == nil {
		t.Fatal("a store under a file was written")
	}
	if r.report.Findings[0].StoreID != "" {
		t.Fatalf("an unsaved ID reached the report: %+v", r.report.Findings[0])
	}
}

func TestResumeKeepsTheReplyOrder(t *testing.T) {
	reviewer := review.Reviewer{Name: "a", Client: "claude", Model: "m", Effort: "high"}
	previous := &runReport{
		Jobs: []jobRecord{{Kind: "find", Reviewer: "a", Client: "claude", Model: "m", Effort: "high", Lens: "l", Findings: 3,
			PromptSHA: "sha", Result: review.Result{Status: review.StatusOK}}},
		// Sorted by verdict, as write() leaves them.
		Findings: []finding{{ID: "a.l.3", Reviewer: "a", Lens: "l"}, {ID: "a.l.10", Reviewer: "a", Lens: "l"}, {ID: "a.l.2", Reviewer: "a", Lens: "l"}},
	}
	r := &reviewRun{previous: previous, previousName: "earlier"}
	_, findings, ok := r.reuseFind(reviewer, "l", "sha")
	if !ok || findings[0].ID != "a.l.2" || findings[1].ID != "a.l.3" || findings[2].ID != "a.l.10" {
		t.Fatalf("reused = %v %+v", ok, findings)
	}
}

func TestMatcherNonMatchesStandAgainstTitles(t *testing.T) {
	matching := filepath.Join(t.TempDir(), "matching")
	f := newReviewFixture(t, "REVKIT_FAKE_MATCH_FILE="+matching)
	withMatcher(t, f)
	if code := repoReview(context.Background(), []string{"--config", f.config, f.repo}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if _, code := f.findings(t, "dismiss", "F1", "--reason", "intended"); code != 0 {
		t.Fatal("dismiss failed")
	}
	// The matcher now says every finding is a new problem, although one
	// keeps the dismissed problem's exact title: its judgement stands.
	os.WriteFile(matching, []byte("none"), 0o644)
	time.Sleep(1100 * time.Millisecond)
	if code := repoReview(context.Background(), []string{"--config", f.config, f.repo}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	s := f.store(t)
	if f1 := s.find("F1"); f1.Status != storeDismissed || f1.Sightings != 1 {
		t.Fatalf("F1 = %+v", f1)
	}
	if len(s.Findings) != 3 {
		t.Fatalf("store = %+v", s.Findings)
	}
}

func TestResumeReportsADismissalMadeMeanwhile(t *testing.T) {
	failing := filepath.Join(t.TempDir(), "failing")
	os.WriteFile(failing, []byte("codex"), 0o644)
	f := newReviewFixture(t, "REVKIT_FAKE_FAIL_FILE="+failing)
	if code := repoReview(context.Background(), []string{"--config", f.config, f.repo}); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if _, code := f.findings(t, "dismiss", "F1", "--reason", "known"); code != 0 {
		t.Fatal("dismiss failed")
	}
	os.Remove(failing)
	time.Sleep(1100 * time.Millisecond)
	if code := repoReview(context.Background(), []string{"--config", f.config, "--resume", f.repo}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, finding := range f.runs(t)[1].Findings {
		if finding.StoreID == "F1" && finding.StoreStatus != storeDismissed {
			t.Fatalf("finding = %+v", finding)
		}
	}
}

func TestSightingsCountReviews(t *testing.T) {
	var sf storedFinding
	sf.sight(finding{Lens: "l", Title: "x", Reviewer: "a", Verdict: "unverified"}, sighting{Run: "r1"})
	sf.sight(finding{Lens: "l", Title: "x", Reviewer: "a", Verdict: "confirmed"}, sighting{Run: "r2"})
	// r1 resumes after r2: the same review, so no new sighting, and its
	// verification beats the unverified report it made before.
	sf.sight(finding{Lens: "l", Title: "x", Reviewer: "a", Verdict: "rejected"}, sighting{Run: "r1"})
	if sf.Sightings != 2 {
		t.Fatalf("sightings = %d", sf.Sightings)
	}
	var unverified storedFinding
	unverified.sight(finding{Lens: "l", Title: "x", Reviewer: "a", Verdict: "unverified"}, sighting{Run: "r1"})
	unverified.sight(finding{Lens: "l", Title: "x", Reviewer: "a", Verdict: "rejected"}, sighting{Run: "r1"})
	if unverified.Verdict != "rejected" {
		t.Fatalf("verdict = %s", unverified.Verdict)
	}
}

func TestTitleTargetsAreNotSharedByFindingsTheMatcherSeparated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "findings.json")
	withStore(path, func(s *findingStore) error {
		s.add(finding{Lens: "l", Title: "Same title", Reviewer: "r", Verdict: "confirmed"}, sighting{Run: "20261016T000000Z-1"})
		s.find("F1").Status = storeDismissed
		return nil
	})
	r := rememberRun(t, path, "20261017T000000Z-1",
		finding{ID: "a.l.1", Lens: "l", Title: "Same title", Reviewer: "a", Verdict: "confirmed"},
		finding{ID: "b.l.1", Lens: "l", Title: "Same title", Reviewer: "b", Verdict: "confirmed"})
	// The matcher saw no known problem (F1 is beyond its prompt) and told
	// the two findings apart.
	targets := map[string]string{}
	r.cfg.Matcher = nil
	if err := r.store(targets, nil); err != nil {
		t.Fatal(err)
	}
	ids := []string{r.report.Findings[0].StoreID, r.report.Findings[1].StoreID}
	if ids[0] != "F1" || ids[1] == "F1" {
		t.Fatalf("store IDs = %v", ids)
	}
}

func TestResumedFindingsKeepTheirProblem(t *testing.T) {
	path := filepath.Join(t.TempDir(), "findings.json")
	withStore(path, func(s *findingStore) error {
		s.add(finding{Lens: "l", Title: "Worded once", Reviewer: "a", Verdict: "unverified"}, sighting{Run: "20261017T000000Z-1"})
		s.find("F1").Status = storeDismissed
		return nil
	})
	r := rememberRun(t, path, "20261017T000000Z-1",
		finding{ID: "a.l.1", Lens: "l", Title: "Worded once", Reviewer: "a", Verdict: "confirmed", carried: "F1"})
	// The matcher saw F1 and missed the repeat; the carried ID still holds.
	known, _ := readStore(path)
	if err := r.store(map[string]string{}, known.Findings); err != nil {
		t.Fatal(err)
	}
	if f := r.report.Findings[0]; f.StoreID != "F1" || f.StoreStatus != storeDismissed {
		t.Fatalf("finding = %+v", f)
	}
}

func TestCarriedProblemsAreClaimed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "findings.json")
	withStore(path, func(s *findingStore) error {
		s.add(finding{Lens: "l", Title: "Same title", Reviewer: "a", Verdict: "confirmed"}, sighting{Run: "20261017T000000.000000000Z-1"})
		s.find("F1").Status = storeDismissed
		return nil
	})
	r := rememberRun(t, path, "20261017T000000.000000000Z-1",
		finding{ID: "a.l.1", Lens: "l", Title: "Same title", Reviewer: "a", Verdict: "confirmed", carried: "F1"},
		finding{ID: "b.l.1", Lens: "l", Title: "Same title", Reviewer: "b", Verdict: "confirmed"})
	// The matcher, not shown F1, told the two findings apart.
	if err := r.store(map[string]string{}, nil); err != nil {
		t.Fatal(err)
	}
	if ids := []string{r.report.Findings[0].StoreID, r.report.Findings[1].StoreID}; ids[0] != "F1" || ids[1] == "F1" {
		t.Fatalf("store IDs = %v", ids)
	}
}

func TestRepliesCannotCarryStoreIDs(t *testing.T) {
	findings, _, problem := parseFindings(`{"findings": [{"title": "x", "claim": "y", "storeId": "F1", "storeStatus": "dismissed"}]}`)
	if problem != "" || findings[0].StoreID != "" || findings[0].StoreStatus != "" || findings[0].carried != "" {
		t.Fatalf("findings = %+v, %q", findings, problem)
	}
}

func TestEarlierReviewVerifiesWhatWentUnverified(t *testing.T) {
	var sf storedFinding
	sf.sight(finding{Lens: "l", Title: "x", Reviewer: "a", Verdict: "unverified"}, sighting{Run: "20261017T000000.1Z-a"})
	sf.sight(finding{Lens: "l", Title: "x", Reviewer: "a", Verdict: "unverified"}, sighting{Run: "20261017T000000.2Z-b"})
	sf.sight(finding{Lens: "l", Title: "x", Reviewer: "a", Verdict: "confirmed"}, sighting{Run: "20261017T000000.1Z-a"})
	if sf.Verdict != "confirmed" || sf.Sightings != 2 {
		t.Fatalf("problem = %+v", sf)
	}
	// An earlier review does not replace a later review's verification.
	sf.sight(finding{Lens: "l", Title: "x", Reviewer: "b", Verdict: "rejected"}, sighting{Run: "20261017T000000.05Z-c"})
	if sf.Verdict != "confirmed" {
		t.Fatalf("verdict = %s", sf.Verdict)
	}
}

func TestCarriedProblemsWinAndAreReservedFirst(t *testing.T) {
	path := filepath.Join(t.TempDir(), "findings.json")
	withStore(path, func(s *findingStore) error {
		s.add(finding{Lens: "l", Title: "Same title", Reviewer: "a", Verdict: "confirmed"}, sighting{Run: "20261017T000000.000000000Z-1"})
		s.find("F1").Status = storeDismissed
		s.add(finding{Lens: "l", Title: "Other", Reviewer: "a", Verdict: "confirmed"}, sighting{Run: "20261016T000000.000000000Z-1"})
		return nil
	})
	// The fresh finding comes first and shares the carried problem's title;
	// the matcher told them apart and pointed the reused one elsewhere.
	r := rememberRun(t, path, "20261017T000000.000000000Z-1",
		finding{ID: "b.l.1", Lens: "l", Title: "Same title", Reviewer: "b", Verdict: "confirmed"},
		finding{ID: "a.l.1", Lens: "l", Title: "Same title", Reviewer: "a", Verdict: "confirmed", carried: "F1"})
	if err := r.store(map[string]string{"a.l.1": "F2"}, nil); err != nil {
		t.Fatal(err)
	}
	if ids := []string{r.report.Findings[0].StoreID, r.report.Findings[1].StoreID}; ids[0] == "F1" || ids[1] != "F1" {
		t.Fatalf("store IDs = %v", ids)
	}
}

func TestLastSightingDoesNotGoBack(t *testing.T) {
	var sf storedFinding
	sf.sight(finding{Lens: "l", Title: "x", Reviewer: "a", Verdict: "confirmed"}, sighting{Run: "20261017T000000.1Z-a", Commit: "c1"})
	sf.sight(finding{Lens: "l", Title: "x", Reviewer: "a", Verdict: "confirmed"}, sighting{Run: "20261017T000000.2Z-b", Commit: "c2"})
	sf.sight(finding{Lens: "l", Title: "x", Reviewer: "a", Verdict: "confirmed"}, sighting{Run: "20261017T000000.1Z-a", Commit: "c1"})
	if sf.LastSeen.Commit != "c2" {
		t.Fatalf("last seen = %+v", sf.LastSeen)
	}
}

func TestReusedFindingPullsItsGroupOntoItsProblem(t *testing.T) {
	path := filepath.Join(t.TempDir(), "findings.json")
	withStore(path, func(s *findingStore) error {
		s.add(finding{Lens: "l", Title: "Old wording", Reviewer: "a", Verdict: "confirmed"}, sighting{Run: "20261017T000000.000000000Z-1"})
		s.find("F1").Status = storeDismissed
		return nil
	})
	// The fresh finding comes first; the matcher says the reused one, which
	// keeps F1, repeats it.
	r := rememberRun(t, path, "20261017T000000.000000000Z-1",
		finding{ID: "b.l.1", Lens: "l", Title: "New wording", Reviewer: "b", Verdict: "confirmed"},
		finding{ID: "a.l.1", Lens: "l", Title: "Old wording", Reviewer: "a", Verdict: "confirmed", carried: "F1"})
	if err := r.store(map[string]string{"a.l.1": "b.l.1"}, nil); err != nil {
		t.Fatal(err)
	}
	for _, f := range r.report.Findings {
		if f.StoreID != "F1" || f.StoreStatus != storeDismissed {
			t.Fatalf("finding = %+v", f)
		}
	}
	if s, _ := readStore(path); len(s.Findings) != 1 {
		t.Fatalf("store = %+v", s.Findings)
	}
}

func TestReusedFindingsKeepTheirOwnProblems(t *testing.T) {
	path := filepath.Join(t.TempDir(), "findings.json")
	withStore(path, func(s *findingStore) error {
		s.add(finding{Lens: "l", Title: "One", Reviewer: "a", Verdict: "confirmed"}, sighting{Run: "20261017T000000.000000000Z-1"})
		s.add(finding{Lens: "l", Title: "Two", Reviewer: "b", Verdict: "confirmed"}, sighting{Run: "20261017T000000.000000000Z-1"})
		s.find("F1").Status = storeDismissed
		return nil
	})
	r := rememberRun(t, path, "20261017T000000.000000000Z-1",
		finding{ID: "a.l.1", Lens: "l", Title: "One", Reviewer: "a", Verdict: "confirmed", carried: "F1"},
		finding{ID: "b.l.1", Lens: "l", Title: "Two", Reviewer: "b", Verdict: "confirmed", carried: "F2"})
	// The matcher now groups them; each still keeps its own problem.
	if err := r.store(map[string]string{"b.l.1": "a.l.1"}, nil); err != nil {
		t.Fatal(err)
	}
	if ids := []string{r.report.Findings[0].StoreID, r.report.Findings[1].StoreID}; ids[0] != "F1" || ids[1] != "F2" {
		t.Fatalf("store IDs = %v", ids)
	}
}

func TestAGroupFollowsEveryLink(t *testing.T) {
	path := filepath.Join(t.TempDir(), "findings.json")
	withStore(path, func(s *findingStore) error {
		s.add(finding{Lens: "l", Title: "Kept", Reviewer: "c", Verdict: "confirmed"}, sighting{Run: "20261017T000000.000000000Z-1"})
		s.find("F1").Status = storeDismissed
		return nil
	})
	r := rememberRun(t, path, "20261017T000000.000000000Z-1",
		finding{ID: "x.l.1", Lens: "l", Title: "First wording", Reviewer: "x", Verdict: "confirmed"},
		finding{ID: "b.l.1", Lens: "l", Title: "Second wording", Reviewer: "b", Verdict: "confirmed"},
		finding{ID: "c.l.1", Lens: "l", Title: "Kept", Reviewer: "c", Verdict: "confirmed", carried: "F1"})
	if err := r.store(map[string]string{"b.l.1": "x.l.1", "c.l.1": "b.l.1"}, nil); err != nil {
		t.Fatal(err)
	}
	for _, f := range r.report.Findings {
		if f.StoreID != "F1" || f.StoreStatus != storeDismissed {
			t.Fatalf("finding = %+v", f)
		}
	}
	if s, _ := readStore(path); len(s.Findings) != 1 {
		t.Fatalf("store = %+v", s.Findings)
	}
}

func TestMatchGroups(t *testing.T) {
	findings := []finding{{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}}
	groups := matchGroups(findings, map[string]string{"b": "a", "d": "c", "c": "F9"})
	if fmt.Sprint(groups) != "[[0 1] [2 3]]" {
		t.Fatalf("groups = %v", groups)
	}
}
