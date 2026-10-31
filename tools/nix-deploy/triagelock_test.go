package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Public pairs from nix flake metadata --json of github:nix-systems/default
// at da67096a and github:numtide/flake-utils at 11707dc2, and from
// builtins.fetchTree of a file input (default.nix of nix-systems/default at
// da67096a). The named pair is a synthetic directory with one file,
// hello.txt holding "hello\n", from nix hash path and nix store add
// --dry-run --name probe (and --name source).
func TestLockedStorePath(t *testing.T) {
	for _, c := range []struct{ narHash, name, want string }{
		{"sha256-Vy1rq5AaRuLzOxct8nz4T6wlgyUR7zLU309k9mBC768=", "source", "/nix/store/yj1wxm9hh8610iyzqnz75kvs6xl8j3my-source"},
		{"sha256-l0KFg5HjrsfsO/JpG+r7fRrqm12kzFHyUHqHCVpMMbI=", "source", "/nix/store/01x5k4nlxcpyd85nnr0b9gm89rm8ff4x-source"},
		{"sha256-zklqYResGOhx59bPMTdCzf7qSwNnwP4I/mYLV/C48iI=", "source", "/nix/store/1dwzfgpi9gzvdkrg7y8822fh3yal0g4d-source"},
		{"sha256-i9jJgC+rnkiRG+QQ31i/KRlnOwbAnAZM/cvj73cukwU=", "probe", "/nix/store/zcv1mlrsh1r65mi1741rrd8xf8blvn9z-probe"},
		{"sha256-i9jJgC+rnkiRG+QQ31i/KRlnOwbAnAZM/cvj73cukwU=", "source", "/nix/store/1pmb65l4shli0xi5qs4z86ga1jxwiimq-source"},
		{"sha256-i9jJgC+rnkiRG+QQ31i/KRlnOwbAnAZM/cvj73cukwU=", "a/b", ""},
		{"", "source", ""},
		{"sha512-abc", "source", ""},
	} {
		if got := lockedStorePath(c.narHash, c.name); got != c.want {
			t.Errorf("lockedStorePath(%q, %q) = %q, want %q", c.narHash, c.name, got, c.want)
		}
	}
	// Names as nix-store --print-fixed-path and builtins.toFile of Nix 2.35
	// accept or reject them.
	for name, valid := range map[string]bool{
		".probe": true, "..x": true, "-x": true, "_x": true, "?x": true, "a.b": true,
		strings.Repeat("a", 211): true, strings.Repeat("a", 212): false,
		".": false, "..": false, ".-x": false, "..-x": false, "a/b": false, "": false,
	} {
		if validStoreName(name) != valid {
			t.Errorf("validStoreName(%q) = %v", name, !valid)
		}
	}
	named := lockedInput{NarHash: "sha256-i9jJgC+rnkiRG+QQ31i/KRlnOwbAnAZM/cvj73cukwU=", Name: "probe"}
	if got := named.storePath(); got != "/nix/store/zcv1mlrsh1r65mi1741rrd8xf8blvn9z-probe" {
		t.Errorf("a named input's store path is %q", got)
	}
}

