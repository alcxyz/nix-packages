package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeInventory(t *testing.T, triage any) string {
	t.Helper()
	fields := validFields()
	if triage != nil {
		fields["triage"] = triage
	}
	path := filepath.Join(t.TempDir(), "inventory.json")
	if err := os.WriteFile(path, encode(t, fields), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTriageConfig(t *testing.T) {
	cfg, warnings, err := loadTriageConfig(writeInventory(t, nil))
	if cfg != nil || warnings != nil || err != nil {
		t.Fatalf("no triage object gave %v, %v, %v", cfg, warnings, err)
	}
	if cfg, _, err := loadTriageConfig(""); cfg != nil || err != nil {
		t.Fatalf("no inventory gave %v, %v", cfg, err)
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	checkout := filepath.Join(home, "src", "tool")
	if err := os.MkdirAll(checkout, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg, warnings, err = loadTriageConfig(writeInventory(t, map[string]any{
		"agentCommand":     []any{"agent", "--flag"},
		"inputCheckouts":   map[string]any{"tool": "~/src/tool", "gone": "/nonexistent/gone"},
		"playbookAddendum": "~/addendum.md",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(cfg.AgentCommand, " ") != "agent --flag" || cfg.InputCheckouts["tool"] != checkout ||
		cfg.PlaybookAddendum != filepath.Join(home, "addendum.md") {
		t.Errorf("config = %+v", cfg)
	}
	if len(warnings) != 2 || !strings.Contains(warnings[0], "input gone") || !strings.Contains(warnings[1], "addendum") {
		t.Errorf("warnings = %q", warnings)
	}

	for name, triage := range map[string]any{
		"not an object":    []any{"agent"},
		"empty command":    map[string]any{"agentCommand": []any{}},
		"command string":   map[string]any{"agentCommand": "agent"},
		"empty argument":   map[string]any{"agentCommand": []any{"agent", ""}},
		"number argument":  map[string]any{"agentCommand": []any{"agent", 1}},
		"relative path":    map[string]any{"inputCheckouts": map[string]any{"tool": "src/tool"}},
		"relative addenda": map[string]any{"playbookAddendum": "addendum.md"},
		"unknown field":    map[string]any{"agent": []any{"agent"}},
		"empty name":       map[string]any{"inputCheckouts": map[string]any{"": "/src/tool"}},
	} {
		_, _, err := loadTriageConfig(writeInventory(t, triage))
		if err == nil || !strings.Contains(err.Error(), "triage") || errors.Is(err, errInventoryUnreadable) {
			t.Errorf("%s: err = %v", name, err)
		}
		if name == "empty name" && (err == nil || !strings.Contains(err.Error(), "inputCheckouts has an empty input name")) {
			t.Errorf("empty name: err = %v", err)
		}
	}

	// Only the triage object is validated: an error elsewhere in the
	// inventory does not stop triage.
	fields := validFields()
	fields["knownHosts"] = "not a list"
	fields["triage"] = map[string]any{"agentCommand": []any{"agent"}}
	broken := filepath.Join(t.TempDir(), "broken.json")
	if err := os.WriteFile(broken, encode(t, fields), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg, _, err := loadTriageConfig(broken); err != nil || cfg == nil || cfg.AgentCommand[0] != "agent" {
		t.Errorf("an otherwise invalid inventory gave %v, %v", cfg, err)
	}
	// A file that cannot be read or parsed is reported apart, so triage
	// can still write the context.
	notJSON := filepath.Join(t.TempDir(), "inventory.json")
	if err := os.WriteFile(notJSON, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{notJSON, filepath.Join(t.TempDir(), "missing.json")} {
		if cfg, _, err := loadTriageConfig(path); cfg != nil || !errors.Is(err, errInventoryUnreadable) {
			t.Errorf("%s gave %v, %v", path, cfg, err)
		}
	}
}

func TestTriageUnreadableInventoryWritesContext(t *testing.T) {
	runs := stateRuns(t)
	makeRun(t, runs, "20260102T030500.000000Z-4", true,
		&runReport{Hosts: []hostReport{{Host: "alpha", System: phaseReport{Status: "failed"}}}}, nil)
	notJSON := filepath.Join(t.TempDir(), "inventory.json")
	if err := os.WriteFile(notJSON, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if status := runTriage([]string{"--config", notJSON}); status != 0 {
		t.Errorf("runTriage exit status %d", status)
	}
	if !fileExists(filepath.Join(runs, "20260102T030500.000000Z-4", "context.md")) {
		t.Error("no context.md")
	}
}

func TestIsTerminal(t *testing.T) {
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	regular, err := os.Create(filepath.Join(t.TempDir(), "file"))
	if err != nil {
		t.Fatal(err)
	}
	defer regular.Close()
	if isTerminal(null) || isTerminal(regular) {
		t.Error("the null device or a regular file counts as a terminal")
	}
	// The null device as standard input or output never launches.
	cfg := &triageConfig{AgentCommand: []string{"agent"}}
	if ok, _ := launchDecision(cfg, false, isTerminal(null), true); ok {
		t.Error("launched with the null device as standard input")
	}
	if ok, _ := launchDecision(cfg, false, true, isTerminal(null)); ok {
		t.Error("launched with the null device as standard output")
	}
}

func TestTriageOwnInputs(t *testing.T) {
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
	git("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "a")
	head := git("rev-parse", "HEAD")

	lock := map[string]lockedInput{
		"tool":    {Type: "git", URL: "https://example.invalid/tool", Rev: head, NarHash: hashA},
		"other":   {Type: "git", URL: "https://example.invalid/other", Rev: "0123", NarHash: hashB},
		"nixpkgs": {Type: "github", Owner: "example", Repo: "pkgs", Rev: "bbbb", NarHash: hashB},
	}
	checkouts := map[string]string{"tool": repo, "other": repo}
	runs := stateRuns(t)
	dir := makeRun(t, runs, "20260102T030500.000000Z-4", true,
		&runReport{Hosts: []hostReport{{Host: "alpha", System: phaseReport{Status: "failed"}}}}, nil)
	setRun(t, dir, time.Date(2026, 1, 2, 3, 5, 0, 0, time.UTC), "/flake", "", lockData(t, lock))

	read := func() string {
		path, err := writeTriageContext(dir, checkouts)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(path)
		return string(data)
	}
	text := read()
	for _, want := range []string{
		"| tool | yes | git+https://example.invalid/tool |",
		"| other | yes |",
		"| nixpkgs | no |",
		"### tool\n\n- Checkout: " + repo + "\n- Locked by the run: " + head + "\n- Branch: main\n- HEAD: " + head + "\n- Tracked files: clean\n- HEAD is the locked revision: yes\n",
		"### other\n\n- Checkout: " + repo + "\n- Locked by the run: 0123\n",
		"- HEAD is the locked revision: no\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("context.md lacks %q:\n%s", want, text)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("b"), 0o600); err != nil {
		t.Fatal(err)
	}
	if text := read(); !strings.Contains(text, "- Tracked files: differ from HEAD") {
		t.Errorf("a dirty tree is not reported:\n%s", text)
	}

	// A configured input missing from the lock is named in both sections.
	checkouts = map[string]string{"typo": repo}
	text = read()
	for _, want := range []string{
		"Configured own inputs that are not in this lock (a misspelt name, or an input that follows another): typo; see \"Own inputs\".",
		"### typo\n\n- Checkout: " + repo + "\n- Locked by the run: not in the run's lock, or it follows another input\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("context.md lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "No own input is configured") {
		t.Errorf("context.md says no own input is configured:\n%s", text)
	}

	// The root's input name can point to a node of another name.
	data := lockData(t, lock)
	var parsed map[string]any
	json.Unmarshal(data, &parsed)
	parsed["nodes"].(map[string]any)["root"] = map[string]any{"inputs": map[string]any{"mine": "tool", "follows": []any{"nixpkgs"}}}
	data, _ = json.Marshal(parsed)
	nodes := ownInputNodes(data, map[string]string{"mine": repo, "follows": repo, "absent": repo})
	if len(nodes) != 1 || nodes["mine"] != "tool" {
		t.Errorf("ownInputNodes = %v", nodes)
	}
}

func TestTriageLaunchDecision(t *testing.T) {
	cfg := &triageConfig{AgentCommand: []string{"agent"}}
	for _, c := range []struct {
		cfg                   *triageConfig
		noLaunch, in, out, ok bool
	}{
		{cfg, false, true, true, true},
		{cfg, true, true, true, false},
		{nil, false, true, true, false},
		{&triageConfig{}, false, true, true, false},
		{cfg, false, false, true, false},
		{cfg, false, true, false, false},
	} {
		ok, reason := launchDecision(c.cfg, c.noLaunch, c.in, c.out)
		if ok != c.ok || ok == (reason != "") {
			t.Errorf("launchDecision(%+v) = %v, %q", c, ok, reason)
		}
	}
}

func TestTriageLaunch(t *testing.T) {
	scripts := t.TempDir()
	record := filepath.Join(scripts, "record")
	agent := filepath.Join(scripts, "agent")
	script := "#!/bin/sh\npwd > " + record + "\nfor arg in \"$@\"; do printf '%s\\n' \"$arg\" >> " + record + "; done\nexit 7\n"
	if err := os.WriteFile(agent, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	flake := t.TempDir()
	cfg := &triageConfig{AgentCommand: []string{agent, "--mode", "x y"}, PlaybookAddendum: "/private/addendum.md"}
	if status := launchTriage(cfg, "/runs/r/context.md", flake, "/share/playbook.md"); status != 7 {
		t.Errorf("exit status %d, want 7", status)
	}
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	resolved, _ := filepath.EvalSymlinks(flake)
	if len(lines) != 4 || (lines[0] != flake && lines[0] != resolved) || lines[1] != "--mode" || lines[2] != "x y" {
		t.Fatalf("recorded %q", lines)
	}
	for _, want := range []string{"/runs/r/context.md", "/share/playbook.md", "/private/addendum.md", "activation, push, merge or reboot"} {
		if !strings.Contains(lines[3], want) {
			t.Errorf("prompt %q lacks %q", lines[3], want)
		}
	}
	if status := launchTriage(cfg, "/c", filepath.Join(flake, "gone"), "/p"); status != 1 {
		t.Errorf("a missing flake directory gave %d", status)
	}
}

func TestTriagePlaybookPath(t *testing.T) {
	out := t.TempDir()
	exe := filepath.Join(out, "libexec", "nixfleet")
	if _, err := playbookPath(exe); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("a missing playbook gave %v", err)
	}
	want := filepath.Join(out, "share", "nixfleet", "triage-playbook.md")
	if err := os.MkdirAll(filepath.Dir(want), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(want, []byte("# playbook\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := playbookPath(exe); err != nil || got != want {
		t.Fatalf("playbookPath = %q, %v", got, err)
	}
}

func TestTriageArguments(t *testing.T) {
	t.Setenv("NIX_DEPLOY_CONFIG", "/env.json")
	opts, err := parseTriageArgs([]string{"--no-launch", "run1"})
	if err != nil || opts.configFile != "/env.json" || !opts.noLaunch || opts.run != "run1" {
		t.Errorf("parseTriageArgs = %+v, %v", opts, err)
	}
	opts, err = parseTriageArgs([]string{"--config", "/a.json"})
	if err != nil || opts.configFile != "/a.json" || opts.noLaunch {
		t.Errorf("parseTriageArgs = %+v, %v", opts, err)
	}
	for _, args := range [][]string{{"--config"}, {"--config="}, {"--bogus"}, {"a", "b"}} {
		if _, err := parseTriageArgs(args); err == nil {
			t.Errorf("parseTriageArgs(%q) accepted", args)
		}
	}
}
