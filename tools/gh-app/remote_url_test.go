package main

import "testing"

func TestParseRemoteURLLikeGH(t *testing.T) {
	tests := []struct {
		raw  string
		want string // HOST/OWNER/REPO, or "" when it does not parse
	}{
		{"https://github.com/o/r", "github.com/o/r"},
		{"https://github.com/o/r.git/", "github.com/o/r"},
		{"https://github.com/o/r/", "github.com/o/r"},
		{"https://github.com/o/r/extra", ""},
		{"https://github.com/o/r/pull/3", ""},
		{"https://github.com/o", ""},
		{"git@github.com:o/r/extra.git", ""},
		{"git://forge.example.test/o/r.git", "forge.example.test/o/r"},
		{"git://github.com/o/r.git", "github.com/o/r"},
		{"git+https://github.com/o/r.git", "github.com/o/r"},
		{"git+ssh://git@github.com/o/r.git", "github.com/o/r"},
		{"ssh+git://git@github.com/o/r.git", ""},
		{"file:///srv/o/r.git", ""},
		{"ftp://github.com/o/r.git", "github.com/o/r"},
	}
	for _, tt := range tests {
		ref, ok := parseRemoteURL(tt.raw)
		got := ""
		if ok {
			got = ref.Host + "/" + ref.Owner + "/" + ref.Name
		}
		if got != tt.want {
			t.Errorf("%s: got %q, want %q", tt.raw, got, tt.want)
		}
	}

	// Selector URLs stay loose: a pull request URL names its repository.
	if ref, ok := parseRepoURL("https://github.com/o/r/pull/3"); !ok || ref.Owner+"/"+ref.Name != "o/r" {
		t.Fatalf("selector URL: %+v, %v", ref, ok)
	}
}

func TestRemoteURLStrictnessAndPushFallback(t *testing.T) {
	isolateGit(t)
	tests := []struct {
		name  string
		fetch string
		push  string
		want  string
	}{
		{"long fetch path falls back", "https://github.com/acme/mirror/extra.git", "git@github.com:acme/installed.git", "acme/installed"},
		{"git scheme on another host is filtered", "git://forge.example.test/acme/installed.git", "git@github.com:acme/installed.git", ""},
		{"git scheme on github", "git://github.com/acme/installed.git", "", "acme/installed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := gitRepo(t, [][2]string{{"origin", tt.fetch}}, nil)
			if tt.push != "" {
				git(t, dir, "remote", "set-url", "--push", "origin", tt.push)
			}
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
