package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCrossRepositoryCommands(t *testing.T) {
	isolateGit(t)
	nonRepo := t.TempDir()
	env := map[string]string{"GH_REPO": "acme/installed"}
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"issue transfer", []string{"issue", "transfer", "3", "acme/other"}, ""},
		{"label clone", []string{"label", "clone", "acme/other"}, ""},
		{"repo sync source", []string{"repo", "sync", "acme/installed", "--source", "acme/other"}, ""},
		{"repo sync short source", []string{"repo", "sync", "-s", "acme/other"}, ""},
		{"repo sync", []string{"repo", "sync", "acme/installed"}, "acme/installed"},
		{"local repo sync", []string{"repo", "sync"}, ""},
		{"issue develop branch repo", []string{"issue", "develop", "3", "--branch-repo", "acme/other"}, ""},
		{"issue develop", []string{"issue", "develop", "3", "--name", "topic"}, "acme/installed"},
		{"issue develop issue repo and -R", []string{"issue", "develop", "3", "-i", "acme/installed", "-R", "acme/other"}, ""},
		{"issue develop issue repo alone", []string{"issue", "develop", "3", "-i", "acme/installed"}, "acme/installed"},
		{"repo clone with git arguments", []string{"repo", "clone", "acme/installed", "--", "--recurse-submodules"}, ""},
		{"repo clone", []string{"repo", "clone", "acme/installed"}, ""},
		{"pr checkout submodules", []string{"pr", "checkout", "3", "--recurse-submodules"}, ""},
		{"pr checkout submodules true", []string{"pr", "checkout", "3", "--recurse-submodules=true"}, ""},
		{"pr checkout submodules false", []string{"pr", "checkout", "3", "--recurse-submodules=false"}, ""},
		{"pr status", []string{"pr", "status"}, ""},
		{"issue status", []string{"issue", "status"}, ""},
		{"author @me", []string{"pr", "list", "--author", "@me"}, ""},
		{"search @me", []string{"issue", "list", "--search", "assignee:@me"}, ""},
		{"body mentioning @me text", []string{"issue", "comment", "1", "--body", "Contact dev@me.example"}, "acme/installed"},
		{"assignee short flag @me", []string{"issue", "list", "-a", "@me"}, ""},
		{"quoted @me", []string{"issue", "edit", "1", "--add-assignee", `"@me"`}, ""},
		{"label search @me", []string{"label", "list", "--search", "@me"}, "acme/installed"},
		{"pr update-branch", []string{"pr", "update-branch", "3"}, ""},
		{"unknown subcommand (alias)", []string{"issue", "external"}, ""},
		{"known subcommand without value flags", []string{"pr", "ready", "3"}, "acme/installed"},
		{"issue close duplicate elsewhere", []string{"issue", "close", "1", "--duplicate-of", "https://github.com/acme/other/issues/2"}, ""},
		{"issue close duplicate here", []string{"issue", "close", "1", "--duplicate-of", "2"}, "acme/installed"},
		{"issue edit repeated sub-issue", []string{"issue", "edit", "1", "--add-sub-issue", "https://github.com/acme/other/issues/2", "--add-sub-issue", "4"}, ""},
		{"pr checkout", []string{"pr", "checkout", "3"}, ""},
		{"issue edit parent elsewhere", []string{"issue", "edit", "1", "--parent", "https://github.com/acme/other/issues/2"}, ""},
		{"issue edit sub-issue number", []string{"issue", "edit", "1", "--add-sub-issue", "4"}, "acme/installed"},
		{"issue create blocked by elsewhere", []string{"issue", "create", "-t", "x", "--blocked-by", "acme/other#3"}, ""},
		{"issue develop empty issue repo and -R", []string{"issue", "develop", "3", "--issue-repo=", "-R", "acme/installed"}, ""},
		{"pr create fork head", []string{"pr", "create", "--head", "someone:topic"}, ""},
		{"pr create same owner head", []string{"pr", "create", "-H", "ACME:topic"}, "acme/installed"},
		{"pr create plain head", []string{"pr", "create", "-H", "topic"}, "acme/installed"},
		{"pr create other repo head", []string{"pr", "create", "--head=acme/other:topic"}, ""},
		{"pr create same repo head", []string{"pr", "create", "--head", "acme/installed:topic"}, "acme/installed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref, ok := resolveTarget(tt.args, func(key string) string { return env[key] }, nonRepo)
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

func TestWWWGitHub(t *testing.T) {
	for _, raw := range []string{"https://www.github.com/o/r.git", "git@www.github.com:o/r.git", "ssh://git@WWW.GitHub.com/o/r"} {
		ref, ok := parseRemoteURL(raw)
		if !ok || ref.Host != githubHost {
			t.Fatalf("%s: %+v, %v", raw, ref, ok)
		}
	}
	ref, ok := resolveTarget([]string{"pr", "view", "https://www.github.com/o/r/pull/1"}, func(string) string { return "" }, t.TempDir())
	if !ok || ref.Host != githubHost {
		t.Fatalf("URL selector: %+v, %v", ref, ok)
	}
}

// Concurrent invocations waiting for the lock take the listing a first one
// wrote, even when the cached token has less than ten minutes left.
func TestConcurrentRelistOnce(t *testing.T) {
	fake, srv := newFakeGitHub(t, []string{testOwner + "/one"})
	cfg := testConfig(t, srv.URL)
	m := newMinter(cfg)
	entry, err := m.token()
	if err != nil {
		t.Fatal(err)
	}
	// A token with five minutes left, listed long ago, and minting fails.
	entry.ExpiresAt = time.Now().Add(5 * time.Minute)
	entry.ListedAt = time.Now().Add(-time.Hour)
	entry.ListTriedAt = time.Time{}
	if err := m.writeCache(entry); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.failInstall = true
	before := fake.repoCalls
	fake.mu.Unlock()

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, installed, err := newMinter(cfg).tokenFor(testOwner, "absent")
			if err == nil && installed {
				t.Error("absent repository reported as installed")
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.repoCalls-before != 1 {
		t.Fatalf("listed %d times, want 1", fake.repoCalls-before)
	}
}

func TestGitCommandsWithSubmodules(t *testing.T) {
	isolateGit(t)
	dir := gitRepo(t, [][2]string{{"origin", "https://github.com/acme/installed.git"}}, nil)
	env := func(string) string { return "" }
	create := []string{"pr", "create", "-t", "x"}
	if ref, ok := resolveTarget(create, env, dir); !ok || ref.Owner+"/"+ref.Name != "acme/installed" {
		t.Fatalf("without submodules: %+v, %v", ref, ok)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitmodules"), []byte("[submodule \"x\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	// gh's Git may fetch or push the submodules with the token, so the
	// operator runs these, from any directory of the checkout.
	for _, at := range []string{dir, sub} {
		for _, args := range [][]string{create, {"issue", "develop", "3", "--checkout"}} {
			if ref, ok := resolveTarget(args, env, at); ok {
				t.Fatalf("%v with submodules from %s: %+v", args, at, ref)
			}
		}
	}

	// Initialised submodules count even when .gitmodules is gone locally.
	if err := os.Remove(filepath.Join(dir, ".gitmodules")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".git", "modules", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if ref, ok := resolveTarget(create, env, dir); ok {
		t.Fatalf("with initialised submodules: %+v", ref)
	}
}

func TestSubmodulesInLinkedWorktree(t *testing.T) {
	isolateGit(t)
	dir := gitRepo(t, [][2]string{{"origin", "https://github.com/acme/installed.git"}}, nil)
	git(t, dir, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "-q", "--allow-empty", "-m", "init")
	worktree := filepath.Join(t.TempDir(), "w")
	git(t, dir, "worktree", "add", "-q", worktree, "-b", "w")
	if hasSubmodules(worktree) {
		t.Fatal("a worktree without submodules counts as having them")
	}
	// A linked worktree keeps its submodule repositories under its own Git
	// directory.
	if err := os.MkdirAll(filepath.Join(dir, ".git", "worktrees", "w", "modules", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !hasSubmodules(worktree) {
		t.Fatal("submodules under the worktree's Git directory are missed")
	}
}

func TestFlagsBeforeTheSubcommandRefused(t *testing.T) {
	isolateGit(t)
	_, srv := newFakeGitHub(t, manyRepos())
	cfg := testConfig(t, srv.URL)
	env := []string{"PATH=/bin"}
	refused := [][]string{
		{"--body", "hello", "pr", "comment", "3", "-R", testOwner + "/installed"},
		{"-b", "hello", "pr", "comment", "3", "-R", testOwner + "/installed"},
		{"-aR" + testOwner + "/installed", "pr", "review", "3"},
		{"pr", "-A", "@me", "list"},
		{"pr", "--body", "x", "comment", "3"},
		{"-h", "pr", "comment", "3", "-R", testOwner + "/installed"},
	}
	for _, args := range refused {
		if _, err := planGH(cfg, args, env, t.TempDir(), newMinter(cfg)); err == nil || !strings.Contains(err.Error(), "after the subcommand") {
			t.Fatalf("%v: err = %v", args, err)
		}
	}
	allowed := [][]string{
		{"-R", testOwner + "/installed", "pr", "list"},
		{"--repo=" + testOwner + "/installed", "issue", "list"},
		{"pr", "-R" + testOwner + "/installed", "list"},
		{"api", "-X", "GET", "repos/" + testOwner + "/installed"},
		{"--version"},
		{"search", "issues", "--json", "x"},
		{"status", "--org", "x"},
		{"browse", "--no-browser", "-R", testOwner + "/installed"},
		// Commands the App never handles keep their flags wherever gh takes them.
		{"copilot", "-p", "summarize"},
		{"gist", "--help"},
		{"extension-x", "--flag", "v", "sub"},
	}
	for _, args := range allowed {
		if _, err := planGH(cfg, args, env, t.TempDir(), newMinter(cfg)); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	// As the operator, gh runs whatever it is given.
	if _, err := planGH(cfg, refused[0], append(env, "GH_AS_OPERATOR=1"), t.TempDir(), newMinter(cfg)); err != nil {
		t.Fatal(err)
	}
}

func TestAPIPlaceholdersHonourGHResolved(t *testing.T) {
	isolateGit(t)
	// gh 2.96 builds api on the same repository-resolving factory as pr and
	// issue (pkg/cmd/root/root.go), so gh-resolved counts for placeholders.
	dir := gitRepo(t, [][2]string{
		{"origin", "https://github.com/acme/installed.git"},
		{"upstream", "https://github.com/someone/upstream.git"},
	}, map[string]string{"origin": "base"})
	env := func(string) string { return "" }
	ref, ok := resolveTarget([]string{"api", "-X", "POST", "repos/{owner}/{repo}/issues"}, env, dir)
	if !ok || ref.Owner+"/"+ref.Name != "acme/installed" {
		t.Fatalf("api placeholders: %+v, %v", ref, ok)
	}
}

func TestDeployKeyChildren(t *testing.T) {
	isolateGit(t)
	env := func(key string) string {
		if key == "GH_REPO" {
			return "acme/installed"
		}
		return ""
	}
	if ref, ok := resolveTarget([]string{"repo", "deploy-key", "list"}, env, t.TempDir()); !ok || ref.Owner+"/"+ref.Name != "acme/installed" {
		t.Fatalf("deploy-key list: %+v, %v", ref, ok)
	}
	for _, args := range [][]string{
		{"repo", "deploy-key", "external"},
		{"repo", "deploy-key", "--search", "list", "external"},
		{"repo", "deploy-key", "-w", "add", "external"},
	} {
		if ref, ok := resolveTarget(args, env, t.TempDir()); ok {
			t.Fatalf("%v: %+v", args, ref)
		}
	}
}
