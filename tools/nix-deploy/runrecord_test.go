package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunIDsSortByTime(t *testing.T) {
	base := time.Date(2026, 10, 27, 16, 54, 25, 0, time.UTC)
	times := []time.Time{
		base,
		base.Add(time.Microsecond),
		base.Add(999 * time.Millisecond),
		base.Add(time.Second),
		base.Add(10 * time.Hour),
	}
	var ids []string
	for i, started := range times {
		// A later run may have a smaller process ID.
		id := runID(started, 9000-i*1000)
		if !runIDPattern.MatchString(id) {
			t.Fatalf("id %q does not match the pattern prune uses", id)
		}
		ids = append(ids, id)
	}
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	for i := range ids {
		if sorted[i] != ids[i] {
			t.Fatalf("ids do not sort by time: %v", sorted)
		}
	}
	if runID(base, 1) == runID(base, 2) {
		t.Fatal("runs started together share an id")
	}
	// Local time is recorded as UTC.
	if runID(base.In(time.FixedZone("x", 3600)), 1) != runID(base, 1) {
		t.Fatal("the id depends on the time zone")
	}
}

func flakeWithLock(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "flake.lock"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRunRecordLayout(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	rec := startRunRecord([]string{"--all"}, flakeWithLock(t, "lock one"), true, false, false, 30)
	if rec.dir == "" || filepath.Dir(rec.dir) != filepath.Join(state, "nix-deploy", "runs") {
		t.Fatalf("run directory %q", rec.dir)
	}
	for _, name := range []string{"", "logs"} {
		info, err := os.Stat(filepath.Join(rec.dir, name))
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("%s: %v %v", name, info, err)
		}
	}
	// A step that runs again appends to its log.
	for _, line := range []string{"error: one\n", "error: two\n"} {
		log := rec.log("system-node-evaluate.log")
		io.WriteString(log, line)
		log.Close()
	}
	var running runInfo
	data, _ := os.ReadFile(filepath.Join(rec.dir, "run.json"))
	if err := json.Unmarshal(data, &running); err != nil || running.Ended != nil || running.Failed != nil || !running.DryRun {
		t.Fatalf("run.json while running: %s", data)
	}
	rec.finish(true)

	log, err := os.ReadFile(filepath.Join(rec.dir, "logs", "system-node-evaluate.log"))
	if err != nil || string(log) != "error: one\nerror: two\n" {
		t.Fatalf("log %q %v", log, err)
	}
	for _, name := range []string{"run.json", "flake.lock", "logs/system-node-evaluate.log"} {
		info, err := os.Stat(filepath.Join(rec.dir, name))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", name, info, err)
		}
	}
	var done runInfo
	data, _ = os.ReadFile(filepath.Join(rec.dir, "run.json"))
	if err := json.Unmarshal(data, &done); err != nil || done.Ended == nil || done.Failed == nil || !*done.Failed {
		t.Fatalf("run.json after the run: %s", data)
	}
	if done.LockSHA256 == "" || done.Args[0] != "--all" || done.ID != filepath.Base(rec.dir) {
		t.Fatalf("run.json: %s", data)
	}
	if done.GitRev != "" || done.GitDirty != nil {
		t.Fatalf("git state outside a checkout: %s", data)
	}
	if done.LockSHA256 != lockSum([]byte("lock one")) || done.StartLockSHA256 != "" {
		t.Fatalf("lock hashes in run.json: %s", data)
	}
}

