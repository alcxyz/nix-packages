package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// nixpkgsTree writes a minimal nixpkgs-shaped tree with the given files
// under pkgs/build-support and the given all-packages.nix.
func nixpkgsTree(t *testing.T, files map[string]string, allPackages ...string) string {
	t.Helper()
	dir := t.TempDir()
	top := "{ }\n"
	if len(allPackages) > 0 {
		top = allPackages[0]
	}
	all := map[string]string{"pkgs/top-level/all-packages.nix": top}
	for name, content := range files {
		all["pkgs/build-support/"+name] = content
	}
	for name, content := range all {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestBuildSupportChanges(t *testing.T) {
	old := nixpkgsTree(t, map[string]string{"rust/hook.sh": "a", "go/module.nix": "same", "gone.nix": "x"})
	new := nixpkgsTree(t, map[string]string{"rust/hook.sh": "b", "go/module.nix": "same", "added.nix": "y"})
	got := strings.Join(buildSupportChanges(old, new), "\n")
	want := "added added.nix\nremoved gone.nix\nchanged rust/hook.sh"
	if got != want {
		t.Fatalf("changes:\n%s\nwant:\n%s", got, want)
	}
}

func TestToolchainChanges(t *testing.T) {
	// The netdata line is nested, as in nixpkgs, and must not count.
	allOld := "{\n  netdata = callPackage ../x {\n    go = go_1_27;\n  };\n  nodejs = nodejs_24;\n  go = go_1_26;\n  buildGoModule = buildGo126Module;\n  rustPackages = rustPackages_1_99;\n}\n"
	allNew := "{\n  netdata = callPackage ../x {\n    go = go_1_27;\n  };\n  nodejs = nodejs_24;\n  go = go_1_27;\n  buildGoModule = buildGo127Module;\n  rustPackages = rustPackages_1_99;\n}\n"
	old := nixpkgsTree(t, map[string]string{"rust/hooks/cargo-setup-hook.sh": "old"}, allOld)
	new := nixpkgsTree(t, map[string]string{"rust/hooks/cargo-setup-hook.sh": "new"}, allNew)
	trees := map[string]string{"sha256-old": old, "sha256-new": new}
	// A revision the store lacks still has a store path; it just does not exist.
	inputTree = func(in lockedInput) string {
		if tree, ok := trees[in.NarHash]; ok {
			return tree
		}
		return filepath.Join(t.TempDir(), "collected-"+in.NarHash)
	}
	defer func() { inputTree = func(in lockedInput) string { return lockedStorePath(in.NarHash, in.storeName()) } }()

	lock := func(hash, rev string) map[string]lockedInput {
		inputs, err := lockInputs(lockData(t, map[string]lockedInput{
			"nixpkgs":   {Type: "github", Owner: "NixOS", Repo: "nixpkgs", Rev: rev, NarHash: hash},
			"nixpkgs_2": {Type: "github", Owner: "NixOS", Repo: "nixpkgs", Rev: rev, NarHash: hash},
			"other":     {Type: "github", Owner: "o", Repo: "other", Rev: "r1", NarHash: "sha256-other"},
		}))
		if err != nil {
			t.Fatal(err)
		}
		return inputs
	}
	baseline := &baselineGroup{old: lock("sha256-old", "aaa")}
	var out strings.Builder
	writeToolchainChanges(&out, baseline, lock("sha256-new", "bbb"))
	text := out.String()
	for _, want := range []string{
		"### Toolchain changes",
		"`nixpkgs`, `nixpkgs_2`: aaa -> bbb",
		"- `go`: go_1_26 -> go_1_27",
		"- `buildGoModule`: buildGo126Module -> buildGo127Module",
		"- Unchanged: rustPackages = rustPackages_1_99, nodejs = nodejs_24",
		"- Files changed under pkgs/build-support (1):",
		"  - changed rust/hooks/cargo-setup-hook.sh",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("toolchain section lacks %q:\n%s", want, text)
		}
	}

	// A nixpkgs input whose old revision this store lacks is named, not compared.
	delete(trees, "sha256-old")
	out.Reset()
	writeToolchainChanges(&out, baseline, lock("sha256-new", "bbb"))
	if !strings.Contains(out.String(), "Not compared, since this store lacks one of their two revisions: `nixpkgs`, `nixpkgs_2`.") {
		t.Errorf("missing revisions not reported:\n%s", out.String())
	}

	// An input that is not nixpkgs, such as nixpkgs.lib, is neither compared
	// nor reported missing, even though its name contains nixpkgs.
	lib := t.TempDir()
	libNew := t.TempDir()
	trees["sha256-old"] = old
	trees["sha256-lib-old"], trees["sha256-lib-new"] = lib, libNew
	libLock := func(hash string) map[string]lockedInput {
		inputs, err := lockInputs(lockData(t, map[string]lockedInput{
			"nixpkgs-lib": {Type: "github", Owner: "nix-community", Repo: "nixpkgs.lib", Rev: hash, NarHash: hash},
		}))
		if err != nil {
			t.Fatal(err)
		}
		return inputs
	}
	out.Reset()
	writeToolchainChanges(&out, &baselineGroup{old: libLock("sha256-lib-old")}, libLock("sha256-lib-new"))
	if out.Len() != 0 {
		t.Errorf("nixpkgs.lib got a toolchain section:\n%s", out.String())
	}

	// Without a nixpkgs change there is no section.
	out.Reset()
	writeToolchainChanges(&out, &baselineGroup{old: lock("sha256-new", "bbb")}, lock("sha256-new", "bbb"))
	if out.Len() != 0 {
		t.Errorf("unchanged nixpkgs wrote a section:\n%s", out.String())
	}
}
