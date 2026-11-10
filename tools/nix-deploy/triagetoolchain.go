package main

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// nixpkgsChange is a locked input that is a nixpkgs tree and changed since
// a baseline, with the store paths of its two revisions. Inputs that follow
// the same nixpkgs share one change.
type nixpkgsChange struct {
	names    []string
	was, now lockedInput
	old, new string
}

// inputTree is where an input's source tree is: its store path. Tests
// replace it.
var inputTree = func(in lockedInput) string {
	return lockedStorePath(in.NarHash, in.storeName())
}

// isNixpkgsTree tells whether a store path holds a nixpkgs source tree.
func isNixpkgsTree(path string) bool {
	return fileExists(filepath.Join(path, "pkgs", "top-level", "all-packages.nix"))
}

// namesNixpkgs tells whether a locked input's source is a repository named
// nixpkgs, such as github:NixOS/nixpkgs or a Git URL ending in /nixpkgs; a
// related repository such as nixpkgs.lib or nixpkgs-wayland is not one.
func namesNixpkgs(in lockedInput) bool {
	if in.Repo == "nixpkgs" {
		return true
	}
	u := strings.TrimSuffix(strings.TrimSuffix(in.URL, "/"), ".git")
	return strings.HasSuffix(u, "/nixpkgs")
}

// nixpkgsChanges returns the inputs that changed since the baseline and are
// nixpkgs trees in both revisions, as far as this store holds them; missing
// lists the changed nixpkgs inputs whose old or new revision this store
// lacks, so they cannot be compared.
func nixpkgsChanges(baseline *baselineGroup, inputs map[string]lockedInput) (changes []nixpkgsChange, missing []string) {
	if baseline == nil || baseline.old == nil {
		return nil, nil
	}
	index := map[string]int{}
	for _, name := range sortedNames(inputs) {
		was, ok := baseline.old[name]
		now := inputs[name]
		if !ok || was.whole == now.whole {
			continue
		}
		oldPath, newPath := inputTree(was), inputTree(now)
		if oldPath == "" || newPath == "" {
			continue
		}
		if !pathExists(oldPath) || !pathExists(newPath) {
			// The revision that is still here tells whether it is nixpkgs;
			// with neither, the source's repository name does.
			if isNixpkgsTree(oldPath) || isNixpkgsTree(newPath) || namesNixpkgs(now) {
				missing = append(missing, name)
			}
			continue
		}
		if !isNixpkgsTree(oldPath) || !isNixpkgsTree(newPath) {
			continue
		}
		key := oldPath + "\x00" + newPath
		if i, ok := index[key]; ok {
			changes[i].names = append(changes[i].names, name)
			continue
		}
		index[key] = len(changes)
		changes = append(changes, nixpkgsChange{names: []string{name}, was: was, now: now, old: oldPath, new: newPath})
	}
	return changes, missing
}

// toolchainNames are the top-level attributes of nixpkgs'
// pkgs/top-level/all-packages.nix that choose a default toolchain, such as
// `go = go_1_27;`, in the order the context lists them.
var toolchainNames = []string{
	"go", "buildGoModule", "rustPackages", "default-gcc-version", "llvmPackages",
	"python3", "nodejs", "jdk", "ruby", "php", "perl", "zig",
}

// toolchainDeclaration matches a top-level declaration of all-packages.nix:
// two spaces of indentation, as its top-level attributes have, a name and a
// short value on one line.
var toolchainDeclaration = regexp.MustCompile(`^  ([A-Za-z][A-Za-z0-9_-]*) = ([^;]{1,80});\s*$`)

// toolchainDefaults reads which default toolchain a nixpkgs tree chooses,
// by name, from the first top-level declaration of each in
// pkgs/top-level/all-packages.nix. It reads the file only: the tree is the
// input's own code at whatever revision the lock moved to, and evaluating it
// cannot be confined (a locked builtins.getFlake fetches past Nix's pure and
// restricted modes), so triage evaluates nothing. A default chosen through
// an expression, or outside that file, is not seen.
func toolchainDefaults(tree string) map[string]string {
	data, err := os.ReadFile(filepath.Join(tree, "pkgs", "top-level", "all-packages.nix"))
	if err != nil {
		return nil
	}
	defaults := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		m := toolchainDeclaration.FindStringSubmatch(line)
		if m == nil || !contains(toolchainNames, m[1]) {
			continue
		}
		if _, seen := defaults[m[1]]; !seen {
			defaults[m[1]] = strings.TrimSpace(m[2])
		}
	}
	return defaults
}

