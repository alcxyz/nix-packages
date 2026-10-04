package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func feedEvent(id int64, op, repo, content string, created time.Time) forgejoEvent {
	e := forgejoEvent{ID: id, OpType: op, Content: content, Created: created}
	e.Repo.FullName = repo
	return e
}

func TestSummarizeForgejoEvents(t *testing.T) {
	day := time.Date(2026, 10, 3, 12, 0, 0, 0, time.Local)
	night := time.Date(2026, 10, 4, 1, 0, 0, 0, time.Local)
	push := `{"Commits":[{"Sha1":"aaa","Message":"feat: one\n\nbody"},{"Sha1":"bbb","Message":"fix: two"}],"Len":2}`
	// fetchEvents appends days oldest first, each day newest first, so the
	// merge after midnight comes before the day's events.
	events := []forgejoEvent{
		feedEvent(3, "merge_pull_request", "o/r", `["4","PR title"]`, night),
		feedEvent(8, "commit_repo", "o/r", "", day),
		feedEvent(7, "mirror_sync_push", "o/mirror", `{"Commits":[{"Sha1":"ccc","Message":"x"}]}`, day),
		feedEvent(6, "close_issue", "o/r", `["9",""]`, day),
		feedEvent(5, "comment_issue", "o/r", `["9","comment text"]`, day),
		feedEvent(4, "create_issue", "o/r", `["9","Issue title"]`, day),
		feedEvent(9, "approve_pull_request", "o/r", `["4","LGTM"]`, day),
		feedEvent(2, "comment_pull", "o/r", `["4","review text"]`, day),
		feedEvent(1, "create_pull_request", "o/r", `["4","PR title"]`, day),
		feedEvent(0, "commit_repo", "o/r", push, day),
	}
	noCompare := func(repo, baseHead, head string, total int) ([]commitRef, error) {
		t.Fatalf("unexpected compare %s %s", repo, baseHead)
		return nil, nil
	}

	commits, prs, issues, err := summarizeForgejoEvents(events, noCompare)
	if err != nil {
		t.Fatal(err)
	}

	if len(commits) != 2 || commits[0].SHA != "aaa" || commits[0].Message != "feat: one" || commits[1].Source != "forgejo" {
		t.Errorf("commits = %+v", commits)
	}
	if want := "- [o/r] #4 PR title (opened, commented, approved, merged)"; prs != want {
		t.Errorf("prs = %q, want %q", prs, want)
	}
	if want := "- [o/r] #9 Issue title (opened, commented, closed)"; issues != want {
		t.Errorf("issues = %q, want %q", issues, want)
	}
}

func TestSummarizeForgejoEventsExpandsTruncatedPush(t *testing.T) {
	at := time.Date(2026, 10, 3, 12, 0, 0, 0, time.Local)
	push := `{"Commits":[{"Sha1":"e","Message":"five"}],"Len":3,"CompareURL":"o/r/compare/base...head"}`
	var gotBaseHead string
	compare := func(repo, baseHead, head string, total int) ([]commitRef, error) {
		gotBaseHead = baseHead
		return []commitRef{{Source: "forgejo", Repo: repo, SHA: "c"}, {Source: "forgejo", Repo: repo, SHA: "d"}, {Source: "forgejo", Repo: repo, SHA: "e"}}, nil
	}

	commits, _, _, err := summarizeForgejoEvents([]forgejoEvent{feedEvent(1, "commit_repo", "o/r", push, at)}, compare)

	if err != nil || len(commits) != 3 || gotBaseHead != "base...head" {
		t.Errorf("commits = %+v, baseHead = %q, err = %v", commits, gotBaseHead, err)
	}

	failing := func(repo, baseHead, head string, total int) ([]commitRef, error) { return nil, fmt.Errorf("boom") }
	if _, _, _, err := summarizeForgejoEvents([]forgejoEvent{feedEvent(1, "commit_repo", "o/r", push, at)}, failing); err == nil {
		t.Error("expected a compare failure to fail the summary")
	}

	// A pruned base keeps the listed commits instead of failing every later
	// run.
	pruned := func(repo, baseHead, head string, total int) ([]commitRef, error) {
		return nil, &forgejoStatusError{path: "/compare", code: http.StatusNotFound}
	}
	commits, _, _, err = summarizeForgejoEvents([]forgejoEvent{feedEvent(1, "commit_repo", "o/r", push, at)}, pruned)
	if err != nil || len(commits) != 1 || commits[0].SHA != "e" {
		t.Errorf("pruned: commits = %+v, err = %v", commits, err)
	}

	// An initial push has no compare URL; its commits come from the head.
	initial := `{"Commits":[{"Sha1":"e","Message":"five"}],"Len":3,"CompareURL":"","HeadCommit":{"Sha1":"e"}}`
	var gotHead string
	var gotTotal int
	history := func(repo, baseHead, head string, total int) ([]commitRef, error) {
		gotHead, gotTotal = head, total
		return []commitRef{{SHA: "c"}, {SHA: "d"}, {SHA: "e"}}, nil
	}
	commits, _, _, err = summarizeForgejoEvents([]forgejoEvent{feedEvent(1, "commit_repo", "o/r", initial, at)}, history)
	if err != nil || len(commits) != 3 || gotHead != "e" || gotTotal != 3 {
		t.Errorf("initial: commits = %+v, head = %q, total = %d, err = %v", commits, gotHead, gotTotal, err)
	}
}

