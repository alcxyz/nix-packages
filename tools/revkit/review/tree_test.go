package review

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// gitRepo creates a repository with one commit made by run(t, dir, ...).
func gitRepo(t *testing.T) string {
	t.Helper()
	dir := resolvedTempDir(t)
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	runGit(t, dir, "init", "-q", "-b", "main")
	return dir
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=test", "-c", "user.email=test@example.invalid"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func writeFile(t *testing.T, path, content string, perm os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		t.Fatal(err)
	}
}

func TestWriteTreeUsesRawObjects(t *testing.T) {
	repo := gitRepo(t)
	marker := filepath.Join(resolvedTempDir(t), "filter-ran")
	writeFile(t, filepath.Join(repo, "README.md"), "hello\n", 0o644)
	writeFile(t, filepath.Join(repo, "bin", "run.sh"), "#!/bin/sh\n", 0o755)
	writeFile(t, filepath.Join(repo, ".gitattributes"), "*.md filter=probe\n", 0o644)
	if err := os.Symlink("/etc/passwd", filepath.Join(repo, "outside")); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-q", "-m", "first")
	// A smudge filter and a hook in the repository must not run.
	runGit(t, repo, "config", "filter.probe.smudge", "touch "+marker+"; cat")
	writeFile(t, filepath.Join(repo, ".git", "hooks", "post-checkout"), "#!/bin/sh\ntouch "+marker+"\n", 0o755)

	commit, err := ResolveCommit(repo, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(resolvedTempDir(t), "tree")
	if err := WriteTree(repo, commit, dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a filter or hook ran")
	}
	content, err := os.ReadFile(filepath.Join(dest, "README.md"))
	if err != nil || string(content) != "hello\n" {
		t.Fatalf("README.md = %q, %v", content, err)
	}
	info, err := os.Stat(filepath.Join(dest, "bin", "run.sh"))
	if err != nil || info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("run.sh is not executable: %v %v", info, err)
	}
	link, err := os.Lstat(filepath.Join(dest, "outside"))
	if err != nil || !link.Mode().IsRegular() {
		t.Fatalf("symbolic link was not written as a file: %v %v", link, err)
	}
	content, _ = os.ReadFile(filepath.Join(dest, "outside"))
	if string(content) != "symbolic link to /etc/passwd\n" {
		t.Fatalf("outside = %q", content)
	}
	if _, err := os.Stat(filepath.Join(dest, ".git")); err == nil {
		t.Fatal("the tree has a .git directory")
	}
	if err := WriteTree(repo, commit, dest); err == nil {
		t.Fatal("WriteTree overwrote an existing tree")
	}
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(dest), ".tree-*"))
	if len(leftovers) != 0 {
		t.Fatalf("staging directories left behind: %v", leftovers)
	}
}

func TestResolveCommitRejectsUnknownRef(t *testing.T) {
	repo := gitRepo(t)
	writeFile(t, filepath.Join(repo, "a"), "a\n", 0o644)
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-q", "-m", "first")
	if _, err := ResolveCommit(repo, "no-such-branch"); err == nil {
		t.Fatal("resolved an unknown ref")
	}
	if _, err := ResolveCommit(repo, "--output=/tmp/x"); err == nil {
		t.Fatal("resolved an option as a ref")
	}
}
