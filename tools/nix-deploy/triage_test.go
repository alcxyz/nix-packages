package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// makeRun writes a synthetic run directory under runs; a nil report leaves
// report.json out.
func makeRun(t *testing.T, runs, id string, failed bool, rep *runReport, logs map[string]string) string {
	t.Helper()
	dir := filepath.Join(runs, id)
	if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	ended := time.Date(2026, 1, 2, 3, 5, 0, 0, time.UTC)
	dirty := true
	info := runInfo{ID: id, Version: "0.0.0-test", Started: ended.Add(-time.Minute), Ended: &ended,
		Args: []string{"--all"}, FlakeDir: "/flake", GitRev: "abc123", GitDirty: &dirty,
		LockSHA256: "new", StartLockSHA256: "old", Force: true, Failed: &failed}
	write := func(name string, value any) {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("run.json", info)
	if rep != nil {
		write("report.json", rep)
	}
	for name, text := range logs {
		if err := os.WriteFile(filepath.Join(dir, "logs", name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func stateRuns(t *testing.T) string {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	return filepath.Join(state, "nix-deploy", "runs")
}

func TestTriageSelectsRuns(t *testing.T) {
	runs := stateRuns(t)
	rep := &runReport{Failed: true}
	makeRun(t, runs, "20260102T030400.000000Z-1", true, rep, nil)
	want := makeRun(t, runs, "20260102T030500.000000Z-2", true, rep, nil)
	makeRun(t, runs, "20260102T030600.000000Z-3", false, rep, nil)

	for _, name := range []string{"", "20260102T030500.000000Z-2", want} {
		got, err := selectRun(runs, name, false)
		if err != nil || got != want {
			t.Fatalf("selectRun(%q) = %q, %v; want %q", name, got, err, want)
		}
	}
	if _, err := selectRun(runs, "20990101T000000.000000Z-1", false); err == nil || !strings.Contains(err.Error(), "no run") {
		t.Fatalf("a missing run gave %v", err)
	}
}

func TestTriageNoFailedRun(t *testing.T) {
	runs := stateRuns(t)
	makeRun(t, runs, "20260102T030600.000000Z-3", false, &runReport{}, nil)
	if _, err := selectRun(runs, "", false); err == nil || !strings.Contains(err.Error(), "no failed run") {
		t.Fatalf("got %v", err)
	}
	if status := runTriage(nil); status != 1 {
		t.Fatalf("runTriage exited %d", status)
	}
}

func TestTriageRefusesRunInProgressOrWithoutReport(t *testing.T) {
	runs := stateRuns(t)
	dir := makeRun(t, runs, "20260102T030500.000000Z-2", true, &runReport{}, nil)
	lock, err := os.OpenFile(filepath.Join(dir, runLockName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if _, err := writeTriageContext(dir, nil); err == nil || !strings.Contains(err.Error(), "still going on") {
		t.Fatalf("a held run gave %v", err)
	}
	lock.Close()

	bare := makeRun(t, runs, "20260102T030600.000000Z-3", true, nil, nil)
	if _, err := writeTriageContext(bare, nil); err == nil || !strings.Contains(err.Error(), "no report.json") {
		t.Fatalf("a run without a report gave %v", err)
	}
}

func TestFirstError(t *testing.T) {
	text := "evaluating\n\x1b[31;1merror:\x1b[0m\n       … while calling\n\n         at /x.nix:1:2:\n       error: inner\nwarning: next\nerror: second\n"
	block, cut := firstError(text)
	want := []string{"error:", "       … while calling", "", "         at /x.nix:1:2:", "       error: inner"}
	if cut || strings.Join(block, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got %q, cut %v", block, cut)
	}

	long := "error: long\n" + strings.Repeat("   frame\n", 100)
	block, cut = firstError(long)
	if !cut || len(block) != maxErrorLines {
		t.Fatalf("got %d lines, cut %v", len(block), cut)
	}
	if block, _ := firstError("no errors here\n  error: indented only\n"); block != nil {
		t.Fatalf("got %q", block)
	}
}

func TestErrorFiles(t *testing.T) {
	hash := strings.Repeat("a", 32)
	block := []string{
		"error: The option `x' does not exist. Definition values:",
		"       - In `/nix/store/" + hash + "-source/hosts/one.nix'",
		"       at «github:example/repo/0123»/lib/modules.nix:12:3:",
		"       at /nix/store/" + hash + "-source/modules/two.nix:7:5:",
		"       at «github:example/repo/0123»/lib/modules.nix:12:3:",
		"       see /nix/store/" + hash + "-source/hosts/one.nix.",
		"       at /nix/store/" + hash + "-module.nix:12:3:",
	}
	got := errorFiles(block)
	want := []string{
		"/nix/store/" + hash + "-source/hosts/one.nix",
		"«github:example/repo/0123»/lib/modules.nix:12:3",
		"/nix/store/" + hash + "-source/modules/two.nix:7:5",
		"/nix/store/" + hash + "-module.nix:12:3",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got %q", got)
	}
}

func TestTriageContext(t *testing.T) {
	runs := stateRuns(t)
	hash := strings.Repeat("b", 32)
	rep := &runReport{Failed: true, Hosts: []hostReport{
		{Host: "alpha", System: phaseReport{Status: "failed", EvaluateSeconds: 1}, HomeManager: phaseReport{Status: "failed", ActivateSeconds: 2}},
		{Host: "beta", System: phaseReport{Status: "failed", EvaluateSeconds: 1}, HomeManager: phaseReport{Status: "ok"}},
	}}
	dir := makeRun(t, runs, "20260102T030500.000000Z-2", true, rep, map[string]string{
		"system-alpha-evaluate.log": "error: attribute 'gone' missing\n       at /nix/store/" + hash + "-source/hosts/alpha.nix:3:4:\nlater\n",
		"build.log":                 "command: nix build\nexit status: 1\nfailed targets: system:beta\n",
		"build-pkg-1.0.log":         "compiling\nerror: builder failed\n",
	})
	path, err := writeTriageContext(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	stat, err := os.Stat(path)
	if err != nil || stat.Mode().Perm() != 0o600 {
		t.Fatalf("context.md: %v, %v", stat, err)
	}
	data, _ := os.ReadFile(path)
	text := string(data)
	for _, want := range []string{
		"- Deploy arguments: `--all`",
		"- Git revision: abc123, tracked files differed from it",
		"- Modes: force",
		"Nix rewrote the lock during evaluation",
		"- alpha system: evaluate failed",
		"- alpha home-manager: activation or its setup failed",
		"- beta system: build failed",
		"error: attribute 'gone' missing",
		"- /nix/store/" + hash + "-source/hosts/alpha.nix:3:4",
		"No activate log: the activation step or the setup just before it failed",
		"## Build errors",
		filepath.Join(dir, "logs", "build-pkg-1.0.log"),
		"error: builder failed",
		"## Lock diff",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("context.md lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "later") {
		t.Errorf("the error block ran past its end:\n%s", text)
	}
}

func TestTriageBuildLogsUnderBracketPath(t *testing.T) {
	runs := filepath.Join(t.TempDir(), "[archive]")
	rep := &runReport{Failed: true, Hosts: []hostReport{{Host: "beta", System: phaseReport{Status: "failed"}}}}
	dir := makeRun(t, runs, "20260102T030500.000000Z-2", true, rep, map[string]string{
		"build.log":         "failed targets: system:beta\n",
		"build-pkg-1.0.log": "error: builder failed\n",
	})
	path, err := writeTriageContext(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "error: builder failed") {
		t.Fatalf("the build log was missed:\n%s", data)
	}
}

func TestTriageUnknownStepListsLogsAndTail(t *testing.T) {
	runs := stateRuns(t)
	rep := &runReport{Failed: true, Hosts: []hostReport{{Host: "gamma", System: phaseReport{Status: "failed", EvaluateSeconds: 1}}}}
	dir := makeRun(t, runs, "20260102T030500.000000Z-2", true, rep, map[string]string{
		"system-gamma-evaluate.log": "evaluated\n\n",
		"system-gamma-activate.log": "copying\n\x1b[1mdeploy: nix: not found\x1b[0m\n\n",
	})
	path, err := writeTriageContext(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	text := string(data)
	for _, want := range []string{
		"- gamma system: activate failed",
		"- " + filepath.Join(dir, "logs", "system-gamma-evaluate.log"),
		"- " + filepath.Join(dir, "logs", "system-gamma-activate.log"),
		"the last 2 non-empty lines instead:\n\n````text\ncopying\ndeploy: nix: not found\n````",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("context.md lacks %q:\n%s", want, text)
		}
	}

	// Without an activate log the step is unknown; the evaluate log's tail
	// is shown.
	os.Remove(filepath.Join(dir, "logs", "system-gamma-activate.log"))
	if _, err := writeTriageContext(dir, nil); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	text = string(data)
	for _, want := range []string{
		"- gamma system: failed step unknown",
		"- " + filepath.Join(dir, "logs", "system-gamma-evaluate.log"),
		"the last 1 non-empty lines instead:\n\n````text\nevaluated\n````",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("context.md lacks %q:\n%s", want, text)
		}
	}
}
