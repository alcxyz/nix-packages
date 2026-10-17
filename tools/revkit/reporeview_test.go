package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"revkit/review"
)

// TestMain lets the test binary play a client: the fake claude and codex
// scripts run it with REVKIT_FAKE_CLIENT set.
func TestMain(m *testing.M) {
	if client := os.Getenv("REVKIT_FAKE_CLIENT"); client != "" {
		os.Exit(fakeClient(client))
	}
	os.Exit(m.Run())
}

var (
	findingID = regexp.MustCompile(`"id": "([^"]+)"`)
	knownID   = regexp.MustCompile(`"id": "(F[0-9]+)"`)
)

// fakeMatches says every new finding repeats the first known one, or, with
// none known, the first new one: the fake reviewers all report one problem.
func fakeMatches(prompt string) string {
	known, fresh, _ := strings.Cut(prompt, "New findings:")
	var target string
	if m := knownID.FindStringSubmatch(known); m != nil {
		target = m[1]
	}
	var entries []string
	for i, m := range findingID.FindAllStringSubmatch(fresh, -1) {
		same := target
		if same == "" && i > 0 {
			same = findingID.FindStringSubmatch(fresh)[1]
		}
		if same == "" {
			entries = append(entries, fmt.Sprintf(`{"id": %q, "same": null}`, m[1]))
		} else {
			entries = append(entries, fmt.Sprintf(`{"id": %q, "same": %q}`, m[1], same))
		}
	}
	return `{"matches": [` + strings.Join(entries, ", ") + `]}`
}

