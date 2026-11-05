package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func reservationFixture(t *testing.T) (*deployer, *serialGroup, string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	os.Mkdir(bin, 0700)
	// Every SSH endpoint is a local synthetic stub. Authority operations use
	// a real kernel lock through Python's portable fcntl; probes return fixtures.
	stub := `#!/usr/bin/env python3
import fcntl, os, pathlib, subprocess, sys
if pathlib.Path(sys.argv[0]).name == 'bash' and sys.argv[1:2] != ['-s']:
    os.execv(REAL_BASH, [REAL_BASH, *sys.argv[1:]])
script=sys.stdin.read()
base=pathlib.Path(os.environ['RESERVATION_FIXTURE'])
if '# nixfleet activation probe' in script:
    state=os.environ.get('WORK_STATE','idle')
    host = sys.argv[-2].split('@')[-1] if pathlib.Path(sys.argv[0]).name == 'ssh' else os.environ.get('LOCAL_CONTROLLER','controller')
    if pathlib.Path(sys.argv[0]).name != 'ssh': state=os.environ.get('LOCAL_WORK_STATE',state)
    if os.environ.get('WORK_BUSY_HOST') == host: state='busy'
    if state=='error': sys.exit(255)
    print(state); sys.exit(0)
if '# nixfleet controller probe' in script:
    print(os.environ.get('CONTROLLER_STATE','gone')); sys.exit(0)
assert '.operations.lock' in sys.argv[-1]
base.mkdir(exist_ok=True)
with open(base/'.operations.lock','a') as lock:
    fcntl.flock(lock,fcntl.LOCK_EX)
    script=script.replace('/var/lib/nix-deploy/coordination',str(base))
    result=subprocess.run(['sh'],input=script,text=True)
    sys.exit(result.returncode)
`
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	stub = "#!" + python + "\n" + strings.TrimPrefix(stub, "#!/usr/bin/env python3\n")
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	bashJSON, _ := json.Marshal(bash)
	stub = strings.ReplaceAll(stub, "REAL_BASH", string(bashJSON))
	os.WriteFile(filepath.Join(bin, "ssh"), []byte(stub), 0700)
	os.WriteFile(filepath.Join(bin, "bash"), []byte(stub), 0700)
	os.WriteFile(filepath.Join(bin, "sudo"), []byte(stub), 0700)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("RESERVATION_FIXTURE", filepath.Join(dir, "shared"))
	fields := map[string]any{
		"schemaVersion": 1, "operatorHost": "controller", "homeManagerUser": "fixture", "homeOutputPrefix": "fixture-",
		"knownHosts": []string{"controller", "node-a", "node-b"}, "homeManagerHosts": []string{}, "remoteHosts": []string{"node-a", "node-b"}, "deployAllHosts": []string{"controller", "node-a", "node-b"},
		"aliases": map[string]string{}, "sshHosts": map[string]string{}, "systemSshUsers": map[string]string{}, "systemRemoteSudoHosts": []string{}, "systemActivationModes": map[string]string{},
	}
	data, _ := json.Marshal(fields)
	config := filepath.Join(dir, "inventory with spaces.json")
	if err := os.WriteFile(config, data, 0600); err != nil {
		t.Fatal(err)
	}
	inv, err := parseInventory(data)
	if err != nil {
		t.Fatal(err)
	}
	d := &deployer{inv: inv, self: "controller", configFile: config}
	return d, &serialGroup{Hosts: []string{"node-a", "node-b"}}, filepath.Join(dir, "shared")
}

func TestReservationVisibilityAndBlockedMessage(t *testing.T) {
	d, g, _ := reservationFixture(t)
	r, err := d.reserveGroup(g)
	if err != nil {
		t.Fatal(err)
	}
	r.persist("lost activation connection; outcome ambiguous")
	held, err := d.listReservations()
	if err != nil || len(held) != 2 {
		t.Fatalf("listing: %v %v", held, err)
	}
	for _, item := range held {
		if item.Info.Controller != "controller" || item.Info.PID != os.Getpid() || item.Info.Started == "" || item.Info.Created.IsZero() || !strings.Contains(item.Info.Reason, "ambiguous") {
			t.Fatalf("incomplete visibility: %+v", item)
		}
	}
	_, err = d.reserveGroup(g)
	if err == nil || !strings.Contains(err.Error(), "reservation "+r.keys[0]) || !strings.Contains(err.Error(), d.reservationCommand(r.keys[0])) {
		t.Fatalf("blocked message: %v", err)
	}
}

