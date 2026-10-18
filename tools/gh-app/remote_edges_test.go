package main

import "testing"

func TestSSHAliasCase(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"git@GH-Mixed:o/r.git", "github.com"},
		{"ssh://git@GH-Mixed/o/r.git", "github.com"},
		{"git@gh-mixed:o/r.git", "gh-mixed"},
	} {
		ref, ok, unknown := resolveRemoteURL(tc.raw)
		if !ok || unknown || ref.Host != tc.want {
			t.Fatalf("%s: %+v, %v, %v", tc.raw, ref, ok, unknown)
		}
	}
}

func TestPRCreateEmptyHead(t *testing.T) {
	isolateGit(t)
	dir := gitRepo(t, [][2]string{{"origin", "git@github.com:me/installed.git"}, {"upstream", "git@github.com:acme/installed.git"}}, nil)
	for _, args := range [][]string{{"pr", "create", "--head="}, {"pr", "create", "-H", ""}} {
		if ref, ok := resolveTarget(args, func(string) string { return "" }, dir); ok {
			t.Fatalf("%v: %+v", args, ref)
		}
	}
}
