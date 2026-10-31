package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

const triageUsage = `Usage: nixfleet triage [<run>]

Writes context.md for a recorded deploy run (ADR-0016): the run's facts, its
failed targets with the step that failed, the paths of their logs, the first
error of each with the files it names, the lock diff since each target's
baseline, and the store path of every locked input. <run> is a run ID, the name of a
directory under $XDG_STATE_HOME/nix-deploy/runs (by default ~/.local/state),
or the path of a run directory; without it, the newest run that failed.

The file is written into the run directory and its path printed. The command
fails when no run failed, the run does not exist, is still going on or has no
report.json. This version launches no agent.

A target's baseline is the flake.lock of the newest earlier run of the same
flake directory that shows it ok or unchanged (a build-only run proves no
deploy, so built does not count). Without one, it is the newest flake.lock
in the Git history, at or before the run's commit, that differs from the
run's lock: an approximation, not a known good deploy.
Store paths are computed from each input's narHash, without Nix or network.
`

// maxErrorLines caps the lines of one error kept in the context.
const maxErrorLines = 40

var errTriageHelp = errors.New("help requested")

func parseTriageArgs(args []string) (string, error) {
	run := ""
	for _, arg := range args {
		switch {
		case arg == "-h" || arg == "--help":
			return "", errTriageHelp
		case strings.HasPrefix(arg, "-"):
			return "", fmt.Errorf("unknown option '%s'", arg)
		case run != "":
			return "", fmt.Errorf("more than one run given")
		default:
			run = arg
		}
	}
	return run, nil
}

// runTriage is nixfleet triage.
func runTriage(args []string) int {
	run, err := parseTriageArgs(args)
	if errors.Is(err, errTriageHelp) {
		fmt.Print(triageUsage)
		return 0
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "nixfleet triage: %v\n", err)
		fmt.Fprint(os.Stderr, triageUsage)
		return 2
	}
	dir, err := selectRun(filepath.Join(stateHome(), "nix-deploy", "runs"), run)
	if err == nil {
		dir, err = writeTriageContext(dir)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "nixfleet triage: %v\n", err)
		return 1
	}
	fmt.Println(dir)
	return 0
}

// selectRun returns the absolute directory of the run named by an ID or a
// path, or without a name the newest run whose run.json says it failed.
func selectRun(runs, name string) (string, error) {
	dir := ""
	switch {
	case name == "":
		entries, err := os.ReadDir(runs)
		if err != nil && !os.IsNotExist(err) {
			return "", err
		}
		var ids []string
		for _, entry := range entries {
			if entry.IsDir() && runIDPattern.MatchString(entry.Name()) {
				ids = append(ids, entry.Name())
			}
		}
		sort.Strings(ids)
		for i := len(ids) - 1; i >= 0 && dir == ""; i-- {
			info, err := readRunInfo(filepath.Join(runs, ids[i]))
			if err == nil && info.Failed != nil && *info.Failed {
				dir = filepath.Join(runs, ids[i])
			}
		}
		if dir == "" {
			return "", fmt.Errorf("no failed run in %s", runs)
		}
	case strings.ContainsRune(name, filepath.Separator) || name == "." || name == "..":
		dir = name
	default:
		dir = filepath.Join(runs, name)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return "", fmt.Errorf("no run %s", name)
	}
	return filepath.Abs(dir)
}

func readRunInfo(dir string) (runInfo, error) {
	var info runInfo
	data, err := os.ReadFile(filepath.Join(dir, "run.json"))
	if err == nil {
		err = json.Unmarshal(data, &info)
	}
	return info, err
}

// runHeld reports whether a run still holds its run.lock, as a prune tests it.
func runHeld(dir string) (bool, error) {
	file, err := os.Open(filepath.Join(dir, runLockName))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return true, nil
		}
		return false, err
	}
	return false, nil
}

// failedTarget is a target whose report status is failed, with the step
// that failed: evaluate, build, activate, activation (activation time but no
// activate log), gate, or empty when the record does not tell.
type failedTarget struct {
	host   string
	phase  string // system or hm, as in log names
	label  string
	step   string
	report phaseReport
}

