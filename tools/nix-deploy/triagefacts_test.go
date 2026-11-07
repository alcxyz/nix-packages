package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// goTestLog is shaped after the Darwin check phase of nixfleet 0.32.0 in
// session 3 of nix-packages#673: one cause behind most failures, worded
// differently by the tests that hit it, and the test binary's own stderr at
// the end.
const goTestLog = `Running phase: checkPhase
@nix { "action": "setPhase", "phase": "checkPhase" }
--- FAIL: TestSharedReservationsAcrossControllers (0.04s)
    coordination_test.go:30: cannot establish controller process identity
--- FAIL: TestReservationRetainedAfterAmbiguousAcquisition (0.04s)
    coordination_test.go:95: reservation lost on disconnect: [], open /build/TestX/001/shared: no such file or directory
--- FAIL: TestKnownContentionRollsBackPartialAcquisition (0.03s)
    coordination_test.go:113: cannot establish controller process identity
deploy: run record: /build/state/nix-deploy/runs/20261105T223437.148307Z-27147
--- FAIL: TestRebootHookEvents (0.02s)
    hook_test.go:44: status 1:
        plan: 3 host(s) to reboot in 2 group(s)
        [host-a]	==> error: cannot record reboot controller: cannot establish controller process identity
        host-a: failed: cannot record reboot controller: cannot establish controller process identity
--- FAIL: TestRebootRunners (0.06s)
    --- FAIL: TestRebootRunners/stop_fails (0.01s)
        reboot_test.go:900: cannot record reboot controller: cannot establish controller process identity
nixfleet triage: not launching an agent: the inventory could not be read
FAIL
FAIL	nix-deploy	62.568s
FAIL
`

func TestGoTestFailuresGroupByCause(t *testing.T) {
	f := goTestFailures(goTestLog)
	if len(f.tests) != 6 {
		t.Fatalf("tests %v", f.tests)
	}
	if len(f.groups) == 0 || f.groups[0].key != "cannot establish controller process identity" || len(f.groups[0].tests) != 4 {
		t.Fatalf("groups %+v", f.groups)
	}
	var out strings.Builder
	if !writeLogErrorText(t, &out, goTestLog) {
		t.Fatal("the test failures were not found")
	}
	text := out.String()
	for _, want := range []string{
		"Go test failures: 6 \"--- FAIL:\" lines (1 of them subtests).",
		"- 4 test(s): cannot establish controller process identity (for example `coordination_test.go:30: cannot establish controller process identity` in `TestSharedReservationsAcrossControllers`)",
		"- 1 test(s): no such file or directory",
		"Failing tests: TestSharedReservationsAcrossControllers, ",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "not launching an agent") {
		t.Errorf("the tail was shown as if it explained the failure:\n%s", text)
	}
}