func TestFetchPushCommits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/o/r/compare/base...head":
			w.Write([]byte(`{"commits":[{"sha":"old","commit":{"message":"committed yesterday"}},{"sha":"new","commit":{"message":"today\n\nbody"}}]}`))
		case r.URL.Path == "/repos/o/r/commits" && r.URL.Query().Get("sha") == "head":
			// Two pages of history: the server caps the page size at 2.
			switch r.URL.Query().Get("page") {
			case "1":
				w.Write([]byte(`[{"sha":"c3","commit":{"message":"three"}},{"sha":"c2","commit":{"message":"two"}}]`))
			case "2":
				w.Write([]byte(`[{"sha":"c1","commit":{"message":"one"}},{"sha":"c0","commit":{"message":"root"}}]`))
			default:
				w.Write([]byte(`[]`))
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	client := &forgejoClient{baseURL: srv.URL, token: "t", http: srv.Client()}

	// Work committed before the push is kept: this push is where it first
	// becomes visible.
	commits, err := client.fetchPushCommits("o/r", "base...head", "head", 2)
	if err != nil || len(commits) != 2 || commits[1].Message != "today" {
		t.Errorf("compare: commits = %+v, err = %v", commits, err)
	}

	commits, err = client.fetchPushCommits("o/r", "", "head", 3)
	if err != nil || len(commits) != 3 || commits[2].SHA != "c1" {
		t.Errorf("history: commits = %+v, err = %v", commits, err)
	}
}

func TestMergeCommitsDropsMirroredSHAs(t *testing.T) {
	forgejo := []commitRef{{Source: "forgejo", Repo: "o/r", SHA: "aaa"}}
	github := []commitRef{{Source: "github", Repo: "o/r", SHA: "aaa"}, {Source: "github", Repo: "o/gh", SHA: "bbb"}}

	merged := mergeCommits(forgejo, github)

	if len(merged) != 2 || merged[0].Source != "forgejo" || merged[1].SHA != "bbb" {
		t.Errorf("merged = %+v", merged)
	}
}

func TestForgejoFetchEvents(t *testing.T) {
	start, end := devlogWindow(time.Date(2026, 10, 3, 0, 0, 0, 0, time.Local))
	inWindow := start.Add(time.Hour)
	before := start.Add(-time.Minute)

	// One full page plus one event on 2026-10-03, and an event before the
	// window that a neighbouring day's query also returns.
	pages := map[string][][]forgejoEvent{}
	var full []forgejoEvent
	for i := 0; i < forgejoFeedPageSize; i++ {
		full = append(full, feedEvent(int64(100+i), "create_issue", "o/r", fmt.Sprintf(`["%d","t"]`, i), inWindow))
	}
	pages["2026-10-03"] = [][]forgejoEvent{full, {feedEvent(1, "create_issue", "o/r", `["x","t"]`, inWindow)}}
	pages["2026-10-02"] = [][]forgejoEvent{{feedEvent(2, "create_issue", "o/r", `["y","t"]`, before)}}
	pages["2026-10-04"] = [][]forgejoEvent{{feedEvent(1, "create_issue", "o/r", `["x","t"]`, inWindow)}}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/api/v1/users/alcxyz/activities/feeds" || r.URL.Query().Get("only-performed-by") != "true" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		batches := pages[r.URL.Query().Get("date")]
		var batch []forgejoEvent
		if page >= 1 && page <= len(batches) {
			batch = batches[page-1]
		}
		if batch == nil {
			batch = []forgejoEvent{}
		}
		json.NewEncoder(w).Encode(batch)
	}))
	defer srv.Close()

	tokenFile := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tokenFile, []byte("secret\n"), 0600)
	t.Setenv("FORGEJO_API_TOKEN_FILE", tokenFile)

	client, err := newForgejoClient(srv.URL+"/", "alcxyz")
	if err != nil {
		t.Fatal(err)
	}
	events, err := client.fetchEvents(start, end)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != forgejoFeedPageSize+1 {
		t.Errorf("got %d events, want %d", len(events), forgejoFeedPageSize+1)
	}
}