func TestReservationReleaseAdmissionAndAudit(t *testing.T) {
	d, g, base := reservationFixture(t)
	r, err := d.reserveGroup(g)
	if err != nil {
		t.Fatal(err)
	}
	held, err := d.listReservations()
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"busy", "unknown", "error"} {
		t.Setenv("WORK_STATE", state)
		if err := d.releaseHeld(held, r.keys[0], false, &bytes.Buffer{}); err == nil {
			t.Fatalf("release accepted work state %s", state)
		}
	}
	t.Setenv("WORK_STATE", "busy")
	if err := d.releaseHeld(held, r.keys[0], true, &bytes.Buffer{}); err == nil {
		t.Fatal("confirmation bypassed active work")
	}
	t.Setenv("WORK_STATE", "idle")
	t.Setenv("CONTROLLER_STATE", "busy")
	if err := d.releaseHeld(held, r.keys[0], true, &bytes.Buffer{}); err == nil {
		t.Fatal("confirmation bypassed a live controller")
	}
	t.Setenv("CONTROLLER_STATE", "gone")
	var output bytes.Buffer
	if err := d.releaseHeld(held, r.keys[0], false, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), r.keys[0]) {
		t.Fatal("release not reported")
	}
	remaining, err := d.listReservations()
	if err != nil || len(remaining) != 0 {
		t.Fatalf("release incomplete: %v %v", remaining, err)
	}
	audit, err := os.ReadFile(filepath.Join(base, "releases.jsonl"))
	if err != nil || !strings.Contains(string(audit), r.keys[0]) {
		t.Fatalf("audit missing: %s %v", audit, err)
	}
	// An old snapshot can never release a newer invocation of the same hosts.
	fresh, err := d.reserveGroup(g)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.releaseHeld(held, r.keys[0], false, &output); err == nil {
		t.Fatal("stale release accepted")
	}
	remaining, _ = d.listReservations()
	if len(remaining) != 2 || remaining[0].Owner != fresh.token {
		t.Fatal("stale release removed a new owner")
	}
}

func TestUnknownInspectionRequiresExplicitConfirmation(t *testing.T) {
	d, g, _ := reservationFixture(t)
	r, err := d.reserveGroup(g)
	if err != nil {
		t.Fatal(err)
	}
	held, err := d.listReservations()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("WORK_STATE", "error")
	if err := d.releaseHeld(held, r.keys[0], false, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "--confirm-stopped") {
		t.Fatalf("missing confirmation: %v", err)
	}
	if err := d.releaseHeld(held, r.keys[0], true, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
}

func TestDefiniteFailureReleasesButAmbiguityRetains(t *testing.T) {
	for _, status := range []int{1, 7, 124, 130, 143, 255} {
		t.Run(time.Duration(status).String(), func(t *testing.T) {
			d, g, _ := reservationFixture(t)
			r, err := d.reserveGroup(g)
			if err != nil {
				t.Fatal(err)
			}
			if !r.start("node-a") {
				t.Fatal("start refused")
			}
			r.ended(status)
			released := r.finish(true)
			held, err := d.listReservations()
			if err != nil {
				t.Fatal(err)
			}
			definite := status == 1 || status == 7
			if released != definite || (len(held) == 0) != definite {
				t.Fatalf("status %d: released=%t held=%d", status, released, len(held))
			}
		})
	}
}

func TestActiveWorkPreventsAutomaticFailureRelease(t *testing.T) {
	d, g, _ := reservationFixture(t)
	r, err := d.reserveGroup(g)
	if err != nil {
		t.Fatal(err)
	}
	if !r.start("node-a") {
		t.Fatal("start refused")
	}
	r.ended(1)
	t.Setenv("WORK_STATE", "busy")
	if r.finish(true) {
		t.Fatal("released while work may still run")
	}
	held, err := d.listReservations()
	if err != nil || len(held) != 2 || !strings.Contains(held[0].Info.Reason, "still be running") {
		t.Fatalf("retention not recorded: %v %v", held, err)
	}
}

func TestAutomaticReleaseInspectsControllerLocally(t *testing.T) {
	for _, workState := range []string{"idle", "busy", "unknown", "error"} {
		t.Run(workState, func(t *testing.T) {
			d, g, _ := reservationFixture(t)
			d.self = "outside-controller"
			t.Setenv("LOCAL_CONTROLLER", d.self)
			r, err := d.reserveGroup(g)
			if err != nil || !r.start("node-a") {
				t.Fatalf("admission: %v", err)
			}
			r.ended(7)
			// The controller has no inventory SSH endpoint. Only its local
			// inspection changes, while remote targets report idle.
			t.Setenv("LOCAL_WORK_STATE", workState)
			if released := r.finish(true); released != (workState == "idle") {
				t.Fatalf("local %s: released=%t", workState, released)
			}
		})
	}
}

func TestRealProbeErrorsFailClosed(t *testing.T) {
	// Probe scripts are executed only against stubs, never a host or cluster.
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "id"), []byte("#!/bin/sh\necho 0\n"), 0700)
	os.WriteFile(filepath.Join(bin, "ps"), []byte("#!/bin/sh\necho '42 shell harmless'\n"), 0700)
	os.WriteFile(filepath.Join(bin, "awk"), []byte("#!/bin/sh\nexit 2\n"), 0700)
	os.WriteFile(filepath.Join(bin, "uname"), []byte("#!/bin/sh\necho Linux\n"), 0700)
	os.WriteFile(filepath.Join(bin, "systemctl"), []byte("#!/bin/sh\nexit 0\n"), 0700)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	if status := (command{ctx: context.Background(), argv: []string{"sh", "-s"}, stdin: strings.NewReader(activationProbe), stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}).run(); status == 0 {
		t.Fatal("failed process scan reported idle")
	}
}

