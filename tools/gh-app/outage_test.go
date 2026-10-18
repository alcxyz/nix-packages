package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// cacheAt rewrites the cached entry's expiry and listing time.
func cacheAt(t *testing.T, cfg config, expires, listed time.Time) {
	t.Helper()
	m := newMinter(cfg)
	entry, ok := m.readCache()
	if !ok {
		t.Fatal("no cache")
	}
	entry.ExpiresAt, entry.ListedAt, entry.ListTriedAt = expires, listed, time.Time{}
	if err := m.writeCache(entry); err != nil {
		t.Fatal(err)
	}
}

func TestMintFailureRefusesOwnerRepositories(t *testing.T) {
	isolateGit(t)
	fake, srv := newFakeGitHub(t, []string{testOwner + "/installed"})
	cfg := testConfig(t, srv.URL)
	if _, err := newMinter(cfg).token(); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.failInstall = true
	fake.failRepos = true
	fake.mu.Unlock()
	now := time.Now()
	credential := func(path string) string {
		var stdout, stderr bytes.Buffer
		runCredential(cfg, []string{"get"}, strings.NewReader(credentialRequest(path)), &stdout, &stderr)
		return stdout.String()
	}
	plan := func(repo string) (ghPlan, error) {
		return planGH(cfg, []string{"pr", "list", "-R", testOwner + "/" + repo}, []string{"PATH=/bin"}, t.TempDir(), newMinter(cfg))
	}

	// No usable token: a repository missing from any list, young or old, is
	// refused in gh and in Git.
	for _, listed := range []time.Time{now.Add(-10 * time.Minute), now.Add(-2 * time.Hour)} {
		cacheAt(t, cfg, now.Add(-time.Minute), listed)
		if _, err := plan("absent"); err == nil || !strings.Contains(err.Error(), "GH_AS_OPERATOR=1") {
			t.Fatalf("listed %v: %v", listed, err)
		}
		if got := credential(testOwner + "/absent.git"); got != "quit=1\n" {
			t.Fatalf("listed %v credential: %q", listed, got)
		}
	}
	// Another owner's repository is not affected.
	if got := credential("someone/else.git"); got != "" {
		t.Fatalf("other owner credential: %q", got)
	}

	// A still-valid cached token: the cached list decides, even when it is
	// old and listing again fails.
	cacheAt(t, cfg, now.Add(50*time.Minute), now.Add(-2*time.Hour))
	for i := 0; i < 2; i++ {
		if p, err := plan("absent"); err != nil || p.withToken {
			t.Fatalf("valid token, attempt %d: %+v, %v", i, p, err)
		}
	}
	if p, err := plan("installed"); err != nil || !p.withToken {
		t.Fatalf("valid token, installed: %+v, %v", p, err)
	}
	entry, _ := newMinter(cfg).readCache()
	if !entry.ListedAt.Before(now.Add(-time.Hour)) || entry.ListTriedAt.Before(now) {
		t.Fatalf("a failed listing changed listed_at: %+v", entry)
	}
}

func TestRepoCommandsGHRepoAndCheckout(t *testing.T) {
	isolateGit(t)
	dir := gitRepo(t, [][2]string{{"origin", "git@github.com:acme/installed.git"}}, nil)
	// An unresolvable upstream could outrank origin.
	openDir := gitRepo(t, [][2]string{{"origin", "git@github.com:acme/installed.git"}, {"upstream", "git@broken:acme/installed.git"}}, nil)
	tests := []struct {
		name   string
		args   []string
		ghRepo string
		dir    string
		want   string
	}{
		{"agree", []string{"repo", "edit", "--description", "x"}, "acme/installed", dir, "acme/installed"},
		{"GH_REPO ignored", []string{"repo", "edit", "--description", "x"}, "acme/other", dir, "acme/installed"},
		{"only GH_REPO", []string{"repo", "edit"}, "acme/other", t.TempDir(), ""},
		{"view ignores GH_REPO", []string{"repo", "view"}, "acme/other", dir, "acme/installed"},
		{"only checkout", []string{"repo", "edit"}, "", dir, "acme/installed"},
		{"positional decides", []string{"repo", "edit", "acme/other"}, "acme/other", dir, "acme/other"},
		{"other families keep GH_REPO", []string{"pr", "list"}, "acme/other", dir, "acme/other"},
		{"rename honours GH_REPO", []string{"repo", "rename", "new-name"}, "acme/other", dir, "acme/other"},
		{"deploy-key honours GH_REPO", []string{"repo", "deploy-key", "list"}, "acme/other", dir, "acme/other"},
		{"open checkout", []string{"repo", "view"}, "acme/installed", openDir, ""},
		{"open checkout without GH_REPO", []string{"repo", "view"}, "", openDir, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getenv := func(key string) string {
				if key == "GH_REPO" {
					return tt.ghRepo
				}
				return ""
			}
			ref, ok := resolveTarget(tt.args, getenv, tt.dir)
			got := ""
			if ok {
				got = ref.Owner + "/" + ref.Name
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRemotePushURLFallback(t *testing.T) {
	isolateGit(t)
	tests := []struct {
		name  string
		fetch string
		push  string
		want  string
	}{
		{"unparsable fetch", "/srv/mirror.git", "git@github.com:acme/installed.git", "acme/installed"},
		{"non-github fetch keeps its host", "https://forge.example.test/acme/installed.git", "git@github.com:acme/installed.git", ""},
		{"unparsable fetch, unknown push host", "/srv/mirror.git", "git@broken:acme/installed.git", ""},
		{"github fetch wins", "git@github.com:acme/installed.git", "git@github.com:acme/other.git", "acme/installed"},
		{"neither", "/srv/mirror.git", "https://forge.example.test/acme/installed.git", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := gitRepo(t, [][2]string{{"origin", tt.fetch}}, nil)
			git(t, dir, "remote", "set-url", "--push", "origin", tt.push)
			ref, ok := remoteTarget(dir)
			got := ""
			if ok {
				got = ref.Owner + "/" + ref.Name
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}
