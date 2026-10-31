package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// triageConfig is the optional triage object of the inventory (ADR-0016):
// the agent command triage launches, the local checkouts of the operator's
// own flake inputs by input name, and a private playbook addendum.
type triageConfig struct {
	AgentCommand     []string
	InputCheckouts   map[string]string
	PlaybookAddendum string
}

// errInventoryUnreadable is an inventory that cannot be read or is not a
// JSON object: triage still writes the context, but launches nothing.
var errInventoryUnreadable = errors.New("inventory unreadable")

// loadTriageConfig reads the triage object of the inventory at path. Only
// that object is validated, so an error elsewhere in the inventory does not
// stop triage. No path or no triage object gives nil; a file that cannot be
// read or parsed gives an error wrapping errInventoryUnreadable. The
// warnings name configured paths that do not exist.
func loadTriageConfig(path string) (*triageConfig, []string, error) {
	if path == "" {
		return nil, nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: cannot read inventory '%s'", errInventoryUnreadable, path)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return nil, nil, fmt.Errorf("%w: inventory '%s' is not a JSON object", errInventoryUnreadable, path)
	}
	raw, present := fields["triage"]
	if !present {
		return nil, nil, nil
	}
	home, _ := os.UserHomeDir()
	cfg, err := parseTriageConfig(raw, home)
	if err != nil {
		return nil, nil, fmt.Errorf("inventory '%s': triage: %v", path, err)
	}
	return cfg, cfg.warnings(), nil
}

// parseTriageConfig validates a triage object; home expands a leading ~/.
func parseTriageConfig(raw json.RawMessage, home string) (*triageConfig, error) {
	if !strings.HasPrefix(strings.TrimSpace(string(raw)), "{") {
		return nil, errors.New("must be an object")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	cfg := &triageConfig{InputCheckouts: map[string]string{}}
	for key, value := range fields {
		switch key {
		case "agentCommand":
			if !strings.HasPrefix(strings.TrimSpace(string(value)), "[") || json.Unmarshal(value, &cfg.AgentCommand) != nil || len(cfg.AgentCommand) == 0 {
				return nil, errors.New("agentCommand must be a non-empty array of strings")
			}
			for _, arg := range cfg.AgentCommand {
				if arg == "" {
					return nil, errors.New("agentCommand must not contain an empty string")
				}
			}
		case "inputCheckouts":
			var checkouts map[string]string
			if !strings.HasPrefix(strings.TrimSpace(string(value)), "{") || json.Unmarshal(value, &checkouts) != nil {
				return nil, errors.New("inputCheckouts must be an object of input names to paths")
			}
			for name, path := range checkouts {
				if name == "" {
					return nil, errors.New("inputCheckouts has an empty input name")
				}
				expanded, err := expandPath(path, home)
				if err != nil {
					return nil, fmt.Errorf("inputCheckouts '%s': %v", name, err)
				}
				cfg.InputCheckouts[name] = expanded
			}
		case "playbookAddendum":
			var path string
			if !isString(value) || json.Unmarshal(value, &path) != nil {
				return nil, errors.New("playbookAddendum must be a path")
			}
			expanded, err := expandPath(path, home)
			if err != nil {
				return nil, fmt.Errorf("playbookAddendum: %v", err)
			}
			cfg.PlaybookAddendum = expanded
		default:
			return nil, fmt.Errorf("unknown field '%s'", key)
		}
	}
	return cfg, nil
}

// expandPath expands a leading ~/ to home and requires an absolute result.
func expandPath(path, home string) (string, error) {
	if rest, ok := strings.CutPrefix(path, "~/"); ok && home != "" {
		path = filepath.Join(home, rest)
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("'%s' is not an absolute path", path)
	}
	return filepath.Clean(path), nil
}

func (cfg *triageConfig) warnings() []string {
	var warnings []string
	for _, name := range sortedPaths(cfg.InputCheckouts) {
		path := cfg.InputCheckouts[name]
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			warnings = append(warnings, fmt.Sprintf("the checkout of input %s, %s, is not a directory", name, path))
		}
	}
	if cfg.PlaybookAddendum != "" && !fileExists(cfg.PlaybookAddendum) {
		warnings = append(warnings, fmt.Sprintf("the playbook addendum %s does not exist", cfg.PlaybookAddendum))
	}
	return warnings
}

