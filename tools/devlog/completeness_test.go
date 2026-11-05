package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mockSearch(t *testing.T, body string) {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\n"+body), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
}

func TestSearchPaginatesAndChecksCompleteness(t *testing.T) {
	mockSearch(t, `case "$2" in
 *page=1) echo '{"total_count":2,"incomplete_results":false,"items":[{"sha":"one"}]}' ;;
 *page=2) echo '{"total_count":2,"incomplete_results":false,"items":[{"sha":"two"}]}' ;;
 *) exit 98 ;;
esac`)
	out, err := completeSearch("commits", "synthetic")
	if err != nil || !strings.Contains(out, "two") {
		t.Fatalf("%s: %v", out, err)
	}
	for _, body := range []string{
		`echo '{"total_count":2,"incomplete_results":true,"items":[]}'`,
		`echo '{"total_count":1001,"incomplete_results":false,"items":[]}'`,
		`echo '{"total_count":2,"incomplete_results":false,"items":[]}'`,
		`echo '{"total_count":2,"incomplete_results":false,"items":[{"sha":"one"}]}'`,
		`echo '{"items":[]}'`,
	} {
		mockSearch(t, body)
		if _, err := completeSearch("commits", "synthetic"); err == nil {
			t.Fatalf("incomplete result accepted: %s", body)
		}
	}
}

func TestDailyDoesNotPersistFailedConfiguredSource(t *testing.T) {
	for _, source := range []string{"commits", "pr", "issue"} {
		mockSearch(t, `case "$2" in
 *search/commits*) kind=commits ;;
 *type:pr*) kind=pr ;;
 *) kind=issue ;;
esac
if [ "$kind" = "$FAIL_SOURCE" ]; then exit 1; fi
echo '{"total_count":0,"incomplete_results":false,"items":[]}'`)
		t.Setenv("FAIL_SOURCE", source)
		repo := t.TempDir()
		date := time.Date(2026, 10, 3, 0, 0, 0, 0, time.Local)
		if generated, err := generateDaily(repo, date, activitySources{ghUser: "synthetic"}); err == nil || generated {
			t.Fatalf("%s failure accepted", source)
		}
		if _, err := os.Stat(filepath.Join(repo, "devlog", "2026-10-03.md")); !os.IsNotExist(err) {
			t.Fatal("incomplete day persisted")
		}
	}
}

func TestComparePaginationAndTruncation(t *testing.T) {
	for _, truncated := range []bool{false, true} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Query().Get("page") {
			case "1":
				fmt.Fprint(w, `{"commits":[{"sha":"one"}]}`)
			case "2":
				if truncated {
					fmt.Fprint(w, `{"commits":[]}`)
				} else {
					fmt.Fprint(w, `{"commits":[{"sha":"two"}]}`)
				}
			default:
				t.Errorf("unexpected page %s", r.URL)
				fmt.Fprint(w, `{"commits":[]}`)
			}
		}))
		client := &forgejoClient{baseURL: srv.URL, user: "fixture", http: srv.Client()}
		commits, err := client.fetchPushCommits("example/repo", "base...head", "head", 2)
		srv.Close()
		if truncated && err == nil {
			t.Fatal("truncated compare accepted")
		}
		if !truncated && (err != nil || len(commits) != 2) {
			t.Fatalf("paginated compare: %v, %v", commits, err)
		}
	}
}
