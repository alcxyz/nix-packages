package main

import "testing"

func TestCloneRunsAsOperator(t *testing.T) {
	isolateGit(t)
	tests := []struct {
		args []string
		want string
	}{
		// repo clone only reads and may fetch a fork's parent, so it never
		// uses the App, whatever form its repository takes.
		{[]string{"repo", "clone", "https://github.com/acme/installed/tree/main"}, ""},
		{[]string{"repo", "clone", "https://github.com/acme/installed.git"}, ""},
		{[]string{"repo", "clone", "acme/installed"}, ""},
		// Other repo subcommands keep the strict parse.
		{[]string{"repo", "view", "https://github.com/acme/installed/tree/main"}, ""},
		{[]string{"repo", "view", "https://github.com/acme/installed"}, "github.com/acme/installed"},
	}
	for _, tt := range tests {
		ref, ok := resolveTarget(tt.args, func(string) string { return "" }, t.TempDir())
		got := ""
		if ok {
			got = ref.Host + "/" + ref.Owner + "/" + ref.Name
		}
		if got != tt.want {
			t.Errorf("%v: got %q, want %q", tt.args, got, tt.want)
		}
	}
}

func TestNameRulesOnlyOnGitHub(t *testing.T) {
	isolateGit(t)
	if ref, ok := parseRemoteURL("https://gitlab.example.test/my_group/app.git"); !ok || ref.Owner != "my_group" {
		t.Fatalf("other host names: %+v, %v", ref, ok)
	}
	if _, ok := parseRemoteURL("https://github.com/my_group/app.git"); ok {
		t.Fatal("github.com accepted an invalid owner")
	}

	// A GitLab fetch URL parses, so its GitHub push URL is not used.
	dir := gitRepo(t, [][2]string{{"origin", "https://gitlab.example.test/my_group/app.git"}}, nil)
	git(t, dir, "remote", "set-url", "--push", "origin", "git@github.com:acme/installed.git")
	if ref, ok := remoteTarget(dir); ok {
		t.Fatalf("push URL used: %+v", ref)
	}
}

func TestLastPushURLCounts(t *testing.T) {
	isolateGit(t)
	// A fetch URL that is not a repository URL falls back to the push URL,
	// and like gh the last push URL of the remote counts.
	dir := gitRepo(t, [][2]string{{"origin", "/srv/mirror.git"}}, nil)
	git(t, dir, "config", "--add", "remote.origin.pushurl", "https://github.com/someone/other.git")
	git(t, dir, "config", "--add", "remote.origin.pushurl", "https://github.com/acme/installed.git")
	if ref, ok := remoteTarget(dir); !ok || ref.Owner+"/"+ref.Name != "acme/installed" {
		t.Fatalf("last push URL: %+v, %v", ref, ok)
	}
}