// buildSupportFiles maps each file under pkgs/build-support of a tree to a
// digest of its content (a symlink to its target).
func buildSupportFiles(tree string) map[string]string {
	root := filepath.Join(tree, "pkgs", "build-support")
	files := map[string]string{}
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if entry.Type()&fs.ModeSymlink != 0 {
			target, _ := os.Readlink(path)
			files[rel] = "link:" + target
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		files[rel] = fmt.Sprintf("%x", sha256.Sum256(data))
		return nil
	})
	return files
}

// buildSupportChanges lists the files under pkgs/build-support that were
// added, removed or changed between two nixpkgs trees, sorted by path.
func buildSupportChanges(old, new string) []string {
	was, now := buildSupportFiles(old), buildSupportFiles(new)
	var paths []string
	for path := range now {
		if was[path] != now[path] {
			paths = append(paths, path)
		}
	}
	for path := range was {
		if _, ok := now[path]; !ok {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	changes := make([]string, len(paths))
	for i, path := range paths {
		_, before := was[path]
		_, after := now[path]
		switch {
		case !before:
			changes[i] = "added " + path
		case !after:
			changes[i] = "removed " + path
		default:
			changes[i] = "changed " + path
		}
	}
	return changes
}

// maxBuildSupportChanges caps the pkgs/build-support files listed.
const maxBuildSupportChanges = 30

// writeToolchainChanges writes, for each nixpkgs input that changed since
// the build-failed targets' baseline, the default toolchains it chooses
// differently and the files changed under pkgs/build-support: a failure
// after a nixpkgs update often comes from a compiler or a builder rather
// than from the failing package, and the lock diff does not show either.
func writeToolchainChanges(out *strings.Builder, baseline *baselineGroup, inputs map[string]lockedInput) {
	changes, missing := nixpkgsChanges(baseline, inputs)
	if len(changes) == 0 && len(missing) == 0 {
		return
	}
	out.WriteString("\n### Toolchain changes\n\nA build that fails after a nixpkgs update often fails because of a toolchain or a builder rather than the package itself, and the lock diff shows neither. These are the default toolchains the old and new nixpkgs revisions choose in pkgs/top-level/all-packages.nix (read from the file, not evaluated), and the files changed under pkgs/build-support between them.\n")
	for _, c := range changes {
		fmt.Fprintf(out, "\n%s: %s -> %s\n\n", quoteNames(c.names), c.was.identity(), c.now.identity())
		was, now := toolchainDefaults(c.old), toolchainDefaults(c.new)
		var same []string
		for _, name := range toolchainNames {
			before, after := was[name], now[name]
			switch {
			case before == "" && after == "":
			case before == after:
				same = append(same, name+" = "+after)
			default:
				fmt.Fprintf(out, "- `%s`: %s -> %s\n", name, orNone(before), orNone(after))
			}
		}
		if len(same) > 0 {
			fmt.Fprintf(out, "- Unchanged: %s\n", strings.Join(same, ", "))
		}
		files := buildSupportChanges(c.old, c.new)
		if len(files) == 0 {
			out.WriteString("- No file changed under pkgs/build-support.\n")
			continue
		}
		fmt.Fprintf(out, "- Files changed under pkgs/build-support (%d):\n", len(files))
		shown := files
		if len(shown) > maxBuildSupportChanges {
			shown = shown[:maxBuildSupportChanges]
		}
		for _, f := range shown {
			fmt.Fprintf(out, "  - %s\n", f)
		}
		if len(files) > len(shown) {
			fmt.Fprintf(out, "  - and %d more\n", len(files)-len(shown))
		}
	}
	if len(missing) > 0 {
		fmt.Fprintf(out, "\nNot compared, since this store lacks one of their two revisions: %s.\n", quoteNames(missing))
	}
}

func orNone(version string) string {
	if version == "" {
		return "none"
	}
	return version
}