func TestControllerIdentityProbeChecksPIDReuseAndErrors(t *testing.T) {
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "ssh"), []byte("#!/bin/sh\nsh -s\n"), 0700)
	os.WriteFile(filepath.Join(bin, "id"), []byte("#!/bin/sh\necho 0\n"), 0700)
	os.WriteFile(filepath.Join(bin, "uname"), []byte("#!/bin/sh\necho Linux\n"), 0700)
	os.WriteFile(filepath.Join(bin, "readlink"), []byte("#!/bin/sh\necho synthetic-namespace\n"), 0700)
	os.WriteFile(filepath.Join(bin, "ps"), []byte(`#!/bin/sh
if [ "$PS_FAIL" = 1 ]; then exit 2; fi
case "$*" in
 '-A -o pid=') printf '%s\n' "$PS_PID" ;;
 '-p 42 -o lstart=') printf '%s\n' "$PS_START" ;;
 *) exit 99 ;;
esac
[ "$PS_FAIL" != partial ] || exit 2
`), 0700)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	d := &deployer{inv: &inventory{KnownHosts: []string{"controller"}}}
	info := reservationInfo{Controller: "controller", ControllerEndpoint: "root@controller", PID: 42, Started: "Thu Nov 5 01:00:00 2026", PIDNamespace: "synthetic-namespace"}
	t.Setenv("PS_PID", "42")
	t.Setenv("PS_START", "  Thu  Nov  5 01:00:00 2026  ")
	state, err := d.controllerState(info)
	if err != nil || state != "busy" {
		t.Fatalf("active identity: %s %v", state, err)
	}
	t.Setenv("PS_FAIL", "partial")
	state, err = d.controllerState(info)
	if err != nil || state != "busy" {
		t.Fatalf("positive identity suppressed by partial scan failure: %s %v", state, err)
	}
	t.Setenv("PS_PID", "7")
	if state, err = d.controllerState(info); err == nil || state != "unknown" {
		t.Fatal("partial scan established absence")
	}
	t.Setenv("PS_FAIL", "")
	t.Setenv("PS_PID", "42\nmalformed")
	state, err = d.controllerState(info)
	if err != nil || state != "busy" {
		t.Fatalf("positive identity suppressed by incomplete row: %s %v", state, err)
	}
	for _, incomplete := range []string{"", "malformed", "7\nmalformed"} {
		t.Setenv("PS_PID", incomplete)
		if state, err = d.controllerState(info); err == nil || state != "unknown" {
			t.Fatalf("incomplete PID list established absence: %q %s %v", incomplete, state, err)
		}
	}
	t.Setenv("PS_PID", "42")
	info.PIDNamespace = "another-namespace"
	state, err = d.controllerState(info)
	if err != nil || state != "unknown" {
		t.Fatalf("namespace mismatch established identity: %s %v", state, err)
	}
	info.PIDNamespace = "synthetic-namespace"
	t.Setenv("PS_START", "Fri Nov 6 01:00:00 2026")
	state, err = d.controllerState(info)
	if err != nil || state != "unknown" {
		t.Fatalf("PID reuse or clock step established absence: %s %v", state, err)
	}
	t.Setenv("PS_PID", "7")
	state, err = d.controllerState(info)
	if err != nil || state != "gone" {
		t.Fatalf("absent PID: %s %v", state, err)
	}
	t.Setenv("PS_FAIL", "1")
	if state, err = d.controllerState(info); err == nil || state != "unknown" {
		t.Fatal("failed process read established absence")
	}
}

