package main

import (
	"strings"
	"testing"
)

func TestParseInvocation(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		sub         string
		repo        string
		positionals string
	}{
		{"flag values are not positional", []string{"pr", "comment", "-b", "https://github.com/a/b/pull/1", "7"}, "comment", "", "7"},
		{"long value flag", []string{"issue", "create", "--title", "a/b", "--body", "c/d"}, "create", "", ""},
		{"equals never takes the next", []string{"pr", "comment", "--body=x", "https://github.com/o/r/pull/2"}, "comment", "", "https://github.com/o/r/pull/2"},
		{"boolean flag before positional", []string{"pr", "merge", "-s", "https://github.com/o/r/pull/3"}, "merge", "", "https://github.com/o/r/pull/3"},
		{"same letter takes a value elsewhere", []string{"pr", "list", "-s", "open", "-L", "5"}, "list", "", ""},
		{"double dash ends flags", []string{"pr", "view", "--", "-R"}, "view", "", "-R"},
		{"repo flag before subcommand", []string{"pr", "-R", "o/r", "list"}, "list", "o/r", ""},
		{"attached repo flag", []string{"pr", "list", "-Ro/r"}, "list", "o/r", ""},
		{"short cluster with value", []string{"run", "view", "-vj", "5"}, "view", "", ""},
		{"alias", []string{"pr", "co", "-b", "topic", "3"}, "checkout", "", "3"},
		{"api endpoint after flags", []string{"api", "-X", "PATCH", "-H", "Accept: x", "repos/o/r", "-f", "a=b"}, "", "", "repos/o/r"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inv := parseInvocation(tt.args)
			if inv.sub != tt.sub || inv.repoFlag != tt.repo || strings.Join(inv.positionals, " ") != tt.positionals {
				t.Fatalf("got sub %q repo %q positionals %q", inv.sub, inv.repoFlag, inv.positionals)
			}
		})
	}
}