// failedStep derives the failed step: build when build.log lists the target
// as failed, gate when it reached the serial ready gate (only a switch that
// worked does), activate when it has an activate log, activation when it
// spent time in the activation step without a log (a terminal activation,
// or a fleet job whose setup failed before it activated), and evaluate when
// its evaluation log has an error.
func failedStep(dir string, t failedTarget, buildFailed map[string]bool) string {
	activate := filepath.Join(dir, "logs", t.phase+"-"+t.host+"-activate.log")
	switch {
	case buildFailed[t.phase+":"+t.host]:
		return "build"
	case t.report.GateSeconds > 0:
		return "gate"
	case fileExists(activate):
		return "activate"
	case t.report.ActivateSeconds > 0:
		return "activation"
	}
	block, _ := logError(filepath.Join(dir, "logs", t.phase+"-"+t.host+"-evaluate.log"))
	if block != nil {
		return "evaluate"
	}
	return ""
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// writeTriageContext writes context.md into a finished run's directory and
// returns its path.
func writeTriageContext(dir string) (string, error) {
	held, err := runHeld(dir)
	if err != nil {
		return "", err
	}
	if held {
		return "", fmt.Errorf("run %s is still going on", dir)
	}
	info, err := readRunInfo(dir)
	if err != nil {
		return "", fmt.Errorf("cannot read the run.json of %s: %v", dir, err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "report.json"))
	if os.IsNotExist(err) {
		return "", fmt.Errorf("run %s has no report.json; it was interrupted or ended before writing one", dir)
	}
	var rep runReport
	if err == nil {
		err = json.Unmarshal(data, &rep)
	}
	if err != nil {
		return "", fmt.Errorf("cannot read the report.json of %s: %v", dir, err)
	}

	buildFailed := map[string]bool{}
	if data, err := os.ReadFile(filepath.Join(dir, "logs", "build.log")); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if keys, ok := strings.CutPrefix(line, "failed targets: "); ok {
				for _, key := range strings.Fields(keys) {
					buildFailed[key] = true
				}
			}
		}
	}
	// Listed rather than globbed, so a run path with [ or * in it matches.
	var buildLogs []string
	entries, _ := os.ReadDir(filepath.Join(dir, "logs"))
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, "build-") && strings.HasSuffix(name, ".log") {
			buildLogs = append(buildLogs, filepath.Join(dir, "logs", name))
		}
	}
	var failed []failedTarget
	for _, host := range rep.Hosts {
		for _, t := range []failedTarget{
			{host: host.Host, phase: "system", label: "system", report: host.System},
			{host: host.Host, phase: "hm", label: "home-manager", report: host.HomeManager},
		} {
			if t.report.Status == "failed" {
				t.step = failedStep(dir, t, buildFailed)
				failed = append(failed, t)
			}
		}
	}

	var out strings.Builder
	writeRunSummary(&out, dir, info)
	out.WriteString("## Failed targets\n\n")
	if len(failed) == 0 {
		out.WriteString("No target has the status failed; see report.json.\n")
	}
	for _, t := range failed {
		fmt.Fprintf(&out, "- %s %s: %s\n", t.host, t.label, stepName(t.step))
	}
	anyBuild := false
	for _, t := range failed {
		fmt.Fprintf(&out, "\n## %s %s (%s)\n\n", t.host, t.label, stepName(t.step))
		// Every log the target has, whatever step failed, in step order. The
		// last one is the one whose tail is shown when it has no error line.
		var logs []string
		for _, step := range []string{"evaluate", "activate"} {
			if log := filepath.Join(dir, "logs", t.phase+"-"+t.host+"-"+step+".log"); fileExists(log) {
				logs = append(logs, log)
			}
		}
		relevant := ""
		if len(logs) > 0 {
			relevant = logs[len(logs)-1]
		}
		if t.step == "build" {
			if log := filepath.Join(dir, "logs", "build.log"); fileExists(log) {
				logs = append(logs, log)
			}
			logs = append(logs, buildLogs...)
		}
		if len(logs) == 0 {
			out.WriteString("This target has no log.\n\n")
		} else {
			out.WriteString("Logs:\n\n")
			for _, log := range logs {
				fmt.Fprintf(&out, "- %s\n", log)
			}
			out.WriteString("\n")
		}
		switch t.step {
		case "build":
			anyBuild = true
			out.WriteString("The first errors of the build logs are under \"Build errors\".\n")
		case "evaluate", "activate":
			log := filepath.Join(dir, "logs", t.phase+"-"+t.host+"-"+t.step+".log")
			fmt.Fprintf(&out, "From %s:\n", log)
			writeLogError(&out, log)
		case "activation":
			fmt.Fprintf(&out, "No activate log: the activation step or the setup just before it failed, and its output went to the terminal (ADR-0016). Its result is in %s.\n", filepath.Join(dir, "report.json"))
		case "gate":
			out.WriteString("The switch worked and the serial ready gate failed; the gate keeps the terminal and has no log.\n")
		default:
			fmt.Fprintf(&out, "The record does not tell which step failed (for example the build root setup, the activation record, a nixos-rebuild that cannot activate store paths, or a command that failed to start, which report on the terminal). See %s.\n", filepath.Join(dir, "report.json"))
			if relevant != "" {
				fmt.Fprintf(&out, "\nFrom %s:\n", relevant)
				writeLogError(&out, relevant)
			}
		}
	}
	if anyBuild {
		out.WriteString("\n## Build errors\n\n")
		if len(buildLogs) == 0 {
			out.WriteString("The run kept no build log of a derivation; the build kept the terminal. build.log says what was looked up.\n")
		}
		for _, log := range buildLogs {
			fmt.Fprintf(&out, "Log: %s\n", log)
			writeLogError(&out, log)
			out.WriteString("\n")
		}
	}
	writeLockContext(&out, dir, info, failed)

	path := filepath.Join(dir, "context.md")
	// replaceFile creates the file 0600 and renames it into place.
	if err := replaceFile(path, []byte(out.String())); err != nil {
		return "", err
	}
	return path, nil
}

