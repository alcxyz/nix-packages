package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// This fixture runs only in installCheck. Full parity/fault injection runs
// against the installed Go binary, while this checks its actual Nix wrapper.
func TestPackagedWrapper(t *testing.T) {
	binary := os.Getenv("WORKSPACE_SYNC_WRAPPED_BINARY")
	if binary == "" {
		t.Skip("requires the installed Nix wrapper")
	}
	for _, variant := range []string{"shebang", "shell-fallback"} {
		t.Run(variant, func(t *testing.T) {
			fixture := t.TempDir()
			fake := filepath.Join(fixture, "bin")
			if err := os.Mkdir(fake, 0755); err != nil {
				t.Fatal(err)
			}
			bash, err := exec.LookPath("bash")
			if err != nil {
				t.Fatal(err)
			}
			log := filepath.Join(fixture, "git.log")
			git := `printf '%s\n' "$@" >>"$WORKSPACE_FIXTURE_GIT_LOG"
if [ "$1" = clone ]; then
  for target in "$@"; do :; done
  mkdir -p -- "$target"
fi
`
			if variant == "shebang" {
				git = "#!" + bash + "\n" + git
			}
			if err := os.WriteFile(filepath.Join(fake, "git"), []byte(git), 0755); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"jq", "realpath", "dirname", "readlink", "ln"} {
				if err := os.WriteFile(filepath.Join(fake, name), []byte("#!"+bash+"\nexit 71\n"), 0755); err != nil {
					t.Fatal(err)
				}
			}
			manifest := filepath.Join(fixture, "manifest.json")
			if err := os.WriteFile(manifest, []byte(`{"directories":["apps"],"repositories":[{"path":"apps/repo","url":"synthetic local source"}],"links":[{"path":"current","target":"apps/repo","profiles":[]}]}`), 0644); err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(fixture, "workspace")
			invoke := func(mode string) string {
				t.Helper()
				args := []string{root, manifest}
				if mode != "" {
					args = append(args, mode)
				}
				cmd := exec.Command(binary, args...)
				cmd.Env = append(os.Environ(), "PATH="+fake, "WORKSPACE_FIXTURE_GIT_LOG="+log)
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("packaged %q: %v\n%s", mode, err, out)
				}
				return string(out)
			}
			invoke("--help")
			invoke("--status")
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Fatalf("status created root: %v", err)
			}
			invoke("--directories")
			if _, err := os.Stat(filepath.Join(root, "apps")); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(log); !os.IsNotExist(err) {
				t.Fatalf("help/status/directories called Git: %v", err)
			}
			out := invoke("")
			if !strings.Contains(out, "0 existing, 1 cloned, 0 skipped, 0 failed") || !strings.Contains(out, "1 links created") {
				t.Fatalf("unexpected packaged sync: %s", out)
			}
			link, err := os.Readlink(filepath.Join(root, "current"))
			if err != nil || link != "apps/repo" {
				t.Fatalf("packaged link %q: %v", link, err)
			}
			args, err := os.ReadFile(log)
			if err != nil || string(args) != "clone\nsynthetic local source\n"+root+"/apps/repo\n" {
				t.Fatalf("consumer Git arguments %q: %v", args, err)
			}
		})
	}
}