func TestEvaluatedLockIsTheDeployedOne(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	flake := flakeWithLock(t, "stale")
	rec := startRunRecord(nil, flake, false, false, false, 30)
	start := rec.info.LockSHA256
	// An unchanged lock leaves the record alone.
	rec.noteEvaluatedLock()
	if rec.info.LockSHA256 != start || rec.info.StartLockSHA256 != "" {
		t.Fatalf("an unchanged lock moved: %+v", rec.info)
	}
	// Evaluation rewrote the lock.
	os.WriteFile(filepath.Join(flake, "flake.lock"), []byte("rewritten"), 0o644)
	rec.noteEvaluatedLock()
	if rec.info.StartLockSHA256 != start || rec.info.LockSHA256 != lockSum([]byte("rewritten")) {
		t.Fatalf("run info %+v", rec.info)
	}
	var info runInfo
	data, _ := os.ReadFile(filepath.Join(rec.dir, "run.json"))
	if json.Unmarshal(data, &info) != nil || info.LockSHA256 != rec.info.LockSHA256 || info.StartLockSHA256 != start {
		t.Fatalf("run.json %s", data)
	}
	for name, want := range map[string]string{"flake.lock": "rewritten", "flake.start.lock": "stale"} {
		if got, _ := os.ReadFile(filepath.Join(rec.dir, name)); string(got) != want {
			t.Fatalf("%s is %q", name, got)
		}
	}
}

