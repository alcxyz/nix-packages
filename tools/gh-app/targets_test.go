package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseInvocationFlagsBeforeCommand(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		command     string
		sub         string
		repo        string
		positionals string
	}{
		{"repo flag before command", []string{"-R", "acme/installed", "pr", "comment", "3", "-b", "hi"}, "pr", "comment", "acme/installed", "3"},
		{"value flag before subcommand", []string{"pr", "--body", "hello", "comment", "https://github.com/o/r/pull/1"}, "pr", "comment", "", "https://github.com/o/r/pull/1"},
		{"short value flag before subcommand", []string{"issue", "-t", "a title", "create"}, "issue", "create", "", ""},
		{"equals before subcommand", []string{"pr", "--body=x", "view", "4"}, "pr", "view", "", "4"},
		{"hostname before api", []string{"--hostname", "ghe.example.test", "api", "repos/o/r"}, "api", "", "", "repos/o/r"},
		{"repo equals before command", []string{"--repo=o/r", "issue", "list"}, "issue", "list", "o/r", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inv := parseInvocation(tt.args)
			if inv.command != tt.command || inv.sub != tt.sub || inv.repoFlag != tt.repo || strings.Join(inv.positionals, " ") != tt.positionals {
				t.Fatalf("got %+v", inv)
			}
		})
	}
}

