package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfiguredGithubPrimaryReposUsesEnvOverride(t *testing.T) {
	t.Setenv("FORGE_MIRROR_GITHUB_PRIMARY_REPOS", "repo-a, repo-b\nrepo-c")

	repos, configured := configuredGithubPrimaryRepos()
	if !configured {
		t.Fatal("expected env configuration to be detected")
	}

	for _, name := range []string{"repo-a", "repo-b", "repo-c"} {
		if !repos[name] {
			t.Fatalf("expected %q in configured repo set", name)
		}
	}

	if repos["unconfigured-repo"] {
		t.Fatal("unexpected repo in configured set")
	}
}

func TestConfiguredGithubPrimaryReposReadsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "repos.txt")
	t.Setenv("FORGE_MIRROR_GITHUB_PRIMARY_REPOS_FILE", path)

	if err := os.WriteFile(path, []byte("repo-a\nrepo-b,repo-c"), 0o600); err != nil {
		t.Fatal(err)
	}

	repos, configured := configuredGithubPrimaryRepos()
	if !configured {
		t.Fatal("expected file configuration to be detected")
	}

	for _, name := range []string{"repo-a", "repo-b", "repo-c"} {
		if !repos[name] {
			t.Fatalf("expected %q in configured repo set", name)
		}
	}
}

func TestConfiguredGithubPrimaryReposFailsClosedWithoutConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	repos, configured := configuredGithubPrimaryRepos()
	if configured {
		t.Fatal("expected missing configuration to be reported")
	}
	if len(repos) != 0 {
		t.Fatalf("expected empty repo set, got %d entries", len(repos))
	}
}

func TestFetchForgejoReposPaginates(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "token test-token" {
			t.Fatalf("unexpected authorization header: %q", got)
		}

		switch r.URL.Query().Get("page") {
		case "1":
			fmt.Fprint(w, `{"data":[{"name":"one"}]}`)
		case "2":
			fmt.Fprint(w, `{"data":[]}`)
		default:
			t.Fatalf("unexpected page: %s", r.URL.Query().Get("page"))
		}
	}))
	defer server.Close()

	repos, err := fetchForgejoRepos(server.URL, "alcxyz", "test-token")
	if err != nil {
		t.Fatalf("fetchForgejoRepos returned error: %v", err)
	}
	if len(repos) != 1 || repos[0].Name != "one" {
		t.Fatalf("unexpected repositories: %#v", repos)
	}
}

func TestFetchForgejoReposReportsHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, "404 page not found")
	}))
	defer server.Close()

	_, err := fetchForgejoRepos(server.URL, "alcxyz", "test-token")
	if err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("expected HTTP 404 error, got %v", err)
	}
}