func stepName(step string) string {
	switch step {
	case "":
		return "failed step unknown"
	case "activation":
		return "activation or its setup failed"
	}
	return step + " failed"
}

func writeRunSummary(out *strings.Builder, dir string, info runInfo) {
	fmt.Fprintf(out, "# nixfleet triage context\n\nRun record: %s\n\n## Run\n\n", dir)
	fmt.Fprintf(out, "- ID: %s\n- nixfleet version: %s\n- Started: %s\n", info.ID, info.Version, info.Started.Format(time.RFC3339))
	if info.Ended != nil {
		fmt.Fprintf(out, "- Ended: %s\n", info.Ended.Format(time.RFC3339))
	} else {
		out.WriteString("- Ended: not recorded\n")
	}
	fmt.Fprintf(out, "- Deploy arguments: `%s`\n- Flake directory: %s\n", strings.Join(info.Args, " "), info.FlakeDir)
	switch {
	case info.GitRev == "":
		out.WriteString("- Git revision: none (not a Git checkout)\n")
	case info.GitDirty == nil:
		fmt.Fprintf(out, "- Git revision: %s (whether tracked files differed is unknown)\n", info.GitRev)
	case *info.GitDirty:
		fmt.Fprintf(out, "- Git revision: %s, tracked files differed from it\n", info.GitRev)
	default:
		fmt.Fprintf(out, "- Git revision: %s, clean\n", info.GitRev)
	}
	var modes []string
	if info.DryRun {
		modes = append(modes, "dry-run")
	}
	if info.BuildOnly {
		modes = append(modes, "build-only")
	}
	if info.Force {
		modes = append(modes, "force")
	}
	if len(modes) == 0 {
		modes = append(modes, "none")
	}
	fmt.Fprintf(out, "- Modes: %s\n", strings.Join(modes, ", "))
	if info.LockSHA256 == "" {
		out.WriteString("- Lock SHA-256: none (no flake.lock)\n")
	} else {
		fmt.Fprintf(out, "- Lock SHA-256: %s (%s)\n", info.LockSHA256, filepath.Join(dir, "flake.lock"))
	}
	if info.StartLockSHA256 != "" {
		fmt.Fprintf(out, "- Start lock SHA-256: %s (%s). Nix rewrote the lock during evaluation.\n", info.StartLockSHA256, filepath.Join(dir, "flake.start.lock"))
	}
	fmt.Fprintf(out, "- Report: %s\n\n", filepath.Join(dir, "report.json"))
}