func sortedPaths(values map[string]string) []string {
	var keys []string
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// ownInputNodes maps each own input to its node in the lock: the node the
// root's input of that name points to, or the node of that name. An input
// that follows another or is not in the lock is left out.
func ownInputNodes(lock []byte, checkouts map[string]string) map[string]string {
	nodes := map[string]string{}
	var parsed flakeLock
	if len(checkouts) == 0 || json.Unmarshal(lock, &parsed) != nil {
		return nodes
	}
	root := parsed.Nodes[parsed.Root]
	for name := range checkouts {
		node := name
		if raw, ok := root.Inputs[name]; ok && json.Unmarshal(raw, &node) != nil {
			continue
		}
		if _, ok := parsed.Nodes[node]; ok && node != parsed.Root {
			nodes[name] = node
		}
	}
	return nodes
}

// ownMarks returns the lock nodes of the own inputs, and the configured own
// inputs that are not in the lock.
func ownMarks(lock []byte, checkouts map[string]string) (map[string]bool, []string) {
	own := map[string]bool{}
	nodes := ownInputNodes(lock, checkouts)
	for _, node := range nodes {
		own[node] = true
	}
	var unlocked []string
	for _, name := range sortedPaths(checkouts) {
		if _, ok := nodes[name]; !ok {
			unlocked = append(unlocked, name)
		}
	}
	return own, unlocked
}

// writeOwnInputs writes the Git state of each own input's checkout and how
// its HEAD compares with the revision the run's lock pins.
func writeOwnInputs(out *strings.Builder, lock []byte, checkouts map[string]string) {
	out.WriteString("\n## Own inputs\n\n")
	if len(checkouts) == 0 {
		out.WriteString("No input checkout is configured (inputCheckouts in the triage object of the inventory), so no input is known to be the operator's own.\n")
		return
	}
	out.WriteString("The operator's own inputs, as the triage configuration lists them, with the Git state of their local checkouts now. Fix an own input in a worktree of its checkout.\n")
	inputs, _ := lockInputs(lock)
	nodes := ownInputNodes(lock, checkouts)
	for _, name := range sortedPaths(checkouts) {
		path := checkouts[name]
		fmt.Fprintf(out, "\n### %s\n\n- Checkout: %s\n", name, path)
		pinned := ""
		if node, ok := nodes[name]; ok {
			pinned = inputs[node].Rev
			label := pinned
			if pinned == "" {
				label = "no Git revision (" + inputs[node].identity() + ")"
			}
			if node != name {
				label += ", lock node " + node
			}
			fmt.Fprintf(out, "- Locked by the run: %s\n", label)
		} else {
			out.WriteString("- Locked by the run: not in the run's lock, or it follows another input\n")
		}
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			out.WriteString("- The checkout is not a directory.\n")
			continue
		}
		head, ok := readGit(path, "rev-parse", "--verify", "-q", "HEAD")
		if !ok {
			out.WriteString("- Git cannot read a HEAD commit here.\n")
			continue
		}
		branch := "detached HEAD"
		if ref, ok := readGit(path, "symbolic-ref", "-q", "--short", "HEAD"); ok {
			branch = strings.TrimSpace(string(ref))
		}
		fmt.Fprintf(out, "- Branch: %s\n- HEAD: %s\n", branch, strings.TrimSpace(string(head)))
		status, ok := readGit(path, "status", "--porcelain", "--untracked-files=no")
		switch {
		case !ok:
			out.WriteString("- Tracked files: unknown\n")
		case len(strings.TrimSpace(string(status))) > 0:
			out.WriteString("- Tracked files: differ from HEAD\n")
		default:
			out.WriteString("- Tracked files: clean\n")
		}
		switch {
		case pinned == "":
			out.WriteString("- HEAD is the locked revision: unknown\n")
		case strings.TrimSpace(string(head)) == pinned:
			out.WriteString("- HEAD is the locked revision: yes\n")
		default:
			out.WriteString("- HEAD is the locked revision: no\n")
		}
	}
}

// launchDecision tells whether triage launches the agent, and why not.
func launchDecision(cfg *triageConfig, noLaunch, stdinTerminal, stdoutTerminal bool) (bool, string) {
	switch {
	case noLaunch:
		return false, "--no-launch given"
	case cfg == nil || len(cfg.AgentCommand) == 0:
		return false, "no agent command is configured; set agentCommand in the triage object of the inventory (--config FILE or NIX_DEPLOY_CONFIG) to launch one"
	case !stdinTerminal || !stdoutTerminal:
		return false, "standard input and output are not both terminals"
	}
	return true, ""
}

// playbookPath is the playbook installed beside the executable exe, in
// <exe dir>/../share/nixfleet.
func playbookPath(exe string) (string, error) {
	path := filepath.Join(filepath.Dir(exe), "..", "share", "nixfleet", "triage-playbook.md")
	if !fileExists(path) {
		return "", fmt.Errorf("the playbook %s is missing; nixfleet is not installed with its share directory", path)
	}
	return path, nil
}

func triagePrompt(context, playbook, addendum string, warnings bool) string {
	prompt := "A nixfleet deploy run needs triage. Read " + context + " first, then follow the playbook " + playbook + "."
	if warnings {
		prompt = "This is warning triage: a nixfleet deploy run printed evaluation warnings. Read " + context + " first, then follow the playbook " + playbook + ": its limits apply in every mode, and its Warnings section gives the procedure."
	}
	if addendum != "" {
		prompt += " Also follow the operator's playbook addendum " + addendum + "."
	}
	return prompt + " The operator approves any activation, push, merge or reboot; ask before each one."
}

// launchTriage runs the agent command with the prompt in the flake
// directory, on the inherited terminal, and returns its exit status.
func launchTriage(cfg *triageConfig, context, flakeDir, playbook string, warnings bool) int {
	if info, err := os.Stat(flakeDir); flakeDir == "" || err != nil || !info.IsDir() {
		fmt.Fprintf(os.Stderr, "nixfleet triage: the run's flake directory %s no longer exists; not launching the agent\n", flakeDir)
		return 1
	}
	argv := append(append([]string{}, cfg.AgentCommand...), triagePrompt(context, playbook, cfg.PlaybookAddendum, warnings))
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = flakeDir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// The terminal sends an interrupt or quit to the agent itself, so this
	// process only outlives it; a termination or hangup sent to this
	// process is passed on.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signals)
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "nixfleet triage: cannot start the agent command %s: %v\n", argv[0], err)
		return 1
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case sig := <-signals:
				if sig == syscall.SIGTERM || sig == syscall.SIGHUP {
					cmd.Process.Signal(sig)
				}
			case <-done:
				return
			}
		}
	}()
	cmd.Wait()
	if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal())
	}
	return cmd.ProcessState.ExitCode()
}