// lockData is a flake.lock with the given inputs below its root.
func lockData(t *testing.T, inputs map[string]lockedInput) []byte {
	t.Helper()
	nodes := map[string]any{"root": map[string]any{"inputs": map[string]string{}}}
	for name, in := range inputs {
		nodes[name] = map[string]any{"locked": &in}
	}
	data, err := json.Marshal(map[string]any{"root": "root", "nodes": nodes, "version": 7})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// setRun rewrites a synthetic run's start, flake directory and Git revision,
// and writes its flake.lock.
func setRun(t *testing.T, dir string, started time.Time, flakeDir, rev string, lock []byte) {
	t.Helper()
	info, err := readRunInfo(dir)
	if err != nil {
		t.Fatal(err)
	}
	info.Started, info.FlakeDir, info.GitRev = started, flakeDir, rev
	data, _ := json.Marshal(info)
	if err := os.WriteFile(filepath.Join(dir, "run.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "flake.lock"), lock, 0o600); err != nil {
		t.Fatal(err)
	}
}

const (
	hashA = "sha256-Vy1rq5AaRuLzOxct8nz4T6wlgyUR7zLU309k9mBC768="
	hashB = "sha256-l0KFg5HjrsfsO/JpG+r7fRrqm12kzFHyUHqHCVpMMbI="
)

var (
	oldLock = map[string]lockedInput{
		"nixpkgs": {Type: "github", Owner: "example", Repo: "pkgs", Rev: "aaaa", NarHash: hashA, LastModified: 1767225600},
		"gone":    {Type: "tarball", URL: "https://example.invalid/gone.tar.gz", NarHash: hashA},
	}
	newLock = map[string]lockedInput{
		"nixpkgs": {Type: "github", Owner: "example", Repo: "pkgs", Rev: "bbbb", NarHash: hashB, LastModified: 1769904000},
		"local":   {Type: "path", Path: "/src/local"},
	}
)

func TestLockDiffComparesWholeLockedObject(t *testing.T) {
	// Two flakes of one repository revision differ only in dir.
	old := map[string]lockedInput{"tool": {Type: "github", Owner: "example", Repo: "mono", Rev: "cccc", Dir: "a"}}
	new := map[string]lockedInput{"tool": {Type: "github", Owner: "example", Repo: "mono", Rev: "cccc", Dir: "b"}}
	before, _ := lockInputs(lockData(t, old))
	after, _ := lockInputs(lockData(t, new))
	var out strings.Builder
	writeLockDiff(&out, before, after)
	if want := "- Changed `tool`: github:example/mono?dir=a -> github:example/mono?dir=b"; !strings.Contains(out.String(), want) {
		t.Errorf("diff lacks %q:\n%s", want, out.String())
	}

	// A field the context does not show still counts.
	lock := `{"root":"root","nodes":{"root":{},"x":{"locked":{"type":"git","url":"https://example.invalid/x","rev":"dddd","submodules":%s}}}}`
	before, _ = lockInputs([]byte(strings.Replace(lock, "%s", "false", 1)))
	after, _ = lockInputs([]byte(strings.Replace(lock, "%s", "true", 1)))
	out.Reset()
	writeLockDiff(&out, before, after)
	if !strings.Contains(out.String(), "- Changed `x`") {
		t.Errorf("a change to an unshown field was missed:\n%s", out.String())
	}
}

func TestTriageLockBaselines(t *testing.T) {
	runs := stateRuns(t)
	at := time.Date(2026, 1, 2, 3, 5, 0, 0, time.UTC)
	ok := phaseReport{Status: "ok"}
	bad := phaseReport{Status: "failed"}
	run := func(id string, minutes int, flakeDir string, failed bool, hosts []hostReport, lock map[string]lockedInput) string {
		dir := makeRun(t, runs, id, failed, &runReport{Hosts: hosts}, nil)
		setRun(t, dir, at.Add(time.Duration(minutes)*time.Minute), flakeDir, "", lockData(t, lock))
		return dir
	}
	good := run("20260102T030100.000000Z-1", -4, "/flake", false,
		[]hostReport{{Host: "alpha", System: ok}, {Host: "beta", System: phaseReport{Status: "unchanged"}}}, oldLock)
	run("20260102T030200.000000Z-6", -3, "/flake", false,
		[]hostReport{{Host: "alpha", System: phaseReport{Status: "built"}}, {Host: "beta", System: phaseReport{Status: "built"}}}, newLock)
	run("20260102T030300.000000Z-2", -2, "/flake", true,
		[]hostReport{{Host: "alpha", System: bad}, {Host: "beta", System: bad}}, newLock)
	run("20260102T030400.000000Z-3", -1, "/other", false, []hostReport{{Host: "gamma", HomeManager: ok}}, oldLock)
	run("20260102T030900.000000Z-5", 4, "/flake", false, []hostReport{{Host: "gamma", HomeManager: ok}}, oldLock)
	dir := run("20260102T030500.000000Z-4", 0, "/flake", true,
		[]hostReport{{Host: "alpha", System: bad}, {Host: "beta", System: bad}, {Host: "gamma", HomeManager: bad}}, newLock)

	pathExists = func(path string) bool { return path == lockedStorePath(hashB, "source") }
	defer func() { pathExists = fileExists }()
	path, err := writeTriageContext(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	text := string(data)
	for _, want := range []string{
		"### Since the baseline of alpha system, beta system\n\nBaseline: " + filepath.Join(good, "flake.lock") + ", of run 20260102T030100.000000Z-1",
		"- Added `local`: path:/src/local at unknown (unknown), unknown",
		"- Removed `gone`: tarball+https://example.invalid/gone.tar.gz at " + hashA,
		"- Changed `nixpkgs`: github:example/pkgs\n  - locked: aaaa -> bbbb\n  - last modified: 2026-01-01 00:00 UTC -> 2026-02-01 00:00 UTC\n  - store path: " + lockedStorePath(hashA, "source") + " -> " + lockedStorePath(hashB, "source"),
		"### Since the baseline of gamma home-manager\n\nNo baseline:",
		"| local | no | path:/src/local | unknown | unknown |",
		"| nixpkgs | no | github:example/pkgs | " + lockedStorePath(hashB, "source") + " | yes |",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("context.md lacks %q:\n%s", want, text)
		}
	}
	if strings.Count(text, "### Since the baseline") != 2 || strings.Contains(text, "| gone |") {
		t.Errorf("wrong groups or inputs:\n%s", text)
	}
}

func TestTriageLockGitFallback(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	repo := t.TempDir()
	flakeDir := filepath.Join(repo, "sub", "flake")
	if err := os.MkdirAll(flakeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=test", "-c", "user.email=test@example.invalid"}, args...)...)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	commit := func(name string, data []byte) string {
		if err := os.WriteFile(filepath.Join(flakeDir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
		git("add", "-A")
		git("commit", "-q", "-m", name)
		return git("rev-parse", "HEAD")
	}
	git("init", "-q")
	first := commit("flake.lock", lockData(t, oldLock))
	commit("flake.lock", lockData(t, newLock))
	head := commit("other.txt", []byte("x"))
	commit("flake.lock", lockData(t, map[string]lockedInput{}))

	got, content := gitBaseline(flakeDir, head, lockData(t, newLock))
	if got != first || string(content) != string(lockData(t, oldLock)) {
		t.Fatalf("gitBaseline = %q", got)
	}

	runs := stateRuns(t)
	dir := makeRun(t, runs, "20260102T030500.000000Z-4", true,
		&runReport{Hosts: []hostReport{{Host: "alpha", System: phaseReport{Status: "failed"}}}}, nil)
	setRun(t, dir, time.Date(2026, 1, 2, 3, 5, 0, 0, time.UTC), flakeDir, head, lockData(t, newLock))
	path, err := writeTriageContext(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if want := "Baseline: flake.lock at Git commit " + first + ". No kept run shows these targets deployed, so this is an approximation"; !strings.Contains(string(data), want) {
		t.Errorf("context.md lacks %q:\n%s", want, data)
	}
	if !strings.Contains(string(data), "- Changed `nixpkgs`") {
		t.Errorf("no diff from the Git baseline:\n%s", data)
	}

	// A lock whose object is missing, as in a partial clone, gives no
	// baseline rather than a fetch.
	blob := git("rev-parse", first+":sub/flake/flake.lock")
	if err := os.Remove(filepath.Join(repo, ".git", "objects", blob[:2], blob[2:])); err != nil {
		t.Fatal(err)
	}
	if got, _ := gitBaseline(flakeDir, head, lockData(t, newLock)); got != "" {
		t.Fatalf("gitBaseline with a missing object = %q", got)
	}
}

func TestGitBaselineSkipsDeletions(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	repo := t.TempDir()
	git := func(args ...string) string {
		output, err := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=test", "-c", "user.email=test@example.invalid"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	lock := filepath.Join(repo, "flake.lock")
	git("init", "-q")
	// C1 adds the old lock, C2 deletes it, C3 adds the new one.
	if err := os.WriteFile(lock, lockData(t, oldLock), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "C1")
	first := git("rev-parse", "HEAD")
	git("rm", "-q", "flake.lock")
	git("commit", "-q", "-m", "C2")
	if err := os.WriteFile(lock, lockData(t, newLock), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "C3")
	if got, _ := gitBaseline(repo, git("rev-parse", "HEAD"), lockData(t, newLock)); got != first {
		t.Fatalf("gitBaseline = %q, want C1 %q", got, first)
	}
}

func TestGitBaselineMergeResolution(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	repo := t.TempDir()
	git := func(args ...string) string {
		output, err := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=test", "-c", "user.email=test@example.invalid"}, args...)...).CombinedOutput()
		if err != nil && args[0] != "merge" {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	commit := func(data []byte, message string) {
		if err := os.WriteFile(filepath.Join(repo, "flake.lock"), data, 0o600); err != nil {
			t.Fatal(err)
		}
		git("add", "-A")
		git("commit", "-q", "-m", message)
	}
	lockOf := func(rev string) []byte {
		return lockData(t, map[string]lockedInput{"nixpkgs": {Type: "github", Owner: "example", Repo: "pkgs", Rev: rev}})
	}
	git("init", "-q", "-b", "main")
	commit(lockOf("base"), "base")
	git("checkout", "-q", "-b", "side")
	commit(lockOf("side"), "side")
	git("checkout", "-q", "main")
	commit(lockOf("main"), "main")
	// The branches conflict on the lock; the merge resolves it to a third.
	git("merge", "-q", "side")
	commit(lockOf("merged"), "merge")
	merge := git("rev-parse", "HEAD")
	if parents := strings.Fields(git("rev-list", "--parents", "-n", "1", merge)); len(parents) != 3 {
		t.Fatalf("not a merge commit: %v", parents)
	}
	commit(lockOf("new"), "bump")

	got, content := gitBaseline(repo, git("rev-parse", "HEAD"), lockOf("new"))
	if got != merge || string(content) != string(lockOf("merged")) {
		t.Fatalf("gitBaseline = %q, want the merge %q", got, merge)
	}
}