// writeLogErrorText runs writeLogError on text written to a log file.
func writeLogErrorText(t *testing.T, out *strings.Builder, text string) bool {
	t.Helper()
	log := filepath.Join(t.TempDir(), "x.log")
	if err := os.WriteFile(log, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return writeLogError(out, log)
}

func TestUnitFailuresInActivationLog(t *testing.T) {
	// Shaped after session 1: no line starts with "error:", and the tail
	// names only the generator, while the real impact is further up.
	log := `activating the configuration...
setting up /etc...
restarting the following units: nvidia-container-toolkit-cdi-generator.service
Failed to start k3s.service - Lightweight Kubernetes.
Job for k3s.service failed because the control process exited with error code.
Failed to start wolf-runtime-images.service.
warning: the following units failed: k3s.service, nvidia-container-toolkit-cdi-generator.service, wolf-runtime-images.service
Failed to start k3s.service - Lightweight Kubernetes.
× nvidia-container-toolkit-cdi-generator.service - Container Device Interface (CDI) for Nvidia generator
     Active: failed (Result: exit-code)
`
	var out strings.Builder
	if !writeLogErrorText(t, &out, log) {
		t.Fatal("no finding")
	}
	want := "Units that failed, as the log reports them:\n\n````text\n" +
		"Failed to start k3s.service - Lightweight Kubernetes.\n" +
		"Job for k3s.service failed because the control process exited with error code.\n" +
		"Failed to start wolf-runtime-images.service.\n" +
		"warning: the following units failed: k3s.service, nvidia-container-toolkit-cdi-generator.service, wolf-runtime-images.service\n````"
	if !strings.Contains(out.String(), want) {
		t.Fatalf("got:\n%s", out.String())
	}
}

func TestFixedOutputRebuildAndAttemptedDerivations(t *testing.T) {
	runs := stateRuns(t)
	store := t.TempDir()
	input := lockedStorePath(hashB, "source")
	// A fixed-output derivation that names the source of one input, and
	// nixfleet built for a Mac, which names none.
	fod := filepath.Join(store, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-opencode-node_modules-2.2.5.drv")
	os.WriteFile(fod, []byte(`Derive([("out","/nix/store/zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz-opencode-node_modules-2.2.5","r:sha256","0d5f60")],[],["`+input+`"],"x86_64-linux","/bin/sh",[],[("src","`+input+`/packages/opencode"),("system","x86_64-linux")])`), 0o600)
	tool := filepath.Join(store, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-nixfleet-0.32.0.drv")
	os.WriteFile(tool, []byte(`Derive([("out","/nix/store/yyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyy-nixfleet-0.32.0","","")],[],["/nix/store/xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx-nix-deploy"],"aarch64-darwin","/bin/sh",[],[("system","aarch64-darwin")])`), 0o600)
	rep := &runReport{Failed: true, Hosts: []hostReport{{Host: "mac", HomeManager: phaseReport{Status: "failed"}}}}
	dir := makeRun(t, runs, "20260102T030500.000000Z-4", true, rep, map[string]string{
		"build.log": "failed targets: hm:mac\nattempted, with no pending input: 2\n" +
			attemptedFixedPrefix + fod + "\n" + attemptedPrefix + tool + "\n",
		"build-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-opencode-node_modules-2.2.5.log": "Running phase: installPhase\n",
		nixRebuildLog: "error: hash mismatch in fixed-output derivation '" + fod + "':\n         specified: sha256-AAAA\n            got:    sha256-BBBB\n",
	})
	newer := map[string]lockedInput{"opencode": {Type: "github", Owner: "example", Repo: "opencode", Rev: "bbbb", NarHash: hashB}}
	older := map[string]lockedInput{"opencode": {Type: "github", Owner: "example", Repo: "opencode", Rev: "aaaa", NarHash: hashA}}
	good := makeRun(t, runs, "20260102T030100.000000Z-1", false, &runReport{Hosts: []hostReport{{Host: "mac", HomeManager: phaseReport{Status: "ok"}}}}, nil)
	setRun(t, good, time.Date(2026, 1, 2, 3, 1, 0, 0, time.UTC), "/flake", "", lockData(t, older))
	// An earlier failed run of the same lock as the failed run; the good run
	// above used the older lock, so only this one is listed as the same lock.
	same := makeRun(t, runs, "20260102T030300.000000Z-2", true, &runReport{Failed: true, Hosts: []hostReport{{Host: "mac", HomeManager: phaseReport{Status: "failed"}}}}, nil)
	setRun(t, same, time.Date(2026, 1, 2, 3, 3, 0, 0, time.UTC), "/flake", "", lockData(t, newer))
	setRun(t, dir, time.Date(2026, 1, 2, 3, 5, 0, 0, time.UTC), "/flake", "", lockData(t, newer))

	path, err := writeTriageContext(dir, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	text := string(data)
	for _, want := range []string{
		"The failing package is nixfleet itself (nixfleet-0.32.0). This run used nixfleet 0.0.0-test",
		"An attempted derivation builds for aarch64-darwin. nix-packages builds natively only for x86_64-linux",
		"No error found: no line starts with \"error:\"",
		"Nix's own output from building the attempted fixed-output derivations again",
		"error: hash mismatch in fixed-output derivation",
		"- " + fod + " (fixed-output: Nix reports a hash mismatch itself, outside the builder's log; builds for x86_64-linux)",
		"  - Input `opencode` (upstream): changed since the baseline, aaaa -> bbbb\n    - compare: https://github.com/example/opencode/compare/aaaa...bbbb",
		"- " + tool + " (builds for aarch64-darwin)\n  - Inputs: no locked input's store path appears in it.",
		"## Earlier runs of this lock\n\nEarlier kept runs of this flake with the same lock, newest first:\n\n- 20260102T030300.000000Z-2: failed, `--all`",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("context.md lacks %q:\n%s", want, text)
		}
	}
	section := text[strings.Index(text, "## Earlier runs of this lock"):]
	if end := strings.Index(section[1:], "\n## "); end >= 0 {
		section = section[:end+1]
	}
	if strings.Contains(section, "20260102T030100.000000Z-1") {
		t.Errorf("the run with the older lock is listed as the same lock:\n%s", section)
	}
}

func TestIsNixfleet(t *testing.T) {
	for drv, want := range map[string]bool{
		"/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-nixfleet-0.32.0.drv":            true,
		"/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-nixfleet-0.32.0-go-modules.drv": true,
		"/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-nix-deploy-0.32.0.drv":          true,
		"/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-nixfleet-runner-status.drv":     false,
		"/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-nixfleet-boot-check.drv":        false,
		"/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-nixfleet.drv":                   false,
	} {
		if got := isNixfleet(drv); got != want {
			t.Errorf("isNixfleet(%s) = %v, want %v", drv, got, want)
		}
	}
}

func TestNotesSkipBuiltinSystem(t *testing.T) {
	var out strings.Builder
	writeNotes(&out, runInfo{Version: "0.0.0-test"}, nil, []drvFacts{{system: "builtin"}, {system: "x86_64-linux"}})
	if strings.Contains(out.String(), "builds for builtin") {
		t.Errorf("the builtin fetcher got a system note:\n%s", out.String())
	}
}

// A failed systemctl --failed must fail the script, not read as no failed
// units.
func TestFailedUnitsScriptListingFailure(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	bin := t.TempDir()
	for name, body := range map[string]string{
		"systemctl":  "echo 'Failed to connect to bus' >&2; exit 1",
		"journalctl": "exit 0",
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!"+sh+"\n"+body+"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(sh, "-c", failedUnitsScript)
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the script succeeded although systemctl failed: %q", out)
	}
	if !strings.Contains(string(out), "Failed to connect to bus") {
		t.Errorf("systemctl's error is not passed on: %q", out)
	}
}

func TestLockDiffAndTableCollapseSharedInputs(t *testing.T) {
	old := map[string]lockedInput{}
	new := map[string]lockedInput{}
	for _, name := range []string{"systems", "systems_2", "systems_3"} {
		old[name] = lockedInput{Type: "github", Owner: "nix-systems", Repo: "default", Rev: "aaaa", NarHash: hashA}
		new[name] = lockedInput{Type: "github", Owner: "nix-systems", Repo: "default", Rev: "bbbb", NarHash: hashB}
	}
	before, _ := lockInputs(lockData(t, old))
	after, _ := lockInputs(lockData(t, new))
	var out strings.Builder
	writeLockDiff(&out, before, after)
	if want := "- Changed `systems`, `systems_2`, `systems_3`: github:nix-systems/default\n  - locked: aaaa -> bbbb"; !strings.Contains(out.String(), want) {
		t.Errorf("diff lacks %q:\n%s", want, out.String())
	}
	out.Reset()
	// The same source fetched from a fork has the same store path but its
	// own source, so it keeps its own row.
	new["systems_fork"] = lockedInput{Type: "github", Owner: "example", Repo: "default", Rev: "bbbb", NarHash: hashB}
	after, _ = lockInputs(lockData(t, new))
	writeLockedInputs(&out, after, map[string]bool{}, nil, nil, "")
	if !strings.Contains(out.String(), "| systems, systems_2, systems_3 | no | github:nix-systems/default |") ||
		!strings.Contains(out.String(), "| systems_fork | no | github:example/default |") {
		t.Errorf("table:\n%s", out.String())
	}
	// Names with the same store path and source but different own marks:
	// the upstream ones share a row, the own one has its own.
	out.Reset()
	writeLockedInputs(&out, after, map[string]bool{"systems_2": true}, nil, nil, "")
	if !strings.Contains(out.String(), "| systems, systems_3 | no | github:nix-systems/default |") ||
		!strings.Contains(out.String(), "| systems_2 | yes | github:nix-systems/default |") {
		t.Errorf("table with an own input:\n%s", out.String())
	}
}

func TestOwnInputsFoldAbsentAndShowLocalIdentity(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	repo := t.TempDir()
	git := func(args ...string) {
		output, err := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=test", "-c", "user.email=test@example.invalid"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	git("init", "-q", "-b", "main")
	git("commit", "-q", "--allow-empty", "-m", "a")
	git("config", "--local", "user.name", "forgejo-actions")
	missing := filepath.Join(t.TempDir(), "missing")
	checkouts := map[string]string{"here": repo, "away": missing, "gone": missing}
	var out strings.Builder
	writeOwnInputs(&out, "Own inputs", lockData(t, map[string]lockedInput{}), checkouts)
	text := out.String()
	for _, want := range []string{
		"- Repository-local Git identity: user.name forgejo-actions.",
		"Not cloned on this machine (the checkout is not a directory): away, gone.",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "### away") {
		t.Errorf("an absent checkout has its own section:\n%s", text)
	}
}

func TestClosureDiffPutsKernelAndDriverFirst(t *testing.T) {
	log := filepath.Join(t.TempDir(), "system-node-closure.log")
	os.WriteFile(log, []byte("nix store diff-closures /nix/store/a-old /nix/store/b-new\n"+
		"firefox: 140.0 → 141.0, +1.2 MiB\n"+
		"linux: 7.3.0 → 7.3.1, +0.5 MiB\n"+
		"nvidia-x11: 580.178.04 → 580.196.07\n"), 0o600)
	var out strings.Builder
	writeClosureDiff(&out, log)
	want := "````text\nlinux: 7.3.0 → 7.3.1, +0.5 MiB\nnvidia-x11: 580.178.04 → 580.196.07\nfirefox: 140.0 → 141.0, +1.2 MiB\n````"
	if !strings.Contains(out.String(), want) {
		t.Fatalf("got:\n%s", out.String())
	}
}

func TestActivationFailureReadsLiveStateAndClosure(t *testing.T) {
	runs := stateRuns(t)
	rep := &runReport{Failed: true, Hosts: []hostReport{{Host: "node", System: phaseReport{Status: "failed", ActivateSeconds: 9}}}}
	dir := makeRun(t, runs, "20260102T030500.000000Z-4", true, rep, map[string]string{
		"system-node-activate.log": "Failed to start k3s.service\n",
		"system-node-closure.log":  "nix store diff-closures /nix/store/a /nix/store/b\nlinux: 7.3.0 → 7.3.1\n",
	})
	var asked []string
	readLiveState = func(configFile string, hosts []string) string {
		asked = append(asked, configFile+" "+strings.Join(hosts, " "))
		return "STATUS TABLE\n"
	}
	defer func() { readLiveState = defaultReadLiveState }()
	path, err := writeTriageContext(dir, nil, "/inventory.json")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	text := string(data)
	for _, want := range []string{
		"## Live state\n\nAn activation failed, so the host may be degraded now.",
		"STATUS TABLE",
		"Failed to start k3s.service",
		"Closure diff from the generation the host ran to the failed one",
		"linux: 7.3.0 → 7.3.1",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("context.md lacks %q:\n%s", want, text)
		}
	}
	if strings.Join(asked, ";") != "/inventory.json node" {
		t.Errorf("live state asked for %q", asked)
	}
}

func TestFailedUnitsTable(t *testing.T) {
	s := &statusCheck{self: "node", run: func(timeout time.Duration, argv ...string) (string, string, int) {
		if argv[0] != "sh" {
			t.Fatalf("this machine was read over %v", argv)
		}
		return "unit\tk3s.service\tWed 2026-11-04 10:01:00 CET\t2026-11-04T06:02:00+01:00\nunit\tx.service\t\t\n", "", 0
	}}
	got := s.failedUnits("node")
	for _, want := range []string{
		"| k3s.service | Wed 2026-11-04 10:01:00 CET | 2026-11-04T06:02:00+01:00 |",
		"| x.service | unknown | not found (or the journal is not readable) |",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("lacks %q:\n%s", want, got)
		}
	}
	s.run = func(timeout time.Duration, argv ...string) (string, string, int) { return "", "", 0 }
	if got := s.failedUnits("node"); !strings.HasPrefix(got, "No failed system units.") {
		t.Errorf("got %q", got)
	}
}

func TestParseDerivationsMarksFixedOutput(t *testing.T) {
	listing := `{"derivations":{
		"aaaa-src.drv":{"inputs":{"drvs":{},"srcs":[]},"outputs":{"out":{"method":"nar","hash":"sha256-x"}},"env":{"out":"/nix/store/aaaa-src"}},
		"bbbb-pkg.drv":{"inputs":{"drvs":{"aaaa-src.drv":{"outputs":["out"]}},"srcs":[]},"outputs":{"out":{"path":"bbbb-pkg"}}}},"version":4}`
	facts, err := parseDerivations([]byte(listing))
	if err != nil {
		t.Fatal(err)
	}
	if !facts["aaaa-src.drv"].fixed || facts["bbbb-pkg.drv"].fixed {
		t.Fatalf("facts %+v", facts)
	}
	older := `{"/nix/store/aaaa-src.drv":{"inputDrvs":{},"outputs":{"out":{"path":"/nix/store/aaaa-src","hashAlgo":"r:sha256","hash":"0d5f"}}}}`
	if facts, err := parseDerivations([]byte(older)); err != nil || !facts["aaaa-src.drv"].fixed {
		t.Fatalf("older format: %+v %v", facts, err)
	}
}

func TestClosureDiffNeedsBothGenerations(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	d := &deployer{record: startRunRecord(nil, t.TempDir(), false, false, false, 30)}
	r := newReport()
	ok := target{phase: "system", host: "fine"}
	unknown := target{phase: "system", host: "new"}
	absent := target{phase: "hm", host: "away"}
	built := target{phase: "system", host: "unbuilt"}
	retried := target{phase: "system", host: "retried"}
	for _, t := range []target{ok, unknown, absent, retried} {
		r.record(t.phase, t.host, "failed")
		r.addTime(t, "activate", time.Second)
	}
	r.results[ok.key()] = "ok"
	r.record(built.phase, built.host, "failed")
	d.notePlanned(ok, "/nix/store/a", "/nix/store/b")
	d.notePlanned(unknown, "", "/nix/store/b")
	d.notePlanned(absent, "/nix/store/missing-a", "/nix/store/missing-b")
	d.notePlanned(built, "/nix/store/a", "/nix/store/b")
	d.notePlanned(retried, "/nix/store/b", "/nix/store/b")
	pathExists = func(string) bool { return false }
	defer func() { pathExists = fileExists }()
	d.recordClosureDiffs(r)
	logs := filepath.Join(d.record.dir, "logs")
	if data, _ := os.ReadFile(filepath.Join(logs, "system-new-closure.log")); !strings.Contains(string(data), "is unknown") {
		t.Errorf("unknown running generation: %q", data)
	}
	if data, _ := os.ReadFile(filepath.Join(logs, "hm-away-closure.log")); !strings.Contains(string(data), "not in this machine's store: /nix/store/missing-a (the generation the host ran), /nix/store/missing-b (the failed generation).") {
		t.Errorf("absent paths: %q", data)
	}
	if data, _ := os.ReadFile(filepath.Join(logs, "system-retried-closure.log")); !strings.Contains(string(data), "already pointed at the failed generation") {
		t.Errorf("retried activation: %q", data)
	}
	for _, name := range []string{"system-fine-closure.log", "system-unbuilt-closure.log"} {
		if fileExists(filepath.Join(logs, name)) {
			t.Errorf("%s written for a target that did not fail in activation", name)
		}
	}
}

// Nix's raw log format under systemd marks lines with a severity, which must
// not hide the error line.
func TestLogErrorWithSeverityPrefix(t *testing.T) {
	var out strings.Builder
	log := "<6>building '/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-x.drv'...\n" +
		"<3>error: hash mismatch in fixed-output derivation '/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-x.drv':\n" +
		"<3>         specified: sha256-AAAA\n<3>            got:    sha256-BBBB\n"
	if !writeLogErrorText(t, &out, log) {
		t.Fatalf("no error found:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "First error:\n\n````text\nerror: hash mismatch") || !strings.Contains(out.String(), "got:    sha256-BBBB") {
		t.Errorf("the severity prefix hid the error:\n%s", out.String())
	}
}

// The platform comes from the ATerm field, so a derivation with structured
// attributes, whose system sits in escaped __json, still has one.
func TestDrvPlatform(t *testing.T) {
	structured := `Derive([("out","/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-herdr-1.0","","")],[("/nix/store/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-x.drv",["out"])],[],"aarch64-darwin","/bin/sh",["-c","echo \"a,b\""],[("__json","{\"system\":\"aarch64-darwin\",\"x\":\"(]\"}"),("out","/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-herdr-1.0")])`
	if got := drvPlatform(structured); got != "aarch64-darwin" {
		t.Errorf("drvPlatform(structured) = %q, want aarch64-darwin", got)
	}
	plain := `Derive([("out","/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-x","","")],[],[],"builtin","builtin:fetchurl",[],[("system","builtin")])`
	if got := drvPlatform(plain); got != "builtin" {
		t.Errorf("drvPlatform(plain) = %q, want builtin", got)
	}
	if got := drvPlatform("not a derivation"); got != "" {
		t.Errorf("drvPlatform(junk) = %q, want empty", got)
	}
}

// A long list of attempted derivations is capped in the context.
func TestWriteAttemptedCapsList(t *testing.T) {
	var attempted []attemptedDerivation
	for i := 0; i < 3; i++ {
		attempted = append(attempted, attemptedDerivation{drv: fmt.Sprintf("/nix/store/%032d-src-%d.drv", i, i), fixed: true})
	}
	var out strings.Builder
	writeAttempted(&out, attempted, 250, make([]drvFacts, len(attempted)), nil, nil, nil, nil)
	if !strings.Contains(out.String(), "- and 247 more; build.log lists them all.") {
		t.Errorf("no cap line:\n%s", out.String())
	}
}