func TestOrphanControllerFrontendPreventsRelease(t *testing.T) {
	d, g, _ := reservationFixture(t)
	r, err := d.reserveGroup(g)
	if err != nil {
		t.Fatal(err)
	}
	held, err := d.listReservations()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTROLLER_STATE", "gone")
	t.Setenv("WORK_BUSY_HOST", "controller")
	if err := d.releaseHeld(held, r.keys[0], false, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "controller") {
		t.Fatalf("orphan frontend was ignored: %v", err)
	}
}

func TestReservationsCLIListsAndReleasesStoppedWork(t *testing.T) {
	d, g, _ := reservationFixture(t)
	r, err := d.reserveGroup(g)
	if err != nil {
		t.Fatal(err)
	}
	if !r.persist("connection lost; outcome ambiguous") {
		t.Fatal("persist failed")
	}
	capture := func(args ...string) (string, int) {
		reader, writer, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		previous := os.Stdout
		os.Stdout = writer
		code := dispatch("nixfleet", args)
		os.Stdout = previous
		writer.Close()
		output, err := io.ReadAll(reader)
		reader.Close()
		if err != nil {
			t.Fatal(err)
		}
		return string(output), code
	}
	output, code := capture("reservations", "--config", d.configFile)
	if code != 0 || !strings.Contains(output, "controller=controller") || !strings.Contains(output, "age=") || !strings.Contains(output, "connection lost") || !strings.Contains(output, d.reservationCommand(r.keys[0])) {
		t.Fatalf("CLI list: %d %s", code, output)
	}
	output, code = capture("reservations", "release", r.keys[0], "--config", d.configFile)
	if code != 0 || !strings.Contains(output, "Released reservation "+r.keys[0]) {
		t.Fatalf("CLI release: %d %s", code, output)
	}
	output, code = capture("reservations", "--config", d.configFile)
	if code != 0 || !strings.Contains(output, "No held reservations") {
		t.Fatalf("CLI empty list: %d %s", code, output)
	}
}

func TestReleaseInspectsLockedAndConfiguredMembers(t *testing.T) {
	d, g, _ := reservationFixture(t)
	d.inv.SerialGroups = []*serialGroup{g}
	r, err := d.reserveGroup(g)
	if err != nil {
		t.Fatal(err)
	}
	held, err := d.listReservations()
	if err != nil {
		t.Fatal(err)
	}
	// A stale or incomplete metadata group cannot omit a locked member or
	// the inventory's configured constraint group from work inspection.
	for i := range held {
		held[i].Info.Group = []string{"node-a"}
	}
	t.Setenv("WORK_BUSY_HOST", "node-b")
	if err := d.releaseHeld(held, r.keys[0], true, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "node-b") {
		t.Fatalf("omitted member was ignored: %v", err)
	}
}

func TestConfirmationStillInspectsLegacyEndpointsForPositiveWork(t *testing.T) {
	d, g, _ := reservationFixture(t)
	r, err := d.reserveGroup(g)
	if err != nil {
		t.Fatal(err)
	}
	held, err := d.listReservations()
	if err != nil {
		t.Fatal(err)
	}
	for i := range held {
		held[i].Info.Endpoints = nil
	}
	t.Setenv("WORK_BUSY_HOST", "node-b")
	if err := d.releaseHeld(held, r.keys[0], true, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "node-b") {
		t.Fatalf("confirmation skipped available work inspection: %v", err)
	}
}

