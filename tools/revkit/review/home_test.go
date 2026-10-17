package review

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// userHome creates a fake user home with both clients' configuration
// directories, holding logins and the files a reviewer must not see.
func userHome(t *testing.T) (string, string) {
	t.Helper()
	home := resolvedTempDir(t)
	for _, dir := range []string{".codex", ".claude"} {
		writeFile(t, filepath.Join(home, dir, "AGENTS.md"), "private rules\n", 0o644)
		writeFile(t, filepath.Join(home, dir, "CLAUDE.md"), "private rules\n", 0o644)
		writeFile(t, filepath.Join(home, dir, "settings.json"), "{}", 0o644)
		writeFile(t, filepath.Join(home, dir, "skills", "x", "SKILL.md"), "skill\n", 0o644)
	}
	writeFile(t, filepath.Join(home, ".claude", ".credentials.json"), "claude-login", 0o600)
	writeFile(t, filepath.Join(home, ".agents", "skills", "y", "SKILL.md"), "skill\n", 0o644)
	credentials := filepath.Join(home, ".codex", "auth.json")
	writeFile(t, credentials, "login-1", 0o600)
	return home, credentials
}

func entries(t *testing.T, dir string) []string {
	t.Helper()
	var names []string
	filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			rel, _ := filepath.Rel(dir, path)
			names = append(names, rel)
		}
		return nil
	})
	return names
}

func TestNewHomeHoldsOnlyTheLogin(t *testing.T) {
	user, credentials := userHome(t)
	dir := filepath.Join(resolvedTempDir(t), "home")
	home, err := NewHome(dir, []string{"HOME=" + user}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := entries(t, dir); !slices.Equal(got, []string{".codex/auth.json"}) {
		t.Fatalf("home holds %v", got)
	}
	if target, err := os.Readlink(filepath.Join(dir, ".codex", "auth.json")); err != nil || target != credentials {
		t.Fatalf("link = %q, %v; want %q", target, err, credentials)
	}
	environ := home.Environment()
	for _, entry := range []string{"HOME=" + dir, "CODEX_HOME=" + filepath.Join(dir, ".codex"), "TMPDIR=" + filepath.Join(dir, "tmp")} {
		if !slices.Contains(environ, entry) {
			t.Fatalf("environment %v lacks %s", environ, entry)
		}
	}
	if err := home.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(credentials); err != nil {
		t.Fatalf("removing the home touched the login: %v", err)
	}
}

func TestNewHomeFollowsCodexHome(t *testing.T) {
	other, credentials := userHome(t)
	real := credentials + ".real"
	if err := os.Rename(credentials, real); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, credentials); err != nil {
		t.Fatal(err)
	}
	home, err := NewHome(filepath.Join(resolvedTempDir(t), "h"), []string{"HOME=/nonexistent", "CODEX_HOME=" + filepath.Join(other, ".codex")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if home.source != real {
		t.Fatalf("source = %q, want the resolved %q", home.source, real)
	}
}

func TestNewHomeWithoutLogin(t *testing.T) {
	dir := filepath.Join(resolvedTempDir(t), "h")
	home, err := NewHome(dir, []string{"HOME=" + resolvedTempDir(t)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := entries(t, dir); len(got) != 0 {
		t.Fatalf("home holds %v", got)
	}
	if note := home.CheckLogin(); note != "" {
		t.Fatalf("CheckLogin = %q", note)
	}
}

func TestCheckLogin(t *testing.T) {
	user, credentials := userHome(t)
	home, err := NewHome(filepath.Join(resolvedTempDir(t), "h"), []string{"HOME=" + user}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Codex rewrites auth.json in place, which follows the link.
	if err := os.WriteFile(home.link, []byte("login-2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if note := home.CheckLogin(); note != "" {
		t.Fatalf("CheckLogin = %q", note)
	}
	if content, _ := os.ReadFile(credentials); string(content) != "login-2" {
		t.Fatalf("login = %q", content)
	}
	// A client that replaced the link leaves the user's login behind.
	if err := os.Remove(home.link); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(home.link, []byte("login-3"), 0o600); err != nil {
		t.Fatal(err)
	}
	if note := home.CheckLogin(); !strings.Contains(note, "log in again") {
		t.Fatalf("CheckLogin = %q", note)
	}
}

func TestNewHomeLinksNamedEntries(t *testing.T) {
	user, _ := userHome(t)
	writeFile(t, filepath.Join(user, ".nix-profile", "bin", "guard"), "#!/bin/sh\n", 0o755)
	dir := filepath.Join(resolvedTempDir(t), "h")
	if _, err := NewHome(dir, []string{"HOME=" + user}, []string{".nix-profile", ".missing"}); err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(filepath.Join(dir, ".nix-profile")); err != nil || target != filepath.Join(user, ".nix-profile") {
		t.Fatalf("link = %q, %v", target, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, ".missing")); err == nil {
		t.Fatal("linked a missing entry")
	}
	for _, name := range []string{".codex", ".agents", "a/b", "..", ".config", "", "tmp"} {
		if _, err := NewHome(filepath.Join(resolvedTempDir(t), "h"), []string{"HOME=" + user}, []string{name}); err == nil {
			t.Fatalf("linked %q", name)
		}
	}
}