func TestResolveTargetReviewCases(t *testing.T) {
	isolateGit(t)
	nonRepo := t.TempDir()
	tests := []struct {
		name string
		args []string
		env  map[string]string
		want string
	}{
		{"repo flag before command", []string{"-R", "acme/installed", "pr", "comment", "3", "-b", "hi"}, nil, "github.com/acme/installed"},
		{"body before subcommand", []string{"pr", "--body", "hello", "comment", "https://github.com/acme/installed/pull/3"}, nil, "github.com/acme/installed"},
		{"operator command after flag", []string{"-R", "acme/installed", "search", "issues", "x"}, nil, ""},
		{"later issue URL beats flag", []string{"issue", "edit", "1", "https://github.com/acme/installed/issues/2", "-R", "other/repo"}, nil, "github.com/acme/installed"},
		{"URLs of one repository", []string{"issue", "edit", "https://github.com/acme/installed/issues/1", "https://github.com/ACME/Installed/issues/2"}, nil, "github.com/acme/installed"},
		{"URLs of different repositories", []string{"issue", "edit", "https://github.com/acme/installed/issues/1", "https://github.com/other/repo/issues/2", "-R", "acme/installed"}, nil, ""},
		{"api absolute URL", []string{"api", "https://api.github.com/repos/o/r/pulls?state=open"}, nil, "github.com/o/r"},
		{"api absolute URL beats hostname", []string{"api", "--hostname", "ghe.example.test", "https://api.github.com/repos/o/r"}, nil, "github.com/o/r"},
		{"api enterprise URL", []string{"api", "https://ghe.example.test/api/v3/repos/o/r/issues"}, nil, "ghe.example.test/o/r"},
		{"api other absolute URL", []string{"api", "https://example.test/repos/o/r"}, nil, ""},
		{"api absolute non-repo URL", []string{"api", "https://api.github.com/user"}, nil, ""},
		{"repo view scp", []string{"repo", "view", "git@github.com:acme/installed.git"}, nil, "github.com/acme/installed"},
		{"repo view ssh URL", []string{"repo", "view", "ssh://git@github.com/acme/installed.git"}, nil, "github.com/acme/installed"},
		{"repo view https URL", []string{"repo", "view", "https://github.com/acme/installed"}, nil, "github.com/acme/installed"},
		{"repo view URL with port", []string{"repo", "view", "https://github.com:443/acme/installed.git"}, nil, "github.com/acme/installed"},
		{"repo view other host scp", []string{"repo", "view", "git@forge.example.test:acme/installed.git"}, nil, "forge.example.test/acme/installed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref, ok := resolveTarget(tt.args, func(key string) string { return tt.env[key] }, nonRepo)
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

func TestRemoteTargetEffectiveURLsAndOrder(t *testing.T) {
	isolateGit(t)

	dir := gitRepo(t, [][2]string{{"origin", "gh:o/r"}}, nil)
	git(t, dir, "config", "url.https://github.com/.insteadOf", "gh:")
	if ref, ok := remoteTarget(dir); !ok || ref.Owner+"/"+ref.Name != "o/r" {
		t.Fatalf("insteadOf remote: %+v, %v", ref, ok)
	}

	dir = gitRepo(t, [][2]string{
		{"alpha", "https://github.com/a/one.git"},
		{"origin", "https://github.com/o/r.git"},
	}, nil)
	if ref, ok := remoteTarget(dir); !ok || ref.Owner+"/"+ref.Name != "o/r" {
		t.Fatalf("origin before other remotes: %+v, %v", ref, ok)
	}

	dir = gitRepo(t, [][2]string{
		{"alpha", "https://github.com/a/one.git"},
		{"upstream", "https://github.com/u/p.git"},
	}, map[string]string{"alpha": "base"})
	if ref, ok := remoteTarget(dir); !ok || ref.Owner+"/"+ref.Name != "a/one" {
		t.Fatalf("gh-resolved base beats priority: %+v, %v", ref, ok)
	}

	// A partial clone's fetch line carries its filter as a fourth field.
	dir = gitRepo(t, [][2]string{{"origin", "https://github.com/o/r.git"}}, nil)
	git(t, dir, "config", "remote.origin.promisor", "true")
	git(t, dir, "config", "remote.origin.partialclonefilter", "blob:none")
	if ref, ok := remoteTarget(dir); !ok || ref.Owner+"/"+ref.Name != "o/r" {
		t.Fatalf("partial clone remote: %+v, %v", ref, ok)
	}
}

// later returns a minter whose clock runs d ahead.
func later(cfg config, d time.Duration) *minter {
	m := newMinter(cfg)
	m.now = func() time.Time { return time.Now().Add(d) }
	return m
}

func TestStaleInstallationList(t *testing.T) {
	isolateGit(t)
	fake, srv := newFakeGitHub(t, []string{testOwner + "/old"})
	cfg := testConfig(t, srv.URL)
	if _, err := newMinter(cfg).token(); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.repos = append(fake.repos, testOwner+"/new")
	fake.mu.Unlock()
	env := []string{"PATH=/bin"}
	args := []string{"pr", "list", "-R", testOwner + "/new"}

	// Within five minutes of the last listing the cached list decides.
	p, err := planGH(cfg, args, env, t.TempDir(), later(cfg, 4*time.Minute))
	if err != nil || p.withToken || fake.repoCalls != 1 {
		t.Fatalf("plan = %+v, %v, repository calls %d", p, err, fake.repoCalls)
	}

	// After that one new listing finds the repository, without a new token.
	p, err = planGH(cfg, args, env, t.TempDir(), later(cfg, 6*time.Minute))
	if err != nil || !p.withToken || fake.tokenCalls != 1 || fake.repoCalls != 2 {
		t.Fatalf("plan = %+v, %v, token calls %d, repository calls %d", p, err, fake.tokenCalls, fake.repoCalls)
	}
	entry, ok := newMinter(cfg).readCache()
	if !ok || !entry.installed(testOwner, "new") {
		t.Fatal("the cache was not updated")
	}

	// Still missing: the operator's login, and the next listing waits.
	absent := []string{"pr", "list", "-R", testOwner + "/absent"}
	for i := 0; i < 2; i++ {
		p, err = planGH(cfg, absent, env, t.TempDir(), later(cfg, 6*time.Minute))
		if err != nil || p.withToken || fake.repoCalls != 2 {
			t.Fatalf("plan = %+v, %v, repository calls %d", p, err, fake.repoCalls)
		}
	}
	p, err = planGH(cfg, absent, env, t.TempDir(), later(cfg, 12*time.Minute))
	if err != nil || p.withToken || fake.repoCalls != 3 {
		t.Fatalf("plan = %+v, %v, repository calls %d", p, err, fake.repoCalls)
	}

	// A failed listing falls back to the cached list.
	fake.mu.Lock()
	fake.failRepos = true
	fake.mu.Unlock()
	p, err = planGH(cfg, absent, env, t.TempDir(), later(cfg, 18*time.Minute))
	if err != nil || p.withToken || fake.repoCalls != 4 {
		t.Fatalf("failed listing: plan = %+v, %v, repository calls %d", p, err, fake.repoCalls)
	}
	p, err = planGH(cfg, args, env, t.TempDir(), later(cfg, 18*time.Minute))
	if err != nil || !p.withToken {
		t.Fatalf("failed listing, cached repository: plan = %+v, %v", p, err)
	}
	fake.mu.Lock()
	fake.failRepos = false
	fake.mu.Unlock()

	// The credential helper re-lists too.
	fake.mu.Lock()
	fake.repos = append(fake.repos, testOwner+"/newer")
	fake.mu.Unlock()
	var stdout, stderr bytes.Buffer
	m := later(cfg, 30*time.Minute)
	entry, installed, err := m.tokenFor(testOwner, "newer")
	if err != nil || !installed || entry.Token != testToken {
		t.Fatalf("tokenFor = %v, %v", installed, err)
	}
	runCredential(cfg, []string{"get"}, strings.NewReader(credentialRequest(testOwner+"/newer.git")), &stdout, &stderr)
	if !strings.HasPrefix(stdout.String(), "username=x-access-token\npassword="+testToken+"\n") || stderr.Len() != 0 {
		t.Fatalf("stdout = %q, stderr = %q", stdout.String(), stderr.String())
	}
}

func TestMintFailureWithCachedList(t *testing.T) {
	isolateGit(t)
	fake, srv := newFakeGitHub(t, []string{testOwner + "/installed"})
	cfg := testConfig(t, srv.URL)
	if _, err := newMinter(cfg).token(); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.failInstall = true
	fake.mu.Unlock()
	env := []string{"PATH=/bin"}

	// A token with less than ten minutes but more than one left still serves.
	p, err := planGH(cfg, []string{"pr", "list", "-R", testOwner + "/installed"}, env, t.TempDir(), later(cfg, 55*time.Minute))
	if err != nil || !p.withToken {
		t.Fatalf("usable cached token: plan = %+v, %v", p, err)
	}

	// With that token the list decides for a missing repository.
	p, err = planGH(cfg, []string{"pr", "list", "-R", testOwner + "/absent"}, env, t.TempDir(), later(cfg, 55*time.Minute))
	if err != nil || p.withToken {
		t.Fatalf("absent with usable cached token: plan = %+v, %v", p, err)
	}

	// Expired token: nothing proves a repository is not installed, so every
	// repository of the owner is refused.
	expired := later(cfg, 2*time.Hour)
	if _, err := planGH(cfg, []string{"pr", "list", "-R", testOwner + "/absent"}, env, t.TempDir(), expired); err == nil || !strings.Contains(err.Error(), "GH_AS_OPERATOR=1") {
		t.Fatalf("absent with expired token: %v", err)
	}
	if _, err := planGH(cfg, []string{"pr", "list", "-R", testOwner + "/installed"}, env, t.TempDir(), expired); err == nil {
		t.Fatal("installed repository without a token did not fail")
	}

	// No cache at all: refuse.
	fresh := testConfig(t, srv.URL)
	if _, err := planGH(fresh, []string{"pr", "list", "-R", testOwner + "/absent"}, env, t.TempDir(), newMinter(fresh)); err == nil {
		t.Fatal("first mint failure did not fail")
	}
}

func TestCredentialLFSPath(t *testing.T) {
	_, srv := newFakeGitHub(t, manyRepos())
	cfg := testConfig(t, srv.URL)
	var stdout, stderr bytes.Buffer
	runCredential(cfg, []string{"get"}, strings.NewReader(credentialRequest(testOwner+"/installed.git/info/lfs")), &stdout, &stderr)
	if !strings.HasPrefix(stdout.String(), "username=x-access-token\n") || stderr.Len() != 0 {
		t.Fatalf("stdout = %q, stderr = %q", stdout.String(), stderr.String())
	}
}

func TestRemoteDetailsLikeGH(t *testing.T) {
	isolateGit(t)
	for _, tc := range []struct{ raw, want string }{
		{"git@ssh.github.com:o/r.git", "github.com/o/r"},
		{"ssh://git@ssh.github.com:443/o/r.git", "github.com/o/r"},
		{"ssh://git@ssh.github.com/o/r", "github.com/o/r"},
	} {
		ref, ok := parseRemoteURL(tc.raw)
		if !ok || ref.Host+"/"+ref.Owner+"/"+ref.Name != tc.want {
			t.Fatalf("%s: %+v, %v", tc.raw, ref, ok)
		}
	}

	dir := gitRepo(t, [][2]string{
		{"alpha", "https://github.com/a/one.git"},
		{"Origin", "https://github.com/o/r.git"},
	}, nil)
	if ref, ok := remoteTarget(dir); !ok || ref.Owner != "o" {
		t.Fatalf("case-insensitive priority: %+v, %v", ref, ok)
	}

	// gh-resolved in worktree configuration counts.
	dir = gitRepo(t, [][2]string{{"origin", "https://github.com/o/r.git"}}, nil)
	git(t, dir, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-q", "-m", "init")
	git(t, dir, "config", "extensions.worktreeConfig", "true")
	wt := filepath.Join(t.TempDir(), "wt")
	git(t, dir, "worktree", "add", "-q", wt)
	git(t, wt, "config", "--worktree", "remote.origin.gh-resolved", "chosen/repo")
	if ref, ok := remoteTarget(wt); !ok || ref.Owner+"/"+ref.Name != "chosen/repo" {
		t.Fatalf("worktree gh-resolved: %+v, %v", ref, ok)
	}

	nonRepo := t.TempDir()
	getenv := func(key string) string {
		if key == "GH_REPO" {
			return "g/h"
		}
		return ""
	}
	for _, endpoint := range []string{"repos/:owner/:repo/pulls", "repos/acme/:repo", "repos/{owner}/:repo"} {
		ref, ok := resolveTarget([]string{"api", endpoint}, getenv, nonRepo)
		want := "g/h"
		if strings.HasPrefix(endpoint, "repos/acme") {
			want = "acme/h"
		}
		if !ok || ref.Owner+"/"+ref.Name != want {
			t.Fatalf("%s: %+v, %v", endpoint, ref, ok)
		}
	}
}

func TestCredentialHostWithPort(t *testing.T) {
	_, srv := newFakeGitHub(t, manyRepos())
	cfg := testConfig(t, srv.URL)
	var stdout, stderr bytes.Buffer
	input := "protocol=https\nhost=github.com:443\npath=" + testOwner + "/installed.git\n"
	runCredential(cfg, []string{"get"}, strings.NewReader(input), &stdout, &stderr)
	if !strings.HasPrefix(stdout.String(), "username=x-access-token\n") || stderr.Len() != 0 {
		t.Fatalf("stdout = %q, stderr = %q", stdout.String(), stderr.String())
	}
	stdout.Reset()
	input = "protocol=https\nhost=github.com:8443\npath=" + testOwner + "/installed.git\n"
	runCredential(cfg, []string{"get"}, strings.NewReader(input), &stdout, &stderr)
	if stdout.Len() != 0 {
		t.Fatalf("other port answered: %q", stdout.String())
	}
}
