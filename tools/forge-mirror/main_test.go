package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfiguredGithubPrimaryReposUsesEnvOverride(t *testing.T) {
	t.Setenv("FORGE_MIRROR_GITHUB_PRIMARY_REPOS", "repo-a, repo-b\nrepo-c")

	repos := configuredGithubPrimaryRepos()

	for _, name := range []string{"repo-a", "repo-b", "repo-c"} {
		if !repos[name] {
			t.Fatalf("expected %q in configured repo set", name)
		}
	}

	if repos["DankVault"] {
		t.Fatal("env configuration should override the built-in fallback set")
	}
}

func TestConfiguredGithubPrimaryReposReadsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "repos.txt")
	t.Setenv("FORGE_MIRROR_GITHUB_PRIMARY_REPOS_FILE", path)

	if err := os.WriteFile(path, []byte("repo-a\nrepo-b,repo-c"), 0o600); err != nil {
		t.Fatal(err)
	}

	repos := configuredGithubPrimaryRepos()

	for _, name := range []string{"repo-a", "repo-b", "repo-c"} {
		if !repos[name] {
			t.Fatalf("expected %q in configured repo set", name)
		}
	}
}
