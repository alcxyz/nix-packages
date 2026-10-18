package main

import (
	"testing"
)

func TestFlagAliasesLastWins(t *testing.T) {
	tests := []struct {
		name string
		args []string
		flag string
		want string
	}{
		{"long then short", []string{"pr", "create", "--head", "a:x", "-H", "b:y"}, "--head", "b:y"},
		{"short then long", []string{"pr", "create", "-H", "a:x", "--head=b:y"}, "--head", "b:y"},
		{"repo spellings", []string{"-R", "a/b", "pr", "list", "--repo", "c/d", "-Re/f"}, "--repo", "e/f"},
		{"source spellings", []string{"repo", "sync", "--source", "a/b", "-s", "c/d"}, "--source", "c/d"},
		{"family alias before subcommand", []string{"pr", "-H", "a:x", "create"}, "--head", "a:x"},
		{"same letter, other subcommand", []string{"pr", "checkout", "-b", "topic", "3"}, "--branch", "topic"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inv := parseInvocation(tt.args)
			if got := inv.values[tt.flag]; got != tt.want {
				t.Fatalf("%s = %q, want %q (values %v)", tt.flag, got, tt.want, inv.values)
			}
		})
	}
	inv := parseInvocation([]string{"-R", "a/b", "pr", "list", "--repo", "c/d", "-Re/f"})
	if inv.repoFlag != "e/f" {
		t.Fatalf("repo flag = %q", inv.repoFlag)
	}

	// The last spelling decides the routing too.
	ref, ok := resolveTarget([]string{"pr", "create", "--head", "someone:topic", "-H", "acme:topic"}, func(key string) string {
		if key == "GH_REPO" {
			return "acme/installed"
		}
		return ""
	}, t.TempDir())
	if !ok || ref.Owner != "acme" {
		t.Fatalf("last head: %+v, %v", ref, ok)
	}
}

func TestSSHAliasRemotes(t *testing.T) {
	isolateGit(t)
	for _, tc := range []struct{ raw, want string }{
		{"git@gh-alias:o/r.git", "github.com/o/r"},
		{"ssh://git@gh-alias/o/r.git", "github.com/o/r"},
		{"gh-443:o/r", "github.com/o/r"},
		{"git@forge.example.test:o/r.git", "forge.example.test/o/r"},
		{"https://gh-alias/o/r.git", "gh-alias/o/r"},
	} {
		ref, ok, unknown := resolveRemoteURL(tc.raw)
		if !ok || unknown || ref.Host+"/"+ref.Owner+"/"+ref.Name != tc.want {
			t.Fatalf("%s: %+v, %v, %v", tc.raw, ref, ok, unknown)
		}
	}
	if _, ok, unknown := resolveRemoteURL("git@broken:o/r.git"); !ok || !unknown {
		t.Fatal("failing ssh did not make the host unknown")
	}

	tests := []struct {
		name     string
		remotes  [][2]string
		resolved map[string]string
		want     string
	}{
		{"alias origin", [][2]string{{"origin", "git@gh-alias:o/r.git"}}, nil, "o/r"},
		{"unknown upstream outranks", [][2]string{{"origin", "git@github.com:o/r.git"}, {"upstream", "git@broken:u/p.git"}}, nil, ""},
		{"unknown later remote", [][2]string{{"origin", "git@github.com:o/r.git"}, {"zeta", "git@broken:z/z.git"}}, nil, "o/r"},
		{"unknown with gh-resolved", [][2]string{{"origin", "git@github.com:o/r.git"}, {"zeta", "git@broken:z/z.git"}}, map[string]string{"zeta": "base"}, ""},
		{"gh-resolved before unknown", [][2]string{{"origin", "git@github.com:o/r.git"}, {"zeta", "git@broken:z/z.git"}}, map[string]string{"origin": "base"}, "o/r"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref, ok := remoteTarget(gitRepo(t, tt.remotes, tt.resolved))
			got := ""
			if ok {
				got = ref.Owner + "/" + ref.Name
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}

	// Without ssh every alias is unknown.
	t.Setenv("PATH", t.TempDir())
	if _, ok, unknown := resolveRemoteURL("git@gh-alias:o/r.git"); !ok || !unknown {
		t.Fatal("missing ssh did not make the host unknown")
	}
	if ref, ok, unknown := resolveRemoteURL("git@github.com:o/r.git"); !ok || unknown || ref.Host != githubHost {
		t.Fatal("github.com needs no ssh")
	}
}

func TestPRCreateInForkCheckout(t *testing.T) {
	isolateGit(t)
	tests := []struct {
		name    string
		remotes [][2]string
		args    []string
		want    string
	}{
		{"own checkout", [][2]string{{"origin", "git@github.com:acme/installed.git"}}, []string{"pr", "create"}, "acme/installed"},
		{"fork checkout", [][2]string{{"origin", "git@github.com:me/installed.git"}, {"upstream", "git@github.com:acme/installed.git"}}, []string{"pr", "create"}, ""},
		{"fork checkout with -R", [][2]string{{"origin", "git@github.com:me/installed.git"}}, []string{"pr", "create", "-R", "acme/installed"}, ""},
		{"fork checkout with head", [][2]string{{"origin", "git@github.com:me/installed.git"}, {"upstream", "git@github.com:acme/installed.git"}}, []string{"pr", "create", "--head", "topic"}, "acme/installed"},
		{"unknown remote", [][2]string{{"origin", "git@github.com:acme/installed.git"}, {"zeta", "git@broken:x/y.git"}}, []string{"pr", "create"}, ""},
		{"other host remote", [][2]string{{"origin", "git@github.com:acme/installed.git"}, {"forge", "https://forge.example.test/me/installed.git"}}, []string{"pr", "create"}, "acme/installed"},
		{"other commands in a fork checkout", [][2]string{{"origin", "git@github.com:me/installed.git"}, {"upstream", "git@github.com:acme/installed.git"}}, []string{"pr", "list"}, "acme/installed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref, ok := resolveTarget(tt.args, func(string) string { return "" }, gitRepo(t, tt.remotes, nil))
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