func TestResolveTarget(t *testing.T) {
	isolateGit(t)
	nonRepo := t.TempDir()
	tests := []struct {
		name string
		args []string
		env  map[string]string
		want string // HOST/OWNER/REPO, or "" for no target
	}{
		{"short flag", []string{"pr", "list", "-R", "o/r"}, nil, "github.com/o/r"},
		{"long flag", []string{"pr", "list", "--repo", "o/r"}, nil, "github.com/o/r"},
		{"long flag equals", []string{"pr", "list", "--repo=o/r"}, nil, "github.com/o/r"},
		{"attached short flag", []string{"pr", "list", "-Ro/r"}, nil, "github.com/o/r"},
		{"flag with host", []string{"issue", "list", "-R", "github.com/o/r"}, nil, "github.com/o/r"},
		{"flag with other host", []string{"issue", "list", "-R", "ghe.example.test/o/r"}, nil, "ghe.example.test/o/r"},
		{"flag with URL", []string{"issue", "list", "-R", "https://github.com/o/r.git"}, nil, "github.com/o/r"},
		{"pr URL beats flag", []string{"pr", "view", "https://github.com/a/b/pull/1", "-R", "o/r"}, nil, "github.com/a/b"},
		{"issue URL beats flag", []string{"issue", "close", "-R", "o/r", "https://github.com/a/b/issues/2"}, nil, "github.com/a/b"},
		{"URL argument", []string{"pr", "view", "https://github.com/o/r/pull/3"}, nil, "github.com/o/r"},
		{"URL on another host", []string{"pr", "view", "https://ghe.example.test/o/r/pull/3"}, nil, "ghe.example.test/o/r"},
		{"URL in a flag value", []string{"pr", "comment", "7", "-b", "https://github.com/o/r/pull/3"}, map[string]string{"GH_REPO": "g/h"}, "github.com/g/h"},
		{"URL after boolean flag", []string{"pr", "merge", "-s", "https://github.com/o/r/pull/3"}, map[string]string{"GH_REPO": "g/h"}, "github.com/o/r"},
		{"URL not the selector", []string{"release", "create", "v1", "https://github.com/o/r"}, map[string]string{"GH_REPO": "g/h"}, "github.com/g/h"},
		{"URL of repo", []string{"repo", "view", "https://github.com/o/r"}, nil, "github.com/o/r"},
		{"api repos", []string{"api", "repos/o/r/pulls"}, nil, "github.com/o/r"},
		{"api slash repos", []string{"api", "/repos/o/r"}, nil, "github.com/o/r"},
		{"api repos query", []string{"api", "repos/o/r?per_page=1"}, nil, "github.com/o/r"},
		{"api flags first", []string{"api", "-X", "PATCH", "-H", "Accept: x", "repos/o/r/issues/1", "-f", "state=closed"}, nil, "github.com/o/r"},
		{"api other host", []string{"api", "--hostname", "ghe.example.test", "repos/o/r"}, nil, "ghe.example.test/o/r"},
		{"api github hostname", []string{"api", "--hostname=github.com", "repos/o/r"}, map[string]string{"GH_HOST": "ghe.example.test"}, "github.com/o/r"},
		{"api GH_HOST", []string{"api", "repos/o/r"}, map[string]string{"GH_HOST": "ghe.example.test"}, "ghe.example.test/o/r"},
		{"api user has no target", []string{"api", "user"}, map[string]string{"GH_REPO": "g/h"}, ""},
		{"api placeholders use GH_REPO", []string{"api", "repos/{owner}/{repo}/pulls"}, map[string]string{"GH_REPO": "g/h"}, "github.com/g/h"},
		{"api repo placeholder only", []string{"api", "repos/acme/{repo}"}, map[string]string{"GH_REPO": "other/repo"}, "github.com/acme/repo"},
		{"api placeholders without base", []string{"api", "repos/{owner}/{repo}"}, nil, ""},
		{"api graphql", []string{"api", "graphql", "-f", "query=repos/o/r"}, nil, ""},
		{"api repos prefix only", []string{"api", "reposx/o/r"}, nil, ""},
		{"repo clone", []string{"repo", "clone", "o/r", "dir"}, nil, ""},
		{"repo clone bare name", []string{"repo", "clone", "r"}, map[string]string{"GH_REPO": "g/h"}, ""},
		{"repo view flag first", []string{"repo", "view", "--web", "o/r"}, nil, "github.com/o/r"},
		{"repo view branch value", []string{"repo", "view", "-b", "a/b", "o/r"}, nil, "github.com/o/r"},
		{"repo flag beats positional", []string{"repo", "view", "o/r", "-R", "g/h"}, nil, "github.com/g/h"},
		{"repo set-default", []string{"repo", "set-default", "o/r"}, nil, "github.com/o/r"},
		{"repo unarchive", []string{"repo", "unarchive", "o/r", "--yes"}, nil, "github.com/o/r"},
		{"repo rename uses base", []string{"repo", "rename", "new/name"}, map[string]string{"GH_REPO": "g/h"}, "github.com/g/h"},
		{"repo deploy-key", []string{"repo", "deploy-key", "list"}, map[string]string{"GH_REPO": "g/h"}, "github.com/g/h"},
		{"repo list", []string{"repo", "list", "o"}, map[string]string{"GH_REPO": "g/h"}, ""},
		{"repo create", []string{"repo", "create", "o/r"}, map[string]string{"GH_REPO": "g/h"}, ""},
		{"repo fork", []string{"repo", "fork", "o/r"}, nil, ""},
		{"GH_REPO", []string{"pr", "list"}, map[string]string{"GH_REPO": "o/r"}, "github.com/o/r"},
		{"GH_REPO with host", []string{"pr", "list"}, map[string]string{"GH_REPO": "ghe.example.test/o/r"}, "ghe.example.test/o/r"},
		{"GH_REPO on GH_HOST", []string{"pr", "list"}, map[string]string{"GH_REPO": "o/r", "GH_HOST": "ghe.example.test"}, "ghe.example.test/o/r"},
		{"run family", []string{"run", "list", "-w", "ci.yml"}, map[string]string{"GH_REPO": "o/r"}, "github.com/o/r"},
		{"auth status", []string{"auth", "status"}, map[string]string{"GH_REPO": "o/r"}, ""},
		{"search", []string{"search", "repos", "-R", "o/r"}, map[string]string{"GH_REPO": "o/r"}, ""},
		{"gist", []string{"gist", "list"}, map[string]string{"GH_REPO": "o/r"}, ""},
		{"secret", []string{"secret", "list", "-R", "o/r"}, nil, ""},
		{"browse", []string{"browse", "-R", "o/r"}, nil, ""},
		{"unknown command", []string{"frobnicate", "-R", "o/r"}, nil, ""},
		{"no command", []string{"-R", "o/r"}, nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getenv := func(key string) string { return tt.env[key] }
			ref, ok := resolveTarget(tt.args, getenv, nonRepo)
			got := ""
			if ok {
				got = ref.Host + "/" + ref.Owner + "/" + ref.Name
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveTargetRemotesAndHosts(t *testing.T) {
	isolateGit(t)
	dir := gitRepo(t, [][2]string{{"origin", "git@github.com:o/r.git"}}, nil)
	tests := []struct {
		name string
		args []string
		env  map[string]string
		want string
	}{
		{"remotes", []string{"pr", "list"}, nil, "github.com/o/r"},
		{"api placeholders from remotes", []string{"api", "repos/{owner}/{repo}/issues"}, nil, "github.com/o/r"},
		{"GH_HOST ignores github remotes", []string{"pr", "list"}, map[string]string{"GH_HOST": "ghe.example.test"}, ""},
		{"GH_HOST github.com", []string{"pr", "list"}, map[string]string{"GH_HOST": "GitHub.com"}, "github.com/o/r"},
		{"repo list in a checkout", []string{"repo", "list"}, nil, ""},
		{"auth status in a checkout", []string{"auth", "status"}, nil, ""},
		{"search in a checkout", []string{"search", "issues", "x"}, nil, ""},
		{"api user in a checkout", []string{"api", "user"}, nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref, ok := resolveTarget(tt.args, func(key string) string { return tt.env[key] }, dir)
			got := ""
			if ok {
				got = ref.Host + "/" + ref.Owner + "/" + ref.Name
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}