func TestStepLogsAreClosed(t *testing.T) {
	if _, err := os.Stat("/proc/self/fd"); err != nil {
		t.Skip("no /proc/self/fd")
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	d := &deployer{record: startRunRecord(nil, t.TempDir(), false, false, false, 30)}
	open := func() int {
		entries, _ := os.ReadDir("/proc/self/fd")
		return len(entries)
	}
	before := open()
	for i := 0; i < 100; i++ {
		streams, log := d.logged(attached, target{phase: "system", host: "h" + strconv.Itoa(i)}, "activate")
		io.WriteString(streams.out, "")
		log.Close()
	}
	if after := open(); after > before {
		t.Fatalf("%d descriptors open after the steps, %d before", after, before)
	}
}

func TestBuildLogLookupsAreBounded(t *testing.T) {
	// A mass failure: most derivations left to build never started.
	var drvs []string
	for i := 0; i < 500; i++ {
		drvs = append(drvs, "/nix/store/x-"+strconv.Itoa(i)+".drv")
	}
	later := time.Now().Add(time.Hour)
	tried := 0
	everyTenth := func(drv string, _ time.Duration) []byte {
		tried++
		if tried%10 == 0 {
			return []byte("log of " + drv)
		}
		return nil
	}
	logs, n := collectBuildLogs(drvs, later, everyTenth)
	if len(logs) != maxBuildLogs || n != 200 || tried != 200 {
		t.Fatalf("%d logs after %d lookups (%d tried)", len(logs), n, tried)
	}
	// Few logs among many derivations: lookups stop at their own bound.
	tried = 0
	rare := func(drv string, _ time.Duration) []byte {
		tried++
		if tried == 1 {
			return []byte("log")
		}
		return nil
	}
	logs, n = collectBuildLogs(drvs, later, rare)
	if len(logs) != 1 || n != maxBuildLogLookups {
		t.Fatalf("%d logs after %d lookups", len(logs), n)
	}
	// Logs everywhere: the found ones stop it.
	logs, n = collectBuildLogs(drvs, later, func(string, time.Duration) []byte { return []byte("log") })
	if len(logs) != maxBuildLogs || n != maxBuildLogs {
		t.Fatalf("%d logs after %d lookups", len(logs), n)
	}
	// A slow log store: the budget stops the lookups, and no lookup may take
	// longer than the time left or the per-lookup timeout.
	deadline := time.Now().Add(250 * time.Millisecond)
	var timeouts []time.Duration
	slow := func(_ string, timeout time.Duration) []byte {
		timeouts = append(timeouts, timeout)
		time.Sleep(100 * time.Millisecond)
		return nil
	}
	logs, n = collectBuildLogs(drvs, deadline, slow)
	if len(logs) != 0 || n < 2 || n > 4 {
		t.Fatalf("%d logs after %d lookups within the budget", len(logs), n)
	}
	for _, timeout := range timeouts {
		if timeout > 250*time.Millisecond || timeout <= 0 {
			t.Fatalf("lookup timeout %v", timeout)
		}
	}
}

func TestPruneKeepsNewestAndActiveRuns(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	runs := filepath.Join(state, "nix-deploy", "runs")
	// Six older runs.
	started := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	var old []string
	for i := 0; i < 6; i++ {
		id := runID(started.Add(time.Duration(i)*time.Minute), 100+i)
		old = append(old, id)
		os.MkdirAll(filepath.Join(runs, id, "logs"), 0o700)
	}
	// The oldest run is still going on and holds its directory; the next
	// ended and left its lock file unheld.
	held, err := os.OpenFile(filepath.Join(runs, old[0], runLockName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil || syscall.Flock(int(held.Fd()), syscall.LOCK_EX) != nil {
		t.Fatal(err)
	}
	defer held.Close()
	os.WriteFile(filepath.Join(runs, old[1], runLockName), nil, 0o600)
	// Unrelated entries are never removed.
	os.MkdirAll(filepath.Join(runs, "notes"), 0o700)

	// The current run is older than the newest kept ones, as when its
	// clock was behind, and must survive anyway.
	rec := startRunRecord(nil, flakeWithLock(t, "current"), false, false, false, 2)
	current := runID(started.Add(-time.Hour), 1)
	if err := os.Rename(rec.dir, filepath.Join(runs, current)); err != nil {
		t.Fatal(err)
	}
	rec.dir = filepath.Join(runs, current)
	rec.info.ID = current
	rec.finish(false)

	var left []string
	entries, _ := os.ReadDir(runs)
	for _, entry := range entries {
		left = append(left, entry.Name())
	}
	want := []string{current, old[0], old[4], old[5], "notes"}
	sort.Strings(want)
	if strings.Join(left, " ") != strings.Join(want, " ") {
		t.Fatalf("runs left %v, want %v", left, want)
	}
}

// captureStderr returns what f writes to standard error.
func captureStderr(t *testing.T, f func()) string {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stderr
	os.Stderr = write
	defer func() { os.Stderr = saved }()
	f()
	write.Close()
	data, _ := io.ReadAll(read)
	return string(data)
}

func TestRunRecordUnwritableStateWarnsOnce(t *testing.T) {
	// The state directory is a file, so nothing under it can be created.
	state := filepath.Join(t.TempDir(), "state")
	os.WriteFile(state, []byte("not a directory"), 0o600)
	t.Setenv("XDG_STATE_HOME", state)
	d := &deployer{}
	var logged stdio
	output := captureStderr(t, func() {
		d.record = startRunRecord(nil, flakeWithLock(t, "lock"), false, false, false, 30)
		logged, _ = d.logged(attached, target{phase: "system", host: "node"}, "activate")
		d.finishRecord(nil, newReport(), true)
	})
	if strings.Count(output, "deploy: warning: run record: ") != 1 || strings.Count(output, "\n") != 1 {
		t.Fatalf("stderr %q", output)
	}
	// Without a record, commands keep the terminal itself.
	if logged.out != os.Stdout || logged.err != os.Stderr {
		t.Fatal("streams were wrapped without a record")
	}
}

func TestFailedRunNamesItsRecord(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	d := &deployer{}
	output := captureStderr(t, func() {
		d.record = startRunRecord(nil, flakeWithLock(t, "lock"), false, false, false, 30)
		r := newReport()
		r.record("system", "node", "failed")
		d.finishRecord([]string{"node"}, r, true)
	})
	if output != "deploy: run record: "+d.record.dir+"\n" {
		t.Fatalf("stderr %q", output)
	}
	var report runReport
	data, _ := os.ReadFile(filepath.Join(d.record.dir, "report.json"))
	if err := json.Unmarshal(data, &report); err != nil || !report.Failed || report.Hosts[0].System.Status != "failed" {
		t.Fatalf("report.json %s", data)
	}
}

func TestFailedBuildDerivations(t *testing.T) {
	// After a failed build, the dry run of the failed targets lists the
	// derivation that failed and those that waited on it; fetched paths are
	// not built and have no build log.
	output := `<3>these 3 derivations will be built:
  /nix/store/aaaa-linux-6.12.drv
  /nix/store/bbbb-nixos-system-node.drv
  /nix/store/cccc-etc.drv
this path will be fetched (1.0 KiB download, 1.0 KiB unpacked):
  /nix/store/dddd-source
`
	drvs, err := parsePendingBuilds(output)
	if err != nil || strings.Join(drvs, " ") != "/nix/store/aaaa-linux-6.12.drv /nix/store/bbbb-nixos-system-node.drv /nix/store/cccc-etc.drv" {
		t.Fatalf("%v %v", drvs, err)
	}
	if name := buildLogName(drvs[0]); name != "build-aaaa-linux-6.12.log" {
		t.Fatalf("log name %q", name)
	}
}

func TestFindFlakeDirLikeNix(t *testing.T) {
	root := t.TempDir()
	mk := func(path string) string {
		full := filepath.Join(root, path)
		os.MkdirAll(full, 0o755)
		return full
	}
	touch := func(path string) { os.WriteFile(filepath.Join(root, path), nil, 0o644) }
	// A Git flake searched from a subdirectory.
	repo := mk("repo")
	mk("repo/.git")
	touch("repo/flake.nix")
	deeper := mk("repo/sub/deeper")
	if got := findFlakeDir(deeper); got != repo {
		t.Fatalf("from a subdirectory: %q", got)
	}
	if got := findFlakeDir(repo); got != repo {
		t.Fatalf("from the flake: %q", got)
	}
	// The search stops at the Git root, even with a flake above it.
	touch("flake.nix")
	inner := mk("outer/inner")
	mk("outer/inner/.git")
	if got := findFlakeDir(filepath.Join(inner)); got != inner {
		t.Fatalf("past the Git root: %q", got)
	}
	// Outside Git it goes up to the nearest flake.
	plain := mk("plain/sub")
	if got := findFlakeDir(plain); got != root {
		t.Fatalf("outside Git: %q", got)
	}
}

func TestMissingLockBesideFlakeWarns(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	flake := t.TempDir()
	os.WriteFile(filepath.Join(flake, "flake.nix"), nil, 0o644)
	output := captureStderr(t, func() {
		rec := startRunRecord(nil, flake, false, false, false, 30)
		rec.noteEvaluatedLock()
		rec.noteEvaluatedLock()
	})
	if strings.Count(output, "deploy: warning: run record: no flake.lock beside") != 1 {
		t.Fatalf("stderr %q", output)
	}
	// Without a flake there is nothing to warn about.
	output = captureStderr(t, func() {
		startRunRecord(nil, t.TempDir(), false, false, false, 30).noteEvaluatedLock()
	})
	if output != "" {
		t.Fatalf("stderr %q", output)
	}
}

func TestAttemptedDerivations(t *testing.T) {
	// The format of recent Nix: names, wrapped, inputs under inputs.drvs.
	current := `{"derivations":{
		"aaaa-linux.drv":{"inputs":{"drvs":{"zzzz-gcc.drv":{"outputs":["out"]}},"srcs":[]},"outputs":{"out":{"path":"aaaa-linux"}}},
		"bbbb-modules.drv":{"inputs":{"drvs":{"aaaa-linux.drv":{"outputs":["out"]}},"srcs":[]},"outputs":{"out":{"path":"bbbb-modules"}}},
		"cccc-system.drv":{"inputs":{"drvs":{"bbbb-modules.drv":{"outputs":["out"]},"dddd-etc.drv":{"outputs":["out"]}},"srcs":[]},"outputs":{"out":{"path":"cccc-system"}}},
		"dddd-etc.drv":{"inputs":{"drvs":{},"srcs":[]},"outputs":{"out":{"path":"dddd-etc"}}},
		"eeee-tool.drv":{"inputs":{"drvs":{"yyyy-lib.drv":{"outputs":["out"]}},"srcs":[]},"outputs":{"out":{"path":"eeee-tool"}}},
		"yyyy-lib.drv":{"inputs":{"drvs":{},"srcs":[]},"outputs":{"out":{"path":"yyyy-lib"}}}},"version":4}`
	// Older Nix and Lix: store paths, inputs under inputDrvs.
	older := `{
		"/nix/store/aaaa-linux.drv":{"inputDrvs":{"/nix/store/zzzz-gcc.drv":["out"]},"outputs":{"out":{"path":"/nix/store/aaaa-linux"}}},
		"/nix/store/bbbb-modules.drv":{"inputDrvs":{"/nix/store/aaaa-linux.drv":["out"]},"outputs":{"out":{"path":"/nix/store/bbbb-modules"}}},
		"/nix/store/cccc-system.drv":{"inputDrvs":{"/nix/store/bbbb-modules.drv":["out"],"/nix/store/dddd-etc.drv":["out"]},"outputs":{"out":{"path":"/nix/store/cccc-system"}}},
		"/nix/store/dddd-etc.drv":{"inputDrvs":{},"outputs":{"out":{"path":"/nix/store/dddd-etc"}}},
		"/nix/store/eeee-tool.drv":{"inputDrvs":{"/nix/store/yyyy-lib.drv":["out"]},"outputs":{"out":{"path":"/nix/store/eeee-tool"}}},
		"/nix/store/yyyy-lib.drv":{"inputDrvs":{},"outputs":{"out":{"path":"/nix/store/yyyy-lib"}}}}`
	pending := []string{"/nix/store/aaaa-linux.drv", "/nix/store/bbbb-modules.drv", "/nix/store/cccc-system.drv", "/nix/store/dddd-etc.drv", "/nix/store/eeee-tool.drv"}
	// The library was to be downloaded and its download failed.
	fetches := []string{"/nix/store/yyyy-lib", "/nix/store/xxxx-source"}
	for name, output := range map[string]string{"current": current, "older": older} {
		facts, err := parseDerivations([]byte(output))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		// linux and etc wait on nothing pending (gcc was built); modules and
		// the system wait on linux, and the tool on the library's download.
		got := strings.Join(attemptedDrvs(pending, fetches, facts), " ")
		if got != "/nix/store/aaaa-linux.drv /nix/store/dddd-etc.drv" {
			t.Fatalf("%s: attempted %s", name, got)
		}
		// Without the failed download the tool was attempted too.
		got = strings.Join(attemptedDrvs(pending, nil, facts), " ")
		if got != "/nix/store/aaaa-linux.drv /nix/store/dddd-etc.drv /nix/store/eeee-tool.drv" {
			t.Fatalf("%s: attempted without fetches %s", name, got)
		}
	}
	if _, err := parseDerivations([]byte("{}")); err == nil {
		t.Fatal("an empty listing passed")
	}
}

func TestAttemptedDerivationsByOutput(t *testing.T) {
	// The tool takes only the library's out, present here; the docs take its
	// doc output, whose download failed. The package takes a fixed-output
	// source, whose path Nix gives only in env, and that download failed too.
	listing := `{"derivations":{
		"aaaa-tool.drv":{"inputs":{"drvs":{"yyyy-lib.drv":{"outputs":["out"],"dynamicOutputs":{}}},"srcs":[]},"outputs":{"out":{"path":"aaaa-tool"}}},
		"bbbb-docs.drv":{"inputs":{"drvs":{"yyyy-lib.drv":{"outputs":["doc"],"dynamicOutputs":{}}},"srcs":[]},"outputs":{"out":{"path":"bbbb-docs"}}},
		"cccc-pkg.drv":{"inputs":{"drvs":{"ffff-src.drv":{"outputs":["out"],"dynamicOutputs":{}}},"srcs":[]},"outputs":{"out":{"path":"cccc-pkg"}}},
		"yyyy-lib.drv":{"inputs":{"drvs":{},"srcs":[]},"outputs":{"out":{"path":"yyyy-lib"},"doc":{"path":"yyyy-lib-doc"}}},
		"ffff-src.drv":{"inputs":{"drvs":{},"srcs":[]},"outputs":{"out":{"method":"nar","hash":"sha256-x"}},"env":{"out":"/nix/store/ffff-src"}}},"version":4}`
	facts, err := parseDerivations([]byte(listing))
	if err != nil {
		t.Fatal(err)
	}
	pending := []string{"/nix/store/aaaa-tool.drv", "/nix/store/bbbb-docs.drv", "/nix/store/cccc-pkg.drv"}
	fetches := []string{"/nix/store/yyyy-lib-doc", "/nix/store/ffff-src"}
	if got := strings.Join(attemptedDrvs(pending, fetches, facts), " "); got != "/nix/store/aaaa-tool.drv" {
		t.Fatalf("attempted %s", got)
	}
}
