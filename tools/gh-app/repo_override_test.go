package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestEmptyRepoFlagAndIssueRepo(t *testing.T) {
	isolateGit(t)
	dir := gitRepo(t, [][2]string{{"origin", "git@github.com:acme/checkout.git"}}, nil)
	tests := []struct {
		name   string
		args   []string
		ghRepo string
		want   string
	}{
		{"empty equals falls back to GH_REPO", []string{"pr", "list", "--repo="}, "acme/env", "acme/env"},
		{"empty short falls back to checkout", []string{"pr", "list", "-R", ""}, "", "acme/checkout"},
		{"last empty wins", []string{"pr", "list", "-R", "acme/flag", "--repo="}, "", "acme/checkout"},
		{"issue-repo", []string{"issue", "develop", "3", "--issue-repo", "acme/issues"}, "acme/env", "acme/issues"},
		{"issue-repo with -R makes -R the branch repository", []string{"issue", "develop", "3", "-R", "acme/flag", "-i", "acme/issues"}, "", ""},
		{"issue-repo with branch-repo", []string{"issue", "develop", "3", "-i", "acme/issues", "--branch-repo", "acme/other"}, "", ""},
		{"empty issue-repo", []string{"issue", "develop", "3", "--issue-repo="}, "acme/env", "acme/env"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getenv := func(key string) string {
				if key == "GH_REPO" {
					return tt.ghRepo
				}
				return ""
			}
			ref, ok := resolveTarget(tt.args, getenv, dir)
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

func TestListingFailureKeepsNewToken(t *testing.T) {
	isolateGit(t)
	fake, srv := newFakeGitHub(t, []string{testOwner + "/installed"})
	fake.failRepos = true
	cfg := testConfig(t, srv.URL)
	plan := func(repo string) error {
		_, err := planGH(cfg, []string{"pr", "list", "-R", testOwner + "/" + repo}, []string{"PATH=/bin"}, t.TempDir(), newMinter(cfg))
		return err
	}

	// Minting works, listing fails: refused, but the token is cached.
	if err := plan("installed"); err == nil || !strings.Contains(err.Error(), "GH_AS_OPERATOR=1") {
		t.Fatalf("first: %v", err)
	}
	entry, ok := newMinter(cfg).readCache()
	if !ok || entry.Token != testToken || !entry.ListedAt.IsZero() || entry.ListTriedAt.IsZero() {
		t.Fatalf("cache = %+v, %v", entry, ok)
	}
	// Retries do not mint again and still refuse without a list.
	for _, repo := range []string{"installed", "absent"} {
		if err := plan(repo); err == nil {
			t.Fatalf("%s without a list: no error", repo)
		}
	}
	var stdout, stderr bytes.Buffer
	runCredential(cfg, []string{"get"}, strings.NewReader(credentialRequest(testOwner+"/absent.git")), &stdout, &stderr)
	if stdout.String() != "quit=1\n" {
		t.Fatalf("credential: %q", stdout.String())
	}
	if code := runStatus(cfg, &stdout, &stderr); code == 0 {
		t.Fatal("status succeeded without a list")
	}
	if fake.tokenCalls != 1 {
		t.Fatalf("token calls %d, want 1", fake.tokenCalls)
	}

	// Once listing works again, after the re-listing interval, it decides.
	fake.mu.Lock()
	fake.failRepos = false
	fake.mu.Unlock()
	p, err := planGH(cfg, []string{"pr", "list", "-R", testOwner + "/installed"}, []string{"PATH=/bin"}, t.TempDir(), later(cfg, 6*time.Minute))
	if err != nil || !p.withToken || fake.tokenCalls != 1 {
		t.Fatalf("recovered: %+v, %v, token calls %d", p, err, fake.tokenCalls)
	}
}

func TestOrganizationInstallationFallback(t *testing.T) {
	fake, srv := newFakeGitHub(t, []string{testOwner + "/installed"})
	fake.orgOnly = true
	entry, err := newMinter(testConfig(t, srv.URL)).token()
	if err != nil || entry.InstallationID != 777 {
		t.Fatalf("entry = %+v, %v", entry, err)
	}
	if fake.installCalls != 1 || fake.orgCalls != 1 {
		t.Fatalf("user calls %d, org calls %d", fake.installCalls, fake.orgCalls)
	}

	// Both 404: the error names the installation lookup.
	fake.mu.Lock()
	fake.orgOnly = false
	fake.failInstall = true
	fake.mu.Unlock()
	if _, err := newMinter(testConfig(t, srv.URL)).token(); err == nil || !strings.Contains(err.Error(), "/orgs/"+testOwner+"/installation") {
		t.Fatalf("error = %v", err)
	}
}
