package main

import (
	"os"
	"path/filepath"
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