func TestNewSourcesRequiresForgejoToken(t *testing.T) {
	t.Setenv("FORGEJO_API_TOKEN_FILE", "")
	if _, err := newSources("u", "https://forge.example", "u"); err == nil {
		t.Error("expected an error without FORGEJO_API_TOKEN_FILE")
	}
	src, err := newSources("u", "", "u")
	if err != nil || src.forgejo != nil {
		t.Errorf("Forgejo should be disabled without a URL: %+v, %v", src, err)
	}
}

func TestFetchDiffsStopsAtBudget(t *testing.T) {
	diff := ""
	for i := 0; i < maxDiffLines*2; i++ {
		diff += "+line\n"
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(diff))
	}))
	defer srv.Close()
	client := &forgejoClient{baseURL: srv.URL, token: "t", http: srv.Client()}

	var commits []commitRef
	n := maxTotalDiffLines/maxDiffLines + 2
	for i := 0; i < n; i++ {
		commits = append(commits, commitRef{Source: "forgejo", Repo: "o/r", SHA: fmt.Sprint(i), Message: "m"})
	}

	out, err := fetchDiffs(commits, client)
	if err != nil {
		t.Fatal(err)
	}

	if got := strings.Count(out, "(truncated)"); got != maxTotalDiffLines/maxDiffLines {
		t.Errorf("got %d truncated diffs, want %d", got, maxTotalDiffLines/maxDiffLines)
	}
	if got := strings.Count(out, "budget reached"); got != 2 {
		t.Errorf("got %d omitted diffs, want 2", got)
	}
	if got := strings.Count(out, "+line"); got != maxTotalDiffLines {
		t.Errorf("got %d diff lines, want %d", got, maxTotalDiffLines)
	}
}

func TestFetchDiffsCapsDiffAtRemainingBudget(t *testing.T) {
	diff := strings.Repeat("+line\n", maxDiffLines-1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(diff))
	}))
	defer srv.Close()
	client := &forgejoClient{baseURL: srv.URL, token: "t", http: srv.Client()}

	// Each diff is maxDiffLines lines including the trailing empty line, so
	// the budget does not divide evenly once one more short commit is added.
	var commits []commitRef
	for i := 0; i < maxTotalDiffLines/maxDiffLines+1; i++ {
		commits = append(commits, commitRef{Source: "forgejo", Repo: "o/r", SHA: fmt.Sprint(i), Message: "m"})
	}
	out, err := fetchDiffs(commits, client)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(out, "+line"); got > maxTotalDiffLines {
		t.Errorf("got %d diff lines, over the %d budget", got, maxTotalDiffLines)
	}
}

func TestFetchDiffsFailsOnForgejoError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	client := &forgejoClient{baseURL: srv.URL, token: "t", http: srv.Client()}

	if _, err := fetchDiffs([]commitRef{{Source: "forgejo", Repo: "o/r", SHA: "a"}}, client); err == nil {
		t.Error("expected a Forgejo diff error to fail")
	}
}

func TestFetchDiffsToleratesPrunedCommit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	client := &forgejoClient{baseURL: srv.URL, token: "t", http: srv.Client()}

	out, err := fetchDiffs([]commitRef{{Source: "forgejo", Repo: "o/r", SHA: "a", Message: "m"}}, client)
	if err != nil || !strings.Contains(out, "(diff unavailable)") {
		t.Errorf("out = %q, err = %v", out, err)
	}
}