func fakeClient(client string) int {
	prompt, _ := io.ReadAll(os.Stdin)
	failing := os.Getenv("REVKIT_FAKE_FAIL")
	if path := os.Getenv("REVKIT_FAKE_FAIL_FILE"); path != "" {
		content, _ := os.ReadFile(path)
		failing = strings.TrimSpace(string(content))
	}
	if failing == client {
		fmt.Fprintln(os.Stderr, "not logged in")
		return 1
	}
	var reply string
	if strings.Contains(string(prompt), "You are matching review findings") {
		reply = fakeMatches(string(prompt))
		if path := os.Getenv("REVKIT_FAKE_MATCH_FILE"); path != "" {
			if content, _ := os.ReadFile(path); strings.TrimSpace(string(content)) == "none" {
				reply = strings.NewReplacer(`"same": "`, `"was": "`).Replace(reply)
				reply = regexp.MustCompile(`"was": "[^"]*"`).ReplaceAllString(reply, `"same": null`)
			}
		}
	} else if strings.Contains(string(prompt), "You are verifying") {
		var verdicts []string
		for _, match := range findingID.FindAllStringSubmatch(string(prompt), -1) {
			verdicts = append(verdicts, fmt.Sprintf(`{"id": %q, "verdict": "confirmed", "reason": "read it"}`, match[1]))
		}
		reply = `{"verdicts": [` + strings.Join(verdicts, ", ") + `]}`
		if os.Getenv("REVKIT_FAKE_NO_VERDICTS") == client {
			reply = `{"verdicts": []}`
		}
	} else if os.Getenv("REVKIT_FAKE_JUNK") == client {
		reply = `{"findings": [7, "x"]}`
	} else if os.Getenv("REVKIT_FAKE_PROSE") == client {
		reply = "I looked around and everything seems fine."
	} else {
		reply = fmt.Sprintf("```json\n{\"findings\": [{\"title\": \"Stale %s note\", \"severity\": \"Medium\", "+
			"\"paths\": [\"README.md:1\"], \"claim\": \"README names a removed file.\", \"evidence\": \"line 1\", "+
			"\"suggestion\": \"remove it\"}, 7]}\n```\n", client)
	}
	blind := os.Getenv("REVKIT_FAKE_BLIND") == client
	if os.Getenv("REVKIT_FAKE_ERROR") == client {
		reply = `{"error": "every command was blocked"}`
	}
	if client == "claude" {
		if !blind {
			fmt.Println(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"1","content":"x"}]}}`)
		}
		result, _ := json.Marshal(map[string]any{
			"type": "result", "result": reply, "is_error": false,
			"usage": map[string]int{"input_tokens": 3, "cache_read_input_tokens": 2000, "cache_creation_input_tokens": 100, "output_tokens": 40},
		})
		fmt.Println(string(result))
		return 0
	}
	for i, arg := range os.Args {
		if arg == "-o" && i+1 < len(os.Args) {
			os.WriteFile(os.Args[i+1], []byte(reply), 0o644)
		}
	}
	if !blind {
		fmt.Println(`{"type":"item.completed","item":{"type":"command_execution","exit_code":0}}`)
	}
	fmt.Println(`{"type":"turn.completed","usage":{"input_tokens":1000,"cached_input_tokens":900,"output_tokens":20}}`)
	return 0
}

type reviewFixture struct {
	repo, config, state string
}

func newReviewFixture(t *testing.T, extra string) reviewFixture {
	t.Helper()
	// The test's state directory is a temporary one; pretend no directory
	// is hidden from reviewers.
	saved := hiddenFromCodex
	hiddenFromCodex = ""
	t.Cleanup(func() { hiddenFromCodex = saved })
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	for _, client := range []string{"claude", "codex"} {
		script := fmt.Sprintf("#!/bin/sh\nREVKIT_FAKE_CLIENT=%s %s exec %q \"$@\"\n", client, extra, self)
		if err := os.WriteFile(filepath.Join(bin, client), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))

	repo := filepath.Join(t.TempDir(), "example")
	os.MkdirAll(repo, 0o755)
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"commit", "-q", "-m", "first"}} {
		if args[0] == "add" {
			os.WriteFile(filepath.Join(repo, "README.md"), []byte("see removed.md\n"), 0o644)
		}
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.invalid"}, args...)...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	state := t.TempDir()
	config := strings.Replace(validConfig, `"timeoutSeconds": 1800`,
		fmt.Sprintf(`"timeoutSeconds": 60, "stateDir": %q`, state), 1)
	return reviewFixture{repo: repo, config: writeConfig(t, config), state: state}
}

func (f reviewFixture) results(t *testing.T) runReport {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(f.state, "repo-review", "example", "*", "runs", "*", "results.json"))
	if len(matches) != 1 {
		t.Fatalf("results: %v", matches)
	}
	content, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	var report runReport
	if err := json.Unmarshal(content, &report); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(matches[0]), "report.md")); err != nil {
		t.Fatal(err)
	}
	return report
}

func TestRepoReviewFindsAndCrossVerifies(t *testing.T) {
	f := newReviewFixture(t, "")
	if code := repoReview(context.Background(), []string{"--config", f.config, f.repo}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	report := f.results(t)
	if !report.Complete || len(report.Jobs) != 4 || len(report.Findings) != 2 {
		t.Fatalf("report = %+v", report)
	}
	for _, finding := range report.Findings {
		if finding.Verdict != "confirmed" || finding.Severity != "medium" {
			t.Fatalf("finding = %+v", finding)
		}
		// Each model's findings are checked by the other model.
		if finding.Verifier == finding.Reviewer {
			t.Fatalf("%s verified its own finding", finding.Reviewer)
		}
	}
	for _, job := range report.Jobs {
		if job.Kind == "find" && (len(job.Notes) != 1 || !strings.Contains(job.Notes[0], "dropped 1")) {
			t.Fatalf("job notes = %v", job.Notes)
		}
	}
	trees, _ := filepath.Glob(filepath.Join(f.state, "repo-review", "example", "*", "tree", "README.md"))
	if len(trees) != 1 {
		t.Fatalf("tree: %v", trees)
	}
}

func TestRepoReviewFailedReviewerIsMissing(t *testing.T) {
	f := newReviewFixture(t, "REVKIT_FAKE_FAIL=codex")
	if code := repoReview(context.Background(), []string{"--config", f.config, f.repo}); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	report := f.results(t)
	if report.Complete {
		t.Fatal("a run with a failed reviewer counts as complete")
	}
	// Codex failed to find, so it verified nothing: Claude's finding stays
	// unverified, and Codex has none, rather than "no findings".
	if len(report.Findings) != 1 || report.Findings[0].Verdict != "unverified" {
		t.Fatalf("findings = %+v", report.Findings)
	}
	failed := 0
	for _, job := range report.Jobs {
		if job.Status != "ok" {
			failed++
		}
	}
	if failed != 2 {
		t.Fatalf("failed jobs = %d, want the find and the verification", failed)
	}
}

func TestRepoReviewProseReplyIsMissing(t *testing.T) {
	f := newReviewFixture(t, "REVKIT_FAKE_PROSE=claude")
	if code := repoReview(context.Background(), []string{"--config", f.config, "--reviewer", "opus", f.repo}); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	report := f.results(t)
	if len(report.Jobs) != 1 || report.Jobs[0].Status != statusUnparsed {
		t.Fatalf("jobs = %+v", report.Jobs)
	}
}

func TestRepoReviewReviewerThatCouldNotReadIsMissing(t *testing.T) {
	for _, extra := range []string{"REVKIT_FAKE_BLIND=codex", "REVKIT_FAKE_ERROR=codex"} {
		t.Run(extra, func(t *testing.T) {
			f := newReviewFixture(t, extra)
			if code := repoReview(context.Background(), []string{"--config", f.config, "--reviewer", "gpt", f.repo}); code != 1 {
				t.Fatalf("exit %d, want 1", code)
			}
			report := f.results(t)
			if len(report.Jobs) != 1 || report.Jobs[0].Status == "ok" || len(report.Findings) != 0 {
				t.Fatalf("report = %+v", report)
			}
		})
	}
}

func TestRepoReviewVerifierWithoutVerdictsIsMissing(t *testing.T) {
	f := newReviewFixture(t, "REVKIT_FAKE_NO_VERDICTS=claude")
	if code := repoReview(context.Background(), []string{"--config", f.config, f.repo}); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	report := f.results(t)
	if report.Complete {
		t.Fatal("a run with findings left unverified counts as complete")
	}
	for _, job := range report.Jobs {
		if job.Kind == "verify" && job.Reviewer == "opus" && (job.Status == "ok" || !strings.Contains(job.Problem, "no verdict")) {
			t.Fatalf("job = %+v", job)
		}
	}
}

func TestRepoReviewMalformedFindingsAreMissing(t *testing.T) {
	f := newReviewFixture(t, "REVKIT_FAKE_JUNK=codex")
	if code := repoReview(context.Background(), []string{"--config", f.config, "--reviewer", "gpt", f.repo}); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if report := f.results(t); report.Jobs[0].Status != statusUnparsed {
		t.Fatalf("jobs = %+v", report.Jobs)
	}
}

func TestRepoReviewIncompleteWhenJobsAreSkipped(t *testing.T) {
	r := &reviewRun{report: runReport{Jobs: []jobRecord{
		{Kind: "find", Reviewer: "gpt", Lens: "a", Result: review.Result{Status: review.StatusOK}},
	}}}
	if r.complete(2) {
		t.Fatal("a run missing a planned lens counts as complete")
	}
	if !r.complete(1) {
		t.Fatal("a run with every planned job counts as incomplete")
	}
	f := newReviewFixture(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if code := repoReview(ctx, []string{"--config", f.config, f.repo}); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	report := f.results(t)
	if report.Complete || len(report.Jobs) != 2 || report.Jobs[0].Status != review.StatusInterrupted {
		t.Fatalf("report = %+v", report)
	}
}

func TestRepoReviewDryRunAndSelection(t *testing.T) {
	f := newReviewFixture(t, "")
	if code := repoReview(context.Background(), []string{"--config", f.config, "--dry-run", f.repo}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if entries, _ := os.ReadDir(f.state); len(entries) != 0 {
		t.Fatalf("a dry run wrote %v", entries)
	}
	for _, args := range [][]string{
		{"--lens", "nope", f.repo},
		{"--reviewer", "nope", f.repo},
		{"--reviewer", "gpt", "--reviewer", "gpt", f.repo},
		{"--lens", "docs-drift", "--lens", "docs-drift", f.repo},
		{"--repository", "nope", f.repo},
		{"--ref", "nope", f.repo},
		{t.TempDir()},
	} {
		if code := repoReview(context.Background(), append([]string{"--config", f.config, "--dry-run"}, args...)); code != 2 {
			t.Fatalf("%v: exit %d, want 2", args, code)
		}
	}
}

func TestRepoReviewRefusesStateUnderTemp(t *testing.T) {
	f := newReviewFixture(t, "")
	hiddenFromCodex = filepath.Dir(f.state)
	if code := repoReview(context.Background(), []string{"--config", f.config, "--dry-run", f.repo}); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	// Claude reviewers do not run under Codex's profile.
	if code := repoReview(context.Background(), []string{"--config", f.config, "--dry-run", "--reviewer", "opus", f.repo}); code != 0 {
		t.Fatalf("Claude only: exit %d, want 0", code)
	}
	link := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(filepath.Dir(f.state), link); err != nil {
		t.Fatal(err)
	}
	if !within(filepath.Join(link, "x", "revkit"), filepath.Dir(f.state)) {
		t.Fatal("a state directory reached through a link counts as outside")
	}
	if within("/tmpfoo/revkit", "/tmp") {
		t.Fatal("a sibling with a common prefix counts as inside")
	}
}

func TestParseReplies(t *testing.T) {
	if _, _, problem := parseFindings("no json here"); problem == "" {
		t.Fatal("parsed prose")
	}
	if findings, _, problem := parseFindings(`{"findings": []}`); problem != "" || findings == nil || len(findings) != 0 {
		t.Fatalf("empty findings = %v, %q", findings, problem)
	}
	if _, _, problem := parseFindings(`{"findings": [7]}`); problem == "" {
		t.Fatal("parsed a list without a single finding as a review without findings")
	}
	if _, _, problem := parseFindings(`{"other": []}`); problem == "" {
		t.Fatal("parsed a reply without findings")
	}
	if _, _, problem := parseFindings(`{"findings": [], "error": "blocked"}`); !strings.Contains(problem, "blocked") {
		t.Fatalf("problem = %q", problem)
	}
	verdicts, problem := parseVerdicts(`Here: {"verdicts": [{"id": "a", "verdict": "Confirmed"}, {"id": "b", "verdict": "maybe"}]}`)
	if problem != "" || len(verdicts) != 1 || verdicts[0].Verdict != "confirmed" {
		t.Fatalf("verdicts = %+v, %q", verdicts, problem)
	}
}