func TestAuthorityRequiresFreshOperationAcknowledgement(t *testing.T) {
	d, g, base := reservationFixture(t)
	r, err := d.reserveGroup(g)
	if err != nil {
		t.Fatal(err)
	}
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		t.Fatal(err)
	}
	// Model SSH's successful empty shell when StdinNull/-n discards input.
	// A stale acknowledgement is equally insufficient for every operation.
	if err := os.WriteFile(ssh, []byte("#!/bin/sh\ncat >/dev/null\nprintf '%s' \"$ACK_FIXTURE\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, response := range []string{"", "\nnixfleet-authority-ack:stale:replayed\n"} {
		t.Setenv("ACK_FIXTURE", response)
		if _, err := d.reserveGroup(g); err == nil {
			t.Fatal("unacknowledged acquisition admitted")
		}
		if r.persist("activation starting") {
			t.Fatal("unacknowledged metadata update admitted")
		}
		if _, err := d.listReservations(); err == nil {
			t.Fatal("unacknowledged listing appeared empty")
		}
		if r.release() {
			t.Fatal("unacknowledged release reported success")
		}
	}
	if entries, err := reservationEntries(base); err != nil || len(entries) != 2 {
		t.Fatalf("protection lost: %v %v", entries, err)
	}
}

func TestActivationProbePortableProcessColumns(t *testing.T) {
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "id"), []byte("#!/bin/sh\necho 0\n"), 0700)
	os.WriteFile(filepath.Join(bin, "uname"), []byte("#!/bin/sh\necho \"$PROBE_OS\"\n"), 0700)
	os.WriteFile(filepath.Join(bin, "systemctl"), []byte("#!/bin/sh\nexit 0\n"), 0700)
	os.WriteFile(filepath.Join(bin, "ps"), []byte(`#!/bin/sh
[ "$*" = '-A -ww -o pid= -o comm= -o args=' ] || exit 99
printf '%s\n' "$PROBE_PROCESSES"
exit "${PROBE_PS_STATUS:-0}"
`), 0700)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	for _, platform := range []string{"Linux", "Darwin"} {
		t.Setenv("PROBE_OS", platform)
		for _, psStatus := range []string{"0", "2"} {
			t.Setenv("PROBE_PS_STATUS", psStatus)
			for _, fixture := range []struct{ processes, want string }{
				{"42 sh sh -s", "idle"},
				{"42 nixos-rebuild nixos-rebuild switch", "busy"},
				{"42 darwin-rebuild darwin-rebuild switch", "busy"},
				{"42 switch-to-configuration /fixture/switch-to-configuration switch", "busy"},
				{"42 sh /fixture/activate", "busy"},
				{"42 darwin-rebuild darwin-rebuild switch\n73 unrelated", "busy"},
				{"73 unrelated\n42 nixos-rebuild nixos-rebuild switch", "busy"},
				{"", "error"},
				{"42", "error"},
			} {
				if psStatus != "0" && fixture.want == "idle" {
					fixture.want = "error"
				}
				t.Setenv("PROBE_PROCESSES", fixture.processes)
				var output bytes.Buffer
				status := (command{ctx: context.Background(), argv: []string{"sh", "-s"}, stdin: strings.NewReader(activationProbe), stdout: &output, stderr: io.Discard}).run()
				if fixture.want == "error" {
					if status == 0 {
						t.Fatal("incomplete process columns established idleness")
					}
				} else if status != 0 || strings.TrimSpace(output.String()) != fixture.want {
					t.Fatalf("%s %q: %d %q", platform, fixture.processes, status, output.String())
				}
			}
		}
	}
}

func TestPositiveWorkSurvivesInspectionDisconnect(t *testing.T) {
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "ssh"), []byte("#!/bin/sh\ncat >/dev/null\necho \"$PROBE_RESPONSE\"\nexit 255\n"), 0700)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	d := &deployer{inv: &inventory{KnownHosts: []string{"controller"}}}
	for _, response := range []string{"busy", "idle", "gone"} {
		t.Setenv("PROBE_RESPONSE", response)
		state, err := d.probeHost("controller", activationProbe)
		if response == "busy" {
			if state != "busy" {
				t.Fatalf("positive work suppressed by disconnect: %s %v", state, err)
			}
		} else if state != "unknown" || err == nil {
			t.Fatalf("disconnect established %s: %s %v", response, state, err)
		}
	}
}
