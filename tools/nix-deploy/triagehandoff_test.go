package main

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTriageHostConfig(t *testing.T) {
	cfg, _, err := loadTriageConfig(writeInventory(t, map[string]any{"triageHost": "xyz", "sshHint": "run ssh-on on this host"}))
	if err != nil || cfg.TriageHost != "xyz" || cfg.SSHHint != "run ssh-on on this host" {
		t.Fatalf("config = %+v, %v", cfg, err)
	}
	for name, triage := range map[string]any{
		"host number":    map[string]any{"triageHost": 1},
		"host empty":     map[string]any{"triageHost": ""},
		"host unsafe":    map[string]any{"triageHost": "-oProxyCommand=x"},
		"hint empty":     map[string]any{"sshHint": " "},
		"hint two lines": map[string]any{"sshHint": "a\nb"},
		"hint array":     map[string]any{"sshHint": []any{"a"}},
	} {
		if _, _, err := loadTriageConfig(writeInventory(t, triage)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestNewTriageHandoff(t *testing.T) {
	previous := triageHostname
	defer func() { triageHostname = previous }()
	name := "xyz"
	triageHostname = func() (string, bool) { return name, true }
	t.Setenv("NIX_SSHOPTS", "-o ServerAliveInterval=5")
	t.Setenv("DEPLOY_SSH_CHECK_TIMEOUT", "7")
	inventory := writeInventory(t, nil)

	if h, err := newTriageHandoff(nil, inventory); h != nil || err != nil {
		t.Errorf("no config gave %v, %v", h, err)
	}
	if h, err := newTriageHandoff(&triageConfig{}, inventory); h != nil || err != nil {
		t.Errorf("no triage host gave %v, %v", h, err)
	}
	if h, err := newTriageHandoff(&triageConfig{TriageHost: "xyz"}, inventory); h != nil || err != nil {
		t.Errorf("the triage host itself gave %v, %v", h, err)
	}
	// The triage host is named by an alias and reached by its sshHosts
	// destination.
	h, err := newTriageHandoff(&triageConfig{TriageHost: "edge", SSHHint: "hint"}, inventory)
	if err != nil || h == nil || h.self != "xyz" || h.host != "node" || h.hostSSH != "node.invalid" || h.selfSSH != "xyz" || h.hint != "hint" {
		t.Fatalf("handoff = %+v, %v", h, err)
	}
	argv := strings.Join(h.sshArgv(h.hostSSH, "true", true), " ")
	if argv != "ssh -t -o BatchMode=yes -o ConnectTimeout=7 -o ConnectionAttempts=1 -o ServerAliveInterval=5 node.invalid true" {
		t.Errorf("ssh argv = %q", argv)
	}
	if _, err := newTriageHandoff(&triageConfig{TriageHost: "elsewhere"}, inventory); err == nil || !strings.Contains(err.Error(), "not a known host") {
		t.Errorf("an unknown triage host gave %v", err)
	}
	name = "node"
	if h, err := newTriageHandoff(&triageConfig{TriageHost: "edge"}, inventory); h != nil || err != nil {
		t.Errorf("the triage host by its alias gave %v, %v", h, err)
	}
}

// handoffFixture is a failed run on a source host whose flake directory is
// ~/src/flake, with one home directory and state directory shared by both
// sides of the hand-off.
type handoffFixture struct {
	home, dir string
	info      runInfo
}

func newHandoffFixture(t *testing.T) handoffFixture {
	t.Helper()
	runs := stateRuns(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	flake := filepath.Join(home, "src", "flake")
	if err := os.MkdirAll(flake, 0o700); err != nil {
		t.Fatal(err)
	}
	id := "20260102T030405.000000Z-42"
	dir := makeRun(t, runs, id, true, &runReport{}, map[string]string{"system-node-evaluate.log": "error: boom\n"})
	info, err := readRunInfo(dir)
	if err != nil {
		t.Fatal(err)
	}
	info.FlakeDir = flake
	data, _ := json.Marshal(info)
	files := map[string]string{
		"run.json":   string(data),
		runLockName:  "",
		"context.md": "# nixfleet triage context\n\nRun record: " + dir + "\n\n## Run\n\n- Log: " + dir + "/logs/system-node-evaluate.log\n",
		"inputs.md":  "# Locked inputs\n\nSee " + dir + "/flake.lock\n",
	}
	for name, text := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return handoffFixture{home: home, dir: dir, info: info}
}

// fakeHandoff answers the SSH checks with the statuses given and runs the
// receive in this process, as the triage host would.
func fakeHandoff(cfg *triageConfig, hostStatus, backStatus, receiveStatus int) (*triageHandoff, *bytes.Buffer, *[]string) {
	var stderr bytes.Buffer
	var terminal []string
	h := &triageHandoff{
		self: "src", host: "hub", selfSSH: "src.invalid", hostSSH: "hub.invalid",
		connectTimeout: "5", hint: "run ssh-on on this host", stderr: &stderr,
	}
	h.run = func(timeout time.Duration, stdin io.Reader, argv ...string) (string, string, int) {
		remote := argv[len(argv)-1]
		switch {
		case remote == "true":
			return "", "ssh: connect to host hub.invalid: Connection refused\n", hostStatus
		case strings.HasPrefix(remote, "ssh "):
			if !strings.HasSuffix(remote, " src.invalid true") || !strings.Contains(remote, "BatchMode=yes") {
				return "", "unexpected reach-back " + remote, 1
			}
			return "", "ssh: connect to host src.invalid port 22: Connection refused\n", backStatus
		case strings.HasPrefix(remote, "nixfleet triage --receive src "):
			if receiveStatus != 0 {
				io.Copy(io.Discard, stdin)
				return "", "nixfleet triage: unknown option '--receive'\nUsage: ...\n", receiveStatus
			}
			fields := strings.Fields(remote)
			// A nil config stands for a host given no inventory.
			configFile := "inventory.json"
			if cfg == nil {
				configFile = ""
			}
			dest, err := receiveHandoff("src", fields[len(fields)-1], stdin, configFile, cfg)
			if err != nil {
				return "", err.Error(), 1
			}
			return dest + "\n", "", 0
		}
		return "", "unexpected command " + remote, 1
	}
	h.terminal = func(argv []string) int {
		terminal = argv
		return 7
	}
	return h, &stderr, &terminal
}

func TestTriageHandOver(t *testing.T) {
	f := newHandoffFixture(t)
	cfg := &triageConfig{AgentCommand: []string{"agent"}}
	h, stderr, terminal := fakeHandoff(cfg, 0, 0, 0)
	reachable, why, ok := h.check()
	if !reachable || why != "" || !ok {
		t.Fatalf("check = %v, %q, %v", reachable, why, ok)
	}
	status, handed := h.handOver(f.dir, f.info, reachable, why, true)
	if status != 7 || !handed {
		t.Fatalf("handOver = %d, %v; stderr %s", status, handed, stderr)
	}
	dest := filepath.Join(stateHome(), "nix-deploy", "handoffs", "src", f.info.ID)
	got := strings.Join(*terminal, " ")
	if !strings.HasPrefix(got, "ssh -t -o BatchMode=yes") || !strings.HasSuffix(got, "hub.invalid nixfleet triage --from-host src --warnings '"+dest+"'") {
		t.Errorf("terminal argv = %q", got)
	}
	for _, want := range []string{"the triage host hub is reachable over SSH as hub.invalid (", "hub reaches this host over SSH as src.invalid (", "copied the run record to hub:" + dest} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
	if fileExists(filepath.Join(dest, runLockName)) || !fileExists(filepath.Join(dest, "logs", "system-node-evaluate.log")) {
		t.Error("the copy has run.lock or lacks the logs")
	}
	context, _ := os.ReadFile(filepath.Join(dest, "context.md"))
	inputs, _ := os.ReadFile(filepath.Join(dest, "inputs.md"))
	for _, want := range []string{"# nixfleet triage context\n\n## Hand-off from src\n", "reachable from here with `ssh src.invalid`", "copied from src:" + f.dir, "Run record: " + dest, "- Log: " + dest + "/logs/", filepath.Join(f.home, "src", "flake"), "## Own inputs on this host"} {
		if !strings.Contains(string(context), want) {
			t.Errorf("context lacks %q:\n%s", want, context)
		}
	}
	if !strings.Contains(string(inputs), dest+"/flake.lock") {
		t.Errorf("inputs.md not rewritten: %s", inputs)
	}
	handoff, err := readHandoffInfo(dest)
	if err != nil || handoff.SourceHost != "src" || handoff.FlakeDirInHome != "src/flake" || !handoff.Reachable || handoff.SourceRecord != f.dir {
		t.Errorf("handoff.json = %+v, %v", handoff, err)
	}
	prompt := triagePrompt(filepath.Join(dest, "context.md"), "/p", "", false, handoffPrompt(handoff))
	if !strings.Contains(prompt, "recorded on src") || !strings.Contains(prompt, "`ssh src.invalid`") || strings.Contains(prompt, "could not be reached") {
		t.Errorf("prompt = %q", prompt)
	}

	// Handing the same run over again replaces the copy.
	if status, handed := h.handOver(f.dir, f.info, true, "", false); status != 7 || !handed {
		t.Errorf("a second hand-off gave %d, %v", status, handed)
	}
}

func TestTriageHandOverReachBackFails(t *testing.T) {
	f := newHandoffFixture(t)
	h, stderr, _ := fakeHandoff(&triageConfig{AgentCommand: []string{"agent"}}, 0, 255, 0)
	reachable, why, ok := h.check()
	if reachable || !ok || !strings.Contains(why, "Connection refused") {
		t.Fatalf("check = %v, %q, %v", reachable, why, ok)
	}
	if !strings.Contains(stderr.String(), "hub cannot reach this host over SSH as src.invalid (") ||
		!strings.Contains(stderr.String(), "to let hub reach this host: run ssh-on on this host") {
		t.Errorf("stderr = %s", stderr)
	}
	if status, handed := h.handOver(f.dir, f.info, reachable, why, false); status != 7 || !handed {
		t.Fatalf("handOver = %d, %v", status, handed)
	}
	if !strings.Contains(stderr.String(), "handing the run over anyway") {
		t.Errorf("stderr = %s", stderr)
	}
	dest := filepath.Join(stateHome(), "nix-deploy", "handoffs", "src", f.info.ID)
	handoff, _ := readHandoffInfo(dest)
	context, _ := os.ReadFile(filepath.Join(dest, "context.md"))
	if handoff.Reachable || !strings.Contains(string(context), "could not be reached from here") {
		t.Errorf("handoff %+v, context:\n%s", handoff, context)
	}
	if prompt := handoffPrompt(handoff); !strings.Contains(prompt, "could not be reached over SSH from here (ssh: connect to host src.invalid") {
		t.Errorf("prompt = %q", prompt)
	}
}

// A record directory given as a symbolic link is copied whole.
func TestTriageHandOverSymlinkedRecord(t *testing.T) {
	f := newHandoffFixture(t)
	link := filepath.Join(t.TempDir(), "latest")
	if err := os.Symlink(f.dir, link); err != nil {
		t.Fatal(err)
	}
	h, stderr, _ := fakeHandoff(&triageConfig{AgentCommand: []string{"agent"}}, 0, 0, 0)
	if _, handed := h.handOver(link, f.info, true, "", false); !handed {
		t.Fatalf("a symlinked record was not handed over; stderr %s", stderr)
	}
	dest := filepath.Join(stateHome(), "nix-deploy", "handoffs", "src", f.info.ID)
	if !fileExists(filepath.Join(dest, "context.md")) || !fileExists(filepath.Join(dest, "logs", "system-node-evaluate.log")) {
		t.Error("the copy of a symlinked record lacks its files")
	}
}

// A record without context.md is refused.
func TestTriageReceiveRefusesIncompleteRecord(t *testing.T) {
	f := newHandoffFixture(t)
	if err := os.Remove(filepath.Join(f.dir, "context.md")); err != nil {
		t.Fatal(err)
	}
	h, stderr, _ := fakeHandoff(&triageConfig{AgentCommand: []string{"agent"}}, 0, 0, 0)
	if _, handed := h.handOver(f.dir, f.info, true, "", false); handed || !strings.Contains(stderr.String(), "has no context.md") {
		t.Errorf("an incomplete record: handed %v, stderr %s", handed, stderr)
	}
}

func TestTriageHandOverFallsBack(t *testing.T) {
	f := newHandoffFixture(t)
	cfg := &triageConfig{AgentCommand: []string{"agent"}}

	// An unreachable triage host stops before the reach-back.
	h, stderr, terminal := fakeHandoff(cfg, 255, 0, 0)
	if _, _, ok := h.check(); ok {
		t.Error("an unreachable triage host passed")
	}
	if !strings.Contains(stderr.String(), "the triage host hub is not reachable over SSH as hub.invalid (") ||
		!strings.Contains(stderr.String(), "Connection refused; running triage here") || strings.Contains(stderr.String(), "reach this host") {
		t.Errorf("stderr = %s", stderr)
	}

	// A triage host whose nixfleet cannot receive.
	h, stderr, terminal = fakeHandoff(cfg, 0, 0, 2)
	if _, handed := h.handOver(f.dir, f.info, true, "", false); handed || *terminal != nil {
		t.Error("a failed copy handed over")
	}
	if !strings.Contains(stderr.String(), "cannot copy the run record to hub (") || !strings.Contains(stderr.String(), "unknown option '--receive'; running triage here") {
		t.Errorf("stderr = %s", stderr)
	}

	// The triage host lacks the checkout or an agent command.
	if err := os.RemoveAll(filepath.Join(f.home, "src")); err != nil {
		t.Fatal(err)
	}
	h, stderr, _ = fakeHandoff(cfg, 0, 0, 0)
	if _, handed := h.handOver(f.dir, f.info, true, "", false); handed || !strings.Contains(stderr.String(), "no checkout at") {
		t.Errorf("a missing checkout: handed %v, stderr %s", handed, stderr)
	}
	if err := os.MkdirAll(f.info.FlakeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	h, stderr, _ = fakeHandoff(&triageConfig{}, 0, 0, 0)
	if _, handed := h.handOver(f.dir, f.info, true, "", false); handed || !strings.Contains(stderr.String(), "no agentCommand") {
		t.Errorf("no agent command: handed %v, stderr %s", handed, stderr)
	}
	h, stderr, _ = fakeHandoff(nil, 0, 0, 0)
	if _, handed := h.handOver(f.dir, f.info, true, "", false); handed || !strings.Contains(stderr.String(), "no inventory is configured here") {
		t.Errorf("no inventory: handed %v, stderr %s", handed, stderr)
	}
	if entries, _ := os.ReadDir(filepath.Join(stateHome(), "nix-deploy", "handoffs", "src")); len(entries) != 0 {
		t.Errorf("failed receives left %d entries", len(entries))
	}

	// A flake directory outside the home directory has no counterpart.
	outside := f.info
	outside.FlakeDir = "/flake"
	h, stderr, _ = fakeHandoff(cfg, 0, 0, 0)
	if _, handed := h.handOver(f.dir, outside, true, "", false); handed || !strings.Contains(stderr.String(), "not inside the home directory") {
		t.Errorf("an outside flake: handed %v, stderr %s", handed, stderr)
	}
}

func TestTriageHandoffArchiveRefusesEscapes(t *testing.T) {
	for _, header := range []*tar.Header{
		{Name: "../outside", Typeflag: tar.TypeReg, Mode: 0o600},
		{Name: "/abs", Typeflag: tar.TypeReg, Mode: 0o600},
		{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc"},
	} {
		var buffer bytes.Buffer
		archive := tar.NewWriter(&buffer)
		archive.WriteHeader(header)
		archive.Close()
		if err := readHandoffArchive(&buffer, t.TempDir()); err == nil {
			t.Errorf("%s was accepted", header.Name)
		}
	}
}

func TestTriageHandoffPrune(t *testing.T) {
	base := t.TempDir()
	ids := []string{"20260101T000000.000000Z-1", "20260102T000000.000000Z-1", "20260103T000000.000000Z-1", "20260104T000000.000000Z-1"}
	for _, id := range append(ids, ".receive-x") {
		if err := os.Mkdir(filepath.Join(base, id), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	pruneHandoffs(base, ids[0], 2)
	for i, id := range ids {
		if want := i != 1; fileExists(filepath.Join(base, id)) != want {
			t.Errorf("%s kept = %v, want %v", id, !want, want)
		}
	}
	if !fileExists(filepath.Join(base, ".receive-x")) {
		t.Error("a receive in progress was removed")
	}
}

func TestTriageFromHost(t *testing.T) {
	f := newHandoffFixture(t)
	cfg := &triageConfig{AgentCommand: []string{"agent"}}
	h, _, _ := fakeHandoff(cfg, 0, 0, 0)
	if _, handed := h.handOver(f.dir, f.info, true, "", false); !handed {
		t.Fatal("not handed over")
	}
	dest := filepath.Join(stateHome(), "nix-deploy", "handoffs", "src", f.info.ID)
	if status := triageFromHost(triageOptions{fromHost: "src", run: dest, noLaunch: true}, cfg, false); status != 0 {
		t.Errorf("--from-host --no-launch gave %d", status)
	}
	if status := triageFromHost(triageOptions{fromHost: "other", run: dest, noLaunch: true}, cfg, false); status != 1 {
		t.Errorf("the wrong source host gave %d", status)
	}
	if status := triageFromHost(triageOptions{fromHost: "src", run: f.dir, noLaunch: true}, cfg, false); status != 1 {
		t.Errorf("a record that was not handed over gave %d", status)
	}
}

func TestTriageHandoffArguments(t *testing.T) {
	opts, err := parseTriageArgs([]string{"--from-host", "mac", "--warnings", "/state/copy"})
	if err != nil || opts.fromHost != "mac" || !opts.warnings || opts.run != "/state/copy" {
		t.Errorf("parseTriageArgs = %+v, %v", opts, err)
	}
	opts, err = parseTriageArgs([]string{"--receive", "mac", "20260102T030405.000000Z-42"})
	if err != nil || opts.receive != "mac" || opts.run != "20260102T030405.000000Z-42" {
		t.Errorf("parseTriageArgs = %+v, %v", opts, err)
	}
	for _, args := range [][]string{
		{"--from-host"}, {"--from-host", "-x", "/c"}, {"--from-host", "mac"},
		{"--receive", "mac"}, {"--receive", "mac", "--warnings", "id"},
		{"--receive", "mac", "--from-host", "mac", "id"},
	} {
		if _, err := parseTriageArgs(args); err == nil {
			t.Errorf("parseTriageArgs(%q) accepted", args)
		}
	}
}