// tailLines is how many non-empty lines of a log without an error line the
// context shows instead.
const tailLines = 10

// writeLogError writes a log's first error and the files it names or,
// without an error line, the log's last non-empty lines, since a command
// that failed to start leaves only deploy's own message.
func writeLogError(out *strings.Builder, log string) {
	block, cut := logError(log)
	if block == nil {
		data, _ := os.ReadFile(log)
		var tail []string
		for _, line := range strings.Split(ansiPattern.ReplaceAllString(string(data), ""), "\n") {
			if line = strings.TrimRight(line, "\r"); strings.TrimSpace(line) != "" {
				tail = append(tail, line)
			}
		}
		if len(tail) > tailLines {
			tail = tail[len(tail)-tailLines:]
		}
		if len(tail) == 0 {
			out.WriteString("\nThe log is empty.\n")
			return
		}
		fmt.Fprintf(out, "\nNo line starting with \"error:\"; the last %d non-empty lines instead:\n\n````text\n%s\n````\n", len(tail), strings.Join(tail, "\n"))
		return
	}
	fmt.Fprintf(out, "\nFirst error:\n\n````text\n%s\n````\n", strings.Join(block, "\n"))
	if cut {
		fmt.Fprintf(out, "\nThe error is cut at %d lines.\n", maxErrorLines)
	}
	files := errorFiles(block)
	if len(files) == 0 {
		return
	}
	out.WriteString("\nFiles the error names:\n\n")
	for _, file := range files {
		fmt.Fprintf(out, "- %s\n", file)
	}
}

func logError(path string) ([]string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	return firstError(string(data))
}

var ansiPattern = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[@-Z\\-_]`)

// firstError returns the first line starting with "error:", ANSI escapes
// removed, and the lines that belong to it: Nix indents continuation lines
// and trace frames, so it stops at the next non-empty line that is not
// indented, or after maxErrorLines, which the second result reports.
func firstError(text string) ([]string, bool) {
	lines := strings.Split(ansiPattern.ReplaceAllString(text, ""), "\n")
	for i, line := range lines {
		if !strings.HasPrefix(line, "error:") {
			continue
		}
		block := []string{strings.TrimRight(line, "\r")}
		cut := false
		for _, next := range lines[i+1:] {
			next = strings.TrimRight(next, "\r")
			if next != "" && next[0] != ' ' && next[0] != '\t' {
				break
			}
			if len(block) == maxErrorLines {
				cut = true
				break
			}
			block = append(block, next)
		}
		for len(block) > 1 && strings.TrimSpace(block[len(block)-1]) == "" {
			block = block[:len(block)-1]
		}
		return block, cut
	}
	return nil, false
}

// filePattern matches a file in the store, /nix/store/<hash>-<name>[/<path>]
// with an optional :line[:col], and a flake reference frame of a trace,
// «<flakeref>»/<path>:line:col.
var filePattern = regexp.MustCompile(`/nix/store/[0-9a-z]{32}-[^/\s:'"«»,;()]+(?:/[^\s:'"«»,;()]+)?(?::[0-9]+(?::[0-9]+)?)?|«[^«»\s]+»/[^\s:'"«»,;()]+:[0-9]+:[0-9]+`)

// errorFiles returns the files an error names, without repeats, in order of
// appearance.
func errorFiles(block []string) []string {
	var files []string
	seen := map[string]bool{}
	for _, line := range block {
		for _, file := range filePattern.FindAllString(line, -1) {
			file = strings.TrimRight(file, ".")
			if !seen[file] {
				seen[file] = true
				files = append(files, file)
			}
		}
	}
	return files
}
