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

const triageUsage = `Usage: nixfleet triage [--config FILE] [--no-launch] [--warnings] [<run>]
       nixfleet triage [--config FILE] [--no-launch] [--warnings] --from-host HOST <copy>

Writes context.md for a recorded deploy run (ADR-0016): the run's facts, its
failed targets with the step that failed, the paths of their logs, what each
log says (the first error with the files it names, the units it reports
failed, its Go test failures grouped by message), earlier runs of the same
lock, the lock diff since each target's baseline, the store paths of the
inputs that matter (every locked input in inputs.md beside it), and the Git
state of the operator's own inputs. A failed build adds the attempted
derivations with the inputs behind them; a failed activation adds its
closure diff and the hosts' live state, read like nixfleet status (it needs
the inventory). <run> is a run ID, the name of a
directory under $XDG_STATE_HOME/nix-deploy/runs (by default ~/.local/state),
or the path of a run directory; without it, the newest run that failed.

The file is written into the run directory and its path printed. The command
fails when no run failed, the run does not exist, is still going on or has no
report.json.

The triage object of the inventory (--config FILE, or NIX_DEPLOY_CONFIG)
configures the rest: agentCommand, the agent's argv; inputCheckouts, the
local checkout of each of the operator's own flake inputs by input name;
playbookAddendum, a private Markdown file the agent also follows. With an
agentCommand, standard input and output on a terminal and no --no-launch,
triage then runs the agent in the run's flake directory with one more
argument, a prompt naming context.md, the playbook shipped with nixfleet and
the addendum, and exits with the agent's status. The agent's environment has
NIXFLEET_TRIAGE=1, so a deploy it runs does not offer triage again.

triageHost names the inventory host whose agent triages every host's runs.
On another host, where it would launch the agent, triage first checks over
SSH (BatchMode, DEPLOY_SSH_CHECK_TIMEOUT) that this host reaches the triage
host and that the triage host reaches back, one line each with its time.
When the triage host is unreachable, the agent runs here as without the
setting. When it cannot reach back, triage prints sshHint, the hint
saying how to let it, and hands over anyway, telling the agent. It copies
the run record to $XDG_STATE_HOME/nix-deploy/handoffs/<this host>/<run> on
the triage host (nixfleet triage --receive there) and runs
nixfleet triage --from-host <this host> <copy> there over ssh -t, which
launches the triage host's own agent in its own checkout of the flake: the
directory at the place of the run's flake directory relative to the home
directory. With --no-launch it runs only the checks.

With --warnings, the run is the newest finished one whatever its result
(or the one named), and context.md lists each distinct "evaluation
warning:" and "trace: warning:" message of its evaluation logs with the
targets that printed it, its kind, the options and files it names (each
file mapped to its locked input, own or upstream), and per target the
command that attributes it: a trace re-evaluation with abort-on-warn.
Names the message calls options are listed as hints; the playbook says how
to look up their definitions. nixfleet runs nothing. It also compares
the flake checkout now with the run's revision and lock, and has the own
inputs and the locked inputs, and no lock diff.

Options:
  --config FILE   Read the inventory from FILE instead of NIX_DEPLOY_CONFIG.
  --no-launch     Write context.md only.
  --warnings      Triage the warnings of a run instead of its failures.
  --from-host HOST
                  Launch the agent on a run record HOST handed over, given
                  as the copy's path; its context.md is not written again.
  --receive HOST  Unpack a run record HOST hands over from standard input,
                  given its run ID, and print the copy's path.

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

type triageOptions struct {
	configFile string
	noLaunch   bool
	warnings   bool
	run        string
	// fromHost and receive are the two halves of a hand-off on the triage
	// host: the source host whose copied record to triage, and the source
	// host whose record to receive on standard input, with run its ID.
	fromHost string
	receive  string
}

func parseTriageArgs(args []string) (triageOptions, error) {
	opts := triageOptions{configFile: os.Getenv("NIX_DEPLOY_CONFIG")}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-h" || arg == "--help":
			return opts, errTriageHelp
		case arg == "--no-launch":
			opts.noLaunch = true
		case arg == "--warnings":
			opts.warnings = true
		case arg == "--from-host" || arg == "--receive":
			if i+1 >= len(args) || !safeName.MatchString(args[i+1]) {
				return opts, fmt.Errorf("%s requires a host name", arg)
			}
			i++
			if arg == "--from-host" {
				opts.fromHost = args[i]
			} else {
				opts.receive = args[i]
			}
		case arg == "--config":
			if i+1 >= len(args) || args[i+1] == "" {
				return opts, fmt.Errorf("--config requires a file")
			}
			i++
			opts.configFile = args[i]
		case strings.HasPrefix(arg, "--config="):
			opts.configFile = strings.TrimPrefix(arg, "--config=")
			if opts.configFile == "" {
				return opts, fmt.Errorf("--config requires a file")
			}
		case strings.HasPrefix(arg, "-"):
			return opts, fmt.Errorf("unknown option '%s'", arg)
		case opts.run != "":
			return opts, fmt.Errorf("more than one run given")
		default:
			opts.run = arg
		}
	}
	switch {
	case opts.fromHost != "" && opts.receive != "":
		return opts, fmt.Errorf("--from-host and --receive exclude each other")
	case opts.receive != "" && (opts.run == "" || opts.noLaunch || opts.warnings):
		return opts, fmt.Errorf("--receive takes only a run ID")
	case opts.fromHost != "" && opts.run == "":
		return opts, fmt.Errorf("--from-host requires the copied run record")
	}
	return opts, nil
}

// runTriage is nixfleet triage.
func runTriage(args []string) int {
	opts, err := parseTriageArgs(args)
	if errors.Is(err, errTriageHelp) {
		fmt.Print(triageUsage)
		return 0
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "nixfleet triage: %v\n", err)
		fmt.Fprint(os.Stderr, triageUsage)
		return 2
	}
	cfg, warnings, err := loadTriageConfig(opts.configFile)
	unreadable := errors.Is(err, errInventoryUnreadable)
	if unreadable {
		warnings = append(warnings, err.Error())
	} else if err != nil {
		fmt.Fprintf(os.Stderr, "nixfleet triage: %v\n", err)
		return 1
	}
	for _, warning := range warnings {
		fmt.Fprintf(os.Stderr, "nixfleet triage: warning: %s\n", warning)
	}
	if opts.receive != "" {
		dest, err := receiveHandoff(opts.receive, opts.run, os.Stdin, opts.configFile, cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "nixfleet triage: %v\n", err)
			return 1
		}
		fmt.Println(dest)
		return 0
	}
	if opts.fromHost != "" {
		return triageFromHost(opts, cfg, unreadable)
	}
	var checkouts map[string]string
	if cfg != nil {
		checkouts = cfg.InputCheckouts
	}
	dir, err := selectRun(filepath.Join(stateHome(), "nix-deploy", "runs"), opts.run, opts.warnings)
	path := ""
	switch {
	case err != nil:
	case opts.warnings:
		// The inventory gives the Home Manager output prefix; without it the
		// commands show a placeholder.
		var inv *inventory
		noInventory := "No inventory is configured (--config FILE or NIX_DEPLOY_CONFIG)"
		if opts.configFile != "" {
			var invErr error
			if inv, invErr = loadInventory(opts.configFile); invErr != nil {
				noInventory = "The inventory could not be read or is invalid"
			}
		}
		path, err = writeWarningsContext(dir, checkouts, inv, noInventory)
	default:
		path, err = writeTriageContext(dir, checkouts, opts.configFile)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "nixfleet triage: %v\n", err)
		return 1
	}
	fmt.Println(path)
	launch, reason := launchDecision(cfg, opts.noLaunch, isTerminal(os.Stdin), isTerminal(os.Stdout))
	if unreadable {
		launch, reason = false, "the inventory could not be read"
	}
	// A host other than the triage host hands the run over when it would
	// launch the agent; with --no-launch it only runs the SSH checks.
	if !unreadable && (launch || opts.noLaunch) {
		if status, handed := handOverRun(cfg, opts, dir, launch); handed {
			return status
		}
	}
	if !launch {
		fmt.Fprintf(os.Stderr, "nixfleet triage: not launching an agent: %s\n", reason)
		return 0
	}
	exe, err := os.Executable()
	playbook := ""
	if err == nil {
		playbook, err = playbookPath(exe)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "nixfleet triage: %v\n", err)
		return 1
	}
	info, err := readRunInfo(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "nixfleet triage: %v\n", err)
		return 1
	}
	return launchTriage(cfg, path, info.FlakeDir, playbook, opts.warnings, "")
}

// selectRun returns the absolute directory of the run named by an ID or a
// path, or without a name the newest run whose run.json says it failed or,
// with finished, the newest finished run whatever its result: one with a
// report.json that no longer holds its run.lock.
func selectRun(runs, name string, finished bool) (string, error) {
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
			run := filepath.Join(runs, ids[i])
			if finished {
				if held, err := runHeld(run); err == nil && !held && fileExists(filepath.Join(run, "report.json")) {
					dir = run
				}
				continue
			}
			info, err := readRunInfo(run)
			if err == nil && info.Failed != nil && *info.Failed {
				dir = run
			}
		}
		if dir == "" && finished {
			return "", fmt.Errorf("no finished run in %s", runs)
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

// readFinishedRun reads the run.json and report.json of a run that no
// longer holds its run.lock.
func readFinishedRun(dir string) (runInfo, runReport, error) {
	var rep runReport
	held, err := runHeld(dir)
	if err != nil {
		return runInfo{}, rep, err
	}
	if held {
		return runInfo{}, rep, fmt.Errorf("run %s is still going on", dir)
	}
	info, err := readRunInfo(dir)
	if err != nil {
		return info, rep, fmt.Errorf("cannot read the run.json of %s: %v", dir, err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "report.json"))
	if os.IsNotExist(err) {
		return info, rep, fmt.Errorf("run %s has no report.json; it was interrupted or ended before writing one", dir)
	}
	if err == nil {
		err = json.Unmarshal(data, &rep)
	}
	if err != nil {
		return info, rep, fmt.Errorf("cannot read the report.json of %s: %v", dir, err)
	}
	return info, rep, nil
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
// returns its path; checkouts are the own inputs' checkouts by input name,
// and configFile the inventory, which live state needs. Beside it goes
// inputs.md, the table of every locked input, which context.md trims to the
// inputs that matter for the failure.
func writeTriageContext(dir string, checkouts map[string]string, configFile string) (string, error) {
	info, rep, err := readFinishedRun(dir)
	if err != nil {
		return "", err
	}

	buildFailed := map[string]bool{}
	buildSummary, _ := os.ReadFile(filepath.Join(dir, "logs", "build.log"))
	for _, line := range strings.Split(string(buildSummary), "\n") {
		if keys, ok := strings.CutPrefix(line, "failed targets: "); ok {
			for _, key := range strings.Fields(keys) {
				buildFailed[key] = true
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
	rebuildLog := filepath.Join(dir, "logs", nixRebuildLog)
	if !fileExists(rebuildLog) {
		rebuildLog = ""
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

	lock, lockErr := os.ReadFile(filepath.Join(dir, "flake.lock"))
	inputs, inputsErr := lockInputs(lock)
	if lockErr != nil {
		inputs, inputsErr = nil, nil
	}
	own, unlocked := ownMarks(lock, checkouts)
	nodeCheckouts := map[string]string{}
	for name, node := range ownInputNodes(lock, checkouts) {
		nodeCheckouts[node] = checkouts[name]
	}
	var baselines []baselineGroup
	if inputs != nil {
		baselines = lockBaselines(dir, info, lock, failed)
	}
	var buildBaseline *baselineGroup
	var liveHosts []string
	anyBuild := false
	for _, t := range failed {
		switch t.step {
		case "build":
			anyBuild = true
			for i := range baselines {
				if buildBaseline == nil && containsFailed(baselines[i].targets, t) {
					buildBaseline = &baselines[i]
				}
			}
		case "activate", "activation", "gate":
			if !contains(liveHosts, t.host) {
				liveHosts = append(liveHosts, t.host)
			}
		}
	}
	attempted := attemptedFromBuildLog(string(buildSummary))
	// A substituter outage can fail hundreds of source fetches at once; the
	// context lists the first maxAttempted and build.log has them all.
	attemptedTotal := len(attempted)
	if len(attempted) > maxAttempted {
		attempted = attempted[:maxAttempted]
	}
	facts := make([]drvFacts, len(attempted))
	for i, a := range attempted {
		facts[i] = readDrv(a.drv)
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
	writeNotes(&out, info, attempted, facts)
	for _, t := range failed {
		fmt.Fprintf(&out, "\n## %s %s (%s)\n\n", t.host, t.label, stepName(t.step))
		// Every log the target has, whatever step failed, in step order. The
		// last one is the one whose tail is shown when it has no error line.
		var logs []string
		for _, step := range []string{"evaluate", "activate", "closure"} {
			if log := filepath.Join(dir, "logs", t.phase+"-"+t.host+"-"+step+".log"); fileExists(log) {
				logs = append(logs, log)
			}
		}
		relevant := ""
		for _, log := range logs {
			if !strings.HasSuffix(log, "-closure.log") {
				relevant = log
			}
		}
		if t.step == "build" {
			if log := filepath.Join(dir, "logs", "build.log"); fileExists(log) {
				logs = append(logs, log)
			}
			logs = append(logs, buildLogs...)
			if rebuildLog != "" {
				logs = append(logs, rebuildLog)
			}
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
			out.WriteString("The first errors of the build logs are under \"Build errors\". The failed generation was not built, so there is no closure diff; the lock diff and the attempted derivations stand in for it.\n")
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
		if log := filepath.Join(dir, "logs", t.phase+"-"+t.host+"-closure.log"); fileExists(log) {
			writeClosureDiff(&out, log)
		}
	}
	if len(liveHosts) > 0 {
		out.WriteString("\n## Live state\n\nAn activation failed, so the host may be degraded now. This is what it looks like as triage writes this file, not what the run recorded.\n\n")
		out.WriteString(readLiveState(configFile, liveHosts))
	}
	if anyBuild {
		out.WriteString("\n## Build errors\n\n")
		if len(buildLogs) == 0 && rebuildLog == "" {
			out.WriteString("The run kept no build log of a derivation; the build kept the terminal. build.log says what was looked up.\n")
		}
		silent := false
		// Two derivations that differ only in an attribute the build does
		// not read leave the same log; it is shown once.
		seenLogs := map[string]string{}
		for _, log := range buildLogs {
			fmt.Fprintf(&out, "Log: %s\n", log)
			data, _ := os.ReadFile(log)
			sum := lockSum(data)
			if first, ok := seenLogs[sum]; ok && len(data) > 0 {
				fmt.Fprintf(&out, "\nThe same content as %s.\n\n", first)
				continue
			}
			seenLogs[sum] = log
			if !writeLogError(&out, log) {
				silent = true
			}
			out.WriteString("\n")
		}
		if rebuildLog != "" {
			fmt.Fprintf(&out, "Nix's own output from building the attempted fixed-output derivations again after the failure, where a hash mismatch shows: %s\n", rebuildLog)
			writeLogError(&out, rebuildLog)
			out.WriteString("\n")
		}
		if silent || len(buildLogs) == 0 {
			out.WriteString("Nix reports some failures itself rather than through the builder, such as a fixed-output hash mismatch or a missing output, and the run records Nix's own output only for the fixed-output rebuild above. When no log explains the failure, reproduce it with `deploy --build-only` and read the terminal.\n")
		}
		if len(attempted) > 0 {
			writeAttempted(&out, attempted, attemptedTotal, facts, inputs, own, nodeCheckouts, buildBaseline)
		}
	}
	writeEarlierRuns(&out, dir, info)

	inputsPath := filepath.Join(dir, "inputs.md")
	switch {
	case lockErr != nil:
		out.WriteString("\n## Lock diff\n\nThe run kept no flake.lock, so there is no lock diff and no input list.\n")
	case inputsErr != nil:
		fmt.Fprintf(&out, "\n## Lock diff\n\nThe run's flake.lock cannot be read: %v\n", inputsErr)
	default:
		writeLockContext(&out, dir, inputs, baselines)
		// The context lists the inputs that changed, the own ones, those
		// behind an attempted derivation and those whose store path it
		// names; inputs.md has them all.
		only := changedInputs(baselines, inputs)
		for name := range own {
			only[name] = true
		}
		byPath := inputsByStorePath(inputs)
		for _, f := range facts {
			for _, name := range drvInputs(f, byPath) {
				only[name] = true
			}
		}
		for _, ref := range storePathRef.FindAllString(out.String(), -1) {
			for path, names := range byPath {
				if strings.HasPrefix(ref, path) {
					for _, name := range names {
						only[name] = true
					}
				}
			}
		}
		var full strings.Builder
		fmt.Fprintf(&full, "# Locked inputs of run %s\n", info.ID)
		writeLockedInputs(&full, inputs, own, unlocked, nil, "")
		if err := replaceFile(inputsPath, []byte(full.String())); err != nil {
			return "", err
		}
		writeLockedInputs(&out, inputs, own, unlocked, only, inputsPath)
	}
	writeOwnInputs(&out, "Own inputs", lock, checkouts)

	path := filepath.Join(dir, "context.md")
	// replaceFile creates the file 0600 and renames it into place.
	if err := replaceFile(path, []byte(out.String())); err != nil {
		return "", err
	}
	return path, nil
}

func containsFailed(targets []failedTarget, t failedTarget) bool {
	for _, other := range targets {
		if other.host == t.host && other.phase == t.phase {
			return true
		}
	}
	return false
}

// writeNotes writes one-line notes about the failure: when the failing
// package is nixfleet itself, and when an attempted derivation builds for a
// system nix-packages does not build natively before promotion.
func writeNotes(out *strings.Builder, info runInfo, attempted []attemptedDerivation, facts []drvFacts) {
	var notes []string
	for _, a := range attempted {
		if isNixfleet(a.drv) {
			notes = append(notes, fmt.Sprintf("The failing package is nixfleet itself (%s). This run used nixfleet %s, so the tool, its playbook and its tests in this run are not the code that failed; that is why the tool still works while its update cannot build.", derivationName(a.drv), info.Version))
			break
		}
	}
	var systems []string
	for _, f := range facts {
		// builtin is Nix's own fetcher, not a platform anything is built for.
		if f.system != "" && f.system != "x86_64-linux" && f.system != "builtin" && !contains(systems, f.system) {
			systems = append(systems, f.system)
		}
	}
	for _, system := range systems {
		notes = append(notes, fmt.Sprintf("An attempted derivation builds for %s. nix-packages builds natively only for x86_64-linux before it promotes a revision and only evaluates the other systems (its ADR-0003), so a nix-packages package failing here may be failing its first native build on %s.", system, system))
	}
	if len(notes) == 0 {
		return
	}
	out.WriteString("\n## Notes\n\n")
	for _, note := range notes {
		fmt.Fprintf(out, "- %s\n", note)
	}
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

// writeLogError writes what a log says about its failure: its first error
// and the files it names, the units it says failed, and its Go test
// failures grouped by message. Without any of those it says so plainly and
// shows the log's last non-empty lines, since a command that failed to
// start leaves only deploy's own message; it then returns false.
func writeLogError(out *strings.Builder, log string) bool {
	data, _ := os.ReadFile(log)
	// Nix's raw log format marks each line with a syslog severity, such as
	// <3>error:, when it runs under systemd; without that, a hash mismatch in
	// nix-rebuild.log would not count as an error line.
	text := severityPattern.ReplaceAllString(ansiPattern.ReplaceAllString(string(data), ""), "")
	found := false
	if block, cut := firstError(text); block != nil {
		found = true
		fmt.Fprintf(out, "\nFirst error:\n\n````text\n%s\n````\n", strings.Join(block, "\n"))
		if cut {
			fmt.Fprintf(out, "\nThe error is cut at %d lines.\n", maxErrorLines)
		}
		if files := errorFiles(block); len(files) > 0 {
			out.WriteString("\nFiles the error names:\n\n")
			for _, file := range files {
				fmt.Fprintf(out, "- %s\n", file)
			}
		}
	}
	if units, more := unitFailures(text); len(units) > 0 {
		found = true
		fmt.Fprintf(out, "\nUnits that failed, as the log reports them:\n\n````text\n%s\n````\n", strings.Join(units, "\n"))
		if more {
			fmt.Fprintf(out, "\nOnly the first %d such lines are shown.\n", maxFindingLines)
		}
	}
	if tests := goTestFailures(text); len(tests.tests) > 0 {
		found = true
		writeTestFailures(out, tests)
	}
	if found {
		return true
	}
	var tail []string
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimRight(line, "\r"); strings.TrimSpace(line) != "" {
			tail = append(tail, line)
		}
	}
	if len(tail) > tailLines {
		tail = tail[len(tail)-tailLines:]
	}
	if len(tail) == 0 {
		out.WriteString("\nThe log is empty.\n")
		return false
	}
	fmt.Fprintf(out, "\nNo error found: no line starts with \"error:\", and none reports a failed unit or a failed Go test. The last %d non-empty lines follow only as the end of the log; they need not explain the failure.\n\n````text\n%s\n````\n", len(tail), strings.Join(tail, "\n"))
	return false
}

func logError(path string) ([]string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	return firstError(string(data))
}

// severityPattern is the syslog severity prefix Nix's raw log format puts on
// each line under systemd (IN_SYSTEMD=1).
var severityPattern = regexp.MustCompile(`(?m)^<[0-7]>`)

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
