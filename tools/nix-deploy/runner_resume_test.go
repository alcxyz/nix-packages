package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nix-deploy/bootcheck"
	"nix-deploy/k8snode"
)

// Execute the real host scripts against a synthetic system manager and tmpfs
// tree. The closed command path cannot reach a real systemctl, sudo or SSH.
type runnerResumeFixture struct {
	t                       *testing.T
	root, bin, base, helper string
}

func newRunnerResumeFixture(t *testing.T) *runnerResumeFixture {
	t.Helper()
	f := &runnerResumeFixture{t: t, root: t.TempDir()}
	f.bin = filepath.Join(f.root, "bin")
	f.base = filepath.Join(f.root, "run", "nixfleet-reboot-runners")
	f.helper = filepath.Join(f.bin, "nixfleet-runner-status")
	for _, path := range []string{f.bin, filepath.Dir(f.base)} {
		if err := os.MkdirAll(path, 0755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"sh", "mktemp", "rm", "rmdir", "cat", "mv", "chmod", "awk"} {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(path, filepath.Join(f.bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	stub := `import fcntl, json, os, pathlib, subprocess, sys
root=pathlib.Path(os.environ['RUNNER_FIXTURE'])
name=pathlib.Path(sys.argv[0]).name
args=[a for a in sys.argv[1:] if a != '--']
if name=='flock':
    assert args[:2]==['-w','10']
    with open(args[2],'a') as lock:
        try: fcntl.flock(lock,fcntl.LOCK_EX|fcntl.LOCK_NB)
        except BlockingIOError: sys.exit(75)
        sys.exit(subprocess.run(args[3:]).returncode)
states=json.loads((root/'states').read_text())
mode=(root/'mode').read_text()
def save(): (root/'states').write_text(json.dumps(states))
if name=='ps':
    if mode=='ps-fails': sys.exit(1)
    if mode=='ps-empty': sys.exit(0)
    if mode=='shutdown-process': print('systemctl systemctl reboot')
    else: print('init /sbin/init')
elif name=='nixfleet-runner-status':
    if mode=='bad-report': sys.exit(1)
    print('version\t1')
    if mode!='no-restart-unit': print('restart-unit\tforgejo-runner-restart.service\t3600')
    print('hold\tactive\t'+('game' if mode=='hold' else 'none'))
    if mode=='restart': print('restart\tactivating\twaiting\t1\t-\t-')
    for unit,state in states.items():
        reason='-' if state=='active' else 'stopped by hand or by a reboot run; nothing starts it again'
        if mode=='gate' and state!='active': reason='its start gate refused the last start; the runner hold retries a held runner'
        print('\t'.join(['unit','runner',unit,state,'running' if state=='active' else 'dead','no','-','ci-fast',reason]))
elif args[0]=='show':
    unit=args[-1]
    if mode=='inspect-fails': sys.exit(1)
    if '--property=LoadState' in args:
        print('loaded' if unit in states else 'not-found'); sys.exit(0)
    if unit=='forgejo-runner-restart.service':
        print('activating' if mode=='restart-starting-no-record' else 'inactive'); sys.exit(0)
    print(('active' if mode=='shutdown-target' else 'inactive') if unit=='shutdown.target' else states[unit])
elif args[0]=='list-jobs':
    if mode=='jobs': print('42 reboot.target start waiting')
elif args[0] in ('stop','start'):
    with (root/'calls').open('a') as out: out.write(' '.join(args)+'\n')
    for unit in args[1:]:
        if args[0]=='start' and mode=='gate-at-start': continue
        states[unit]='inactive' if args[0]=='stop' else 'active'
    save()
else: raise AssertionError(args)
`
	for _, name := range []string{"flock", "systemctl", "ps", "nixfleet-runner-status"} {
		if err := os.WriteFile(filepath.Join(f.bin, name), []byte("#!"+python+"\n"+stub), 0755); err != nil {
			t.Fatal(err)
		}
	}
	f.write("mode", "none")
	f.write("states", `{"forgejo-runner-a.service":"active","forgejo-runner-b.service":"inactive"}`)
	f.write("calls", "")
	t.Setenv("RUNNER_FIXTURE", f.root)
	return f
}

func (f *runnerResumeFixture) write(path, value string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.root, path), []byte(value), 0644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *runnerResumeFixture) script(script string) (string, int) {
	f.t.Helper()
	script = strings.ReplaceAll(script, runnerRebootDir, f.base)
	script = strings.ReplaceAll(script, runnerStatusCommand, f.helper)
	script = strings.ReplaceAll(script, "/run/systemd/shutdown/scheduled", filepath.Join(f.root, "scheduled"))
	cmd := exec.Command(filepath.Join(f.bin, "sh"), "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+f.bin)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	if result, ok := err.(*exec.ExitError); ok {
		return string(out), result.ExitCode()
	}
	f.t.Fatal(err)
	return "", 1
}

const resumeOwner = "0123456789abcdef0123456789abcdef"

func (f *runnerResumeFixture) stop() {
	f.t.Helper()
	info := reservationInfo{Controller: "controller", ControllerEndpoint: "root@controller", PID: 123, Started: "fixture", PIDNamespace: "fixture"}
	// Deliberately include an inactive unit, simulating a stale plan. The host
	// must check actual state rather than claim a hand-stopped unit.
	out, status := f.script(runnerStopScript(resumeOwner, info, []string{"forgejo-runner-a.service", "forgejo-runner-b.service"}))
	if status != 0 {
		f.t.Fatalf("stop: %d %s", status, out)
	}
	data, err := os.ReadFile(filepath.Join(f.base, "units"))
	if err != nil || string(data) != "forgejo-runner-a.service\n" {
		f.t.Fatalf("recorded: %q %v", data, err)
	}
	stat, err := os.Stat(f.base)
	if err != nil || stat.Mode().Perm() != 0755 {
		f.t.Fatalf("status cannot read stop record: %v", err)
	}
}

func TestResumeUnknownRebootOutcome(t *testing.T) {
	f := newRunnerResumeFixture(t)
	f.stop()
	output, status := f.script(runnerRebootReadScript)
	if status != 0 {
		t.Fatal(output)
	}
	r := addRunnerRebootStop(parseRunners(runnerReport([]string{strings.Replace(primaryOK, "forgejo-actions-runner.service\tactive\trunning", "forgejo-runner-a.service\tinactive\tdead", 1)}, "")), output)
	var table bytes.Buffer
	printRunners(&table, nil, []hostStatus{{Host: "node-a", Reachable: true, Runners: r}}, "/inventory with spaces.json")
	if !strings.Contains(table.String(), unknownRunnerReboot) || !strings.Contains(table.String(), "nixfleet runners resume 'node-a' --config '/inventory with spaces.json'") {
		t.Fatal(table.String())
	}
	if r.RebootStop == nil || r.RebootStop.Controller.Controller != "controller" {
		t.Fatalf("record: %+v", r)
	}
	out, status := f.script(runnerStartScript(resumeOwner))
	if status != 0 {
		t.Fatalf("resume: %d %s", status, out)
	}
	calls, _ := os.ReadFile(filepath.Join(f.root, "calls"))
	if string(calls) != "stop forgejo-runner-a.service\nstart forgejo-runner-a.service\n" {
		t.Fatal(string(calls))
	}
	if _, err := os.Stat(f.base); !os.IsNotExist(err) {
		t.Fatal("stop record remains", err)
	}
	// A repeated resume is harmless, including after an acknowledgement loss.
	if out, status := f.script(runnerStartScript(resumeOwner)); status != 0 {
		t.Fatal(out)
	}
}

func TestRunnerResumeRefusals(t *testing.T) {
	for _, mode := range []string{"scheduled", "jobs", "shutdown-target", "shutdown-process", "ps-empty", "ps-fails", "inspect-fails", "hold", "gate", "gate-at-start", "restart", "restart-starting-no-record", "bad-report", "owner-changed"} {
		t.Run(mode, func(t *testing.T) {
			f := newRunnerResumeFixture(t)
			f.stop()
			f.write("mode", mode)
			if mode == "scheduled" {
				f.write("scheduled", "USEC=1")
			}
			owner := resumeOwner
			if mode == "owner-changed" {
				owner = strings.Repeat("f", 32)
			}
			out, status := f.script(runnerStartScript(owner))
			if status == 0 || (mode != "inspect-fails" && !strings.Contains(out, "refused")) {
				t.Fatalf("admitted: %d %s", status, out)
			}
			if _, err := os.Stat(f.base); err != nil {
				t.Fatal("lost recovery record", err)
			}
			calls, _ := os.ReadFile(filepath.Join(f.root, "calls"))
			if mode != "gate-at-start" && strings.Contains(string(calls), "\nstart ") {
				t.Fatal("attempted a refused start", string(calls))
			}
		})
	}
}

func TestRebootClearsRunnerStop(t *testing.T) {
	f := newRunnerResumeFixture(t)
	f.stop()
	// A synthetic real boot empties /run and systemd starts enabled units.
	if err := os.RemoveAll(filepath.Dir(f.base)); err != nil {
		t.Fatal(err)
	}
	f.write("states", `{"forgejo-runner-a.service":"active","forgejo-runner-b.service":"active"}`)
	out, status := f.script(runnerRebootReadScript)
	if status != 0 || strings.Contains(out, "runner-reboot.") {
		t.Fatalf("stale reboot state: %d %s", status, out)
	}
	if addRunnerRebootStop(nil, out) != nil {
		t.Fatal("stale record reported")
	}
	// No startup migration or manual record deletion is required on a boot.
}

func TestRunnerResumeReservationAndController(t *testing.T) {
	d, _, shared := reservationFixture(t)
	r := &runnerStatus{RebootStop: &runnerRebootStop{Owner: resumeOwner, Controller: reservationInfo{Controller: "node-b", ControllerEndpoint: "root@node-b", PID: 123}}}
	state := hostStatus{Host: "node-a", Reachable: true, Runners: r}
	s := &statusCheck{inv: d.inv, self: d.self, run: func(time.Duration, ...string) (string, string, int) { t.Fatal("unsafe start"); return "", "", 1 }}
	active, err := d.reserveGroup(&serialGroup{Hosts: []string{"node-a"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := resumeHostRunners(d, s, state); err == nil || !strings.Contains(err.Error(), "blocked by reservation") {
		t.Fatalf("reservation bypass: %v", err)
	}
	if !active.release() {
		t.Fatal("release")
	}
	for _, controller := range []string{"busy", "unknown"} {
		t.Setenv("CONTROLLER_STATE", controller)
		if err := resumeHostRunners(d, s, state); err == nil || !strings.Contains(err.Error(), "reboot controller") {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(shared, reservationKey("node-a"))); !os.IsNotExist(err) {
			t.Fatal("refusal leaked reservation")
		}
	}
	t.Setenv("CONTROLLER_STATE", "gone")
	t.Setenv("WORK_STATE", "busy")
	if err := resumeHostRunners(d, s, state); err == nil || !strings.Contains(err.Error(), "controller work is not idle") {
		t.Fatalf("orphan reboot child admitted: %v", err)
	}
	t.Setenv("WORK_STATE", "idle")
	f := newRunnerResumeFixture(t)
	f.stop()
	s.run = func(_ time.Duration, argv ...string) (string, string, int) {
		// Execute the actual generated host command locally against the fixture.
		if argv[0] != "ssh" {
			t.Fatal(argv)
		}
		out, status := f.script(argv[len(argv)-1])
		return out, "", status
	}
	if err := resumeHostRunners(d, s, state); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(shared, reservationKey("node-a"))); !os.IsNotExist(err) {
		t.Fatal("successful resume leaked reservation")
	}
}

func TestRunnerResumeUncertainStartRetainsReservation(t *testing.T) {
	for _, status := range []int{0, 124, 255} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			d, _, shared := reservationFixture(t)
			state := hostStatus{Host: "node-a", Reachable: true, Runners: &runnerStatus{RebootStop: &runnerRebootStop{
				Owner: resumeOwner, Controller: reservationInfo{Controller: "node-b", ControllerEndpoint: "root@node-b", PID: 123},
			}}}
			s := &statusCheck{inv: d.inv, self: d.self, run: func(time.Duration, ...string) (string, string, int) { return "", "", status }}
			if err := resumeHostRunners(d, s, state); err == nil || !strings.Contains(err.Error(), "protection retained; next: nixfleet reservations release") {
				t.Fatalf("lost protection after uncertain start: %v", err)
			}
			if _, err := os.Stat(filepath.Join(shared, reservationKey("node-a"))); err != nil {
				t.Fatal("uncertain resume lost reservation", err)
			}
		})
	}
}

func TestRebootStopCannotOverwriteRecovery(t *testing.T) {
	f := newRunnerResumeFixture(t)
	f.stop()
	out, status := f.script(runnerStopScript(strings.Repeat("f", 32), reservationInfo{}, []string{"forgejo-runner-b.service"}))
	if status == 0 || !strings.Contains(out, "previous reboot run") {
		t.Fatal(status, out)
	}
}

func TestRebootUnknownOutcomePublishesRecoverySet(t *testing.T) {
	f := newRunnerResumeFixture(t)
	fake := newRebootFake(t)
	fleet := &fleetReboot{inv: fake.inv, self: fake.self, out: io.Discard, status: &statusCheck{}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := fleet.newHostReboot(ctx, nil, &rebootTarget{host: "host-a", runners: []string{"forgejo-runner-a.service", "forgejo-runner-b.service"}})
	h.runner.Fake = func(c k8snode.Command) k8snode.Result {
		out, status := f.script(c.Argv[len(c.Argv)-1])
		return k8snode.Result{Stdout: out, Status: status}
	}
	if err := h.stopRunners(); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := h.interrupted(); err == nil {
		t.Fatal("unknown outcome succeeded")
	}
	out, status := f.script(runnerRebootReadScript)
	r := addRunnerRebootStop(nil, out)
	if status != 0 || r == nil || r.RebootStop == nil || len(r.RebootStop.Units) != 1 || r.RebootStop.Units[0] != "forgejo-runner-a.service" {
		t.Fatalf("unknown outcome lost exact recovery set: %d %+v", status, r)
	}
	// A second reboot with an all-inactive plan must refuse this record too.
	h.ctx = context.Background()
	h.target.runners = nil
	if err := h.stopRunners(); err == nil {
		t.Fatal("all-inactive reboot bypassed the outstanding record")
	}
	calls, _ := os.ReadFile(filepath.Join(f.root, "calls"))
	if strings.Contains(string(calls), "\nstart ") {
		t.Fatal("unknown outcome automatically started a runner")
	}
}

func TestResumeArguments(t *testing.T) {
	options, err := parseRunnersArgs([]string{"resume", "--all", "--config", "fleet.json"})
	if err != nil || options.command != "resume" || !options.all {
		t.Fatal(options, err)
	}
	for _, args := range [][]string{{"resume", "--yes"}, {"resume", "--dry-run"}, {"resume", "--all", "node-a"}} {
		if _, err := parseRunnersArgs(args); err == nil {
			t.Fatalf("accepted: %v", args)
		}
	}
}

func TestRunnerRecordRemoteQuoting(t *testing.T) {
	f := newRunnerResumeFixture(t)
	f.stop()
	// Use the same nested login-shell quoting as statusCheck.readHost.
	out, code := f.script(bootcheck.RemoteCommand(runnerRebootReadScript))
	r := addRunnerRebootStop(nil, out)
	if code != 0 || r == nil || r.Error != "" || r.RebootStop == nil || len(r.RebootStop.Units) != 1 {
		t.Fatalf("remote record broken: %d %q %+v", code, out, r)
	}
}

func TestRunnerRecordRetirementSurvivesInterruptedDeletion(t *testing.T) {
	f := newRunnerResumeFixture(t)
	f.stop()
	// Fail deletion after the rename, modeling an interrupted cleanup command.
	if err := os.Remove(filepath.Join(f.bin, "rm")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.bin, "rm"), []byte("#!/bin/sh\nexit 1\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if out, code := f.script(runnerStartScript(resumeOwner)); code == 0 {
		t.Fatal("cleanup unexpectedly completed", out)
	}
	if _, err := os.Stat(f.base); !os.IsNotExist(err) {
		t.Fatal("interrupted deletion left a blocking record", err)
	}
	out, code := f.script(bootcheck.RemoteCommand(runnerRebootReadScript))
	if code != 0 || addRunnerRebootStop(nil, out) != nil {
		t.Fatal("retired record reported", out)
	}
	if out, code := f.script(runnerStartScript(resumeOwner)); code != 0 {
		t.Fatal("retry blocked", out)
	}
}

func TestRunnerStopRejectsUnsupportedHostBeforeDraining(t *testing.T) {
	f := newRunnerResumeFixture(t)
	if err := os.Remove(f.helper); err != nil {
		t.Fatal(err)
	}
	out, code := f.script(runnerStopScript(resumeOwner, reservationInfo{}, []string{"forgejo-runner-a.service"}))
	if code == 0 || !strings.Contains(out, "required before draining") {
		t.Fatal(code, out)
	}
	if _, err := os.Stat(f.base); !os.IsNotExist(err) {
		t.Fatal("unsupported host acquired stop record", err)
	}
	calls, _ := os.ReadFile(filepath.Join(f.root, "calls"))
	if len(calls) != 0 {
		t.Fatal("unsupported host stopped runners", string(calls))
	}
}

func TestRunnerResumeRemovedUnitAndNoRestartUnit(t *testing.T) {
	for _, removed := range []bool{false, true} {
		t.Run(fmt.Sprint(removed), func(t *testing.T) {
			f := newRunnerResumeFixture(t)
			f.stop()
			f.write("mode", "no-restart-unit")
			if removed {
				f.write("states", `{"forgejo-runner-b.service":"inactive"}`)
			}
			if out, code := f.script(runnerStartScript(resumeOwner)); code != 0 {
				t.Fatal(code, out)
			}
			calls, _ := os.ReadFile(filepath.Join(f.root, "calls"))
			if strings.Contains(string(calls), "start forgejo-runner-b") || removed && strings.Contains(string(calls), "\nstart ") {
				t.Fatal("started removed or hand-stopped unit", string(calls))
			}
		})
	}
}

func TestResumeLocalSudoSelection(t *testing.T) {
	state := hostStatus{Host: "target", Runners: &runnerStatus{RebootStop: &runnerRebootStop{Controller: reservationInfo{Controller: "controller"}}}}
	if !resumeUsesLocalSudo("controller", []hostStatus{state}) || !resumeUsesLocalSudo("target", []hostStatus{state}) || resumeUsesLocalSudo("elsewhere", []hostStatus{state}) {
		t.Fatal("local sudo paths misclassified")
	}
	state.Runners.RebootStop = nil
	if resumeUsesLocalSudo("controller", []hostStatus{state}) {
		t.Fatal("no-op resume requested sudo")
	}
}
