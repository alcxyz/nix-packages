package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// runRecord keeps the evidence of one deploy run on this machine, so a failed
// run can be examined after its terminal output is gone (#673, ADR-0016):
// run.json, the flake.lock the run deployed, report.json and, under logs/,
// the raw output of each step whose output deploy already reads (every
// evaluation, the parallel fleet jobs and switch-mode system switches) and a
// failed build's Nix logs. Which lock a failed target last deployed well from is left to the
// diagnosis, which reads it from older records.
//
// The record is evidence, not a safety mechanism: the first failure to write
// it prints one warning, and the run goes on as it would without it.
type runRecord struct {
	// state is the nix-deploy state directory; dir is this run's directory,
	// empty when it could not be created.
	state string
	dir   string
	keep  int
	info  runInfo
	// active holds the lock on the run directory for the run's lifetime, so
	// another run's prune leaves the directory alone; the kernel releases it
	// when the process ends.
	active *os.File

	warned sync.Once
}

// runInfo is run.json. Ended and Failed are absent while the run is going on,
// and stay absent when it was interrupted or died. LockSHA256 is the lock the
// run deployed; StartLockSHA256 is set only when evaluation rewrote the lock
// the run started with.
type runInfo struct {
	ID              string     `json:"id"`
	Version         string     `json:"version"`
	Started         time.Time  `json:"started"`
	Ended           *time.Time `json:"ended,omitempty"`
	Args            []string   `json:"args"`
	FlakeDir        string     `json:"flakeDir"`
	GitRev          string     `json:"gitRevision,omitempty"`
	GitDirty        *bool      `json:"gitDirty,omitempty"`
	LockSHA256      string     `json:"lockSha256,omitempty"`
	StartLockSHA256 string     `json:"startLockSha256,omitempty"`
	DryRun          bool       `json:"dryRun"`
	BuildOnly       bool       `json:"buildOnly"`
	Force           bool       `json:"force"`
	Failed          *bool      `json:"failed,omitempty"`
}

var runIDPattern = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}\.[0-9]{6}Z-[0-9]+(-[0-9]+)?$`)

// runLockName is the file in a run directory its run holds locked.
const runLockName = "run.lock"

// runID names a run directory: the UTC start time to the microsecond, which
// sorts like the time, then the process ID, which tells apart runs started
// in the same microsecond.
func runID(started time.Time, pid int) string {
	return started.UTC().Format("20060102T150405.000000Z") + "-" + strconv.Itoa(pid)
}

func lockSum(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// startRunRecord creates the run's directory under the state directory and
// writes run.json and the flake.lock of flakeDir, the directory whose flake
// (.) the run deploys.
func startRunRecord(args []string, flakeDir string, dryRun, buildOnly, force bool, keep int) *runRecord {
	started := time.Now().UTC()
	rec := &runRecord{
		state: filepath.Join(stateHome(), "nix-deploy"),
		keep:  keep,
		info: runInfo{
			Version:   version,
			Started:   started,
			Args:      args,
			FlakeDir:  flakeDir,
			DryRun:    dryRun,
			BuildOnly: buildOnly,
			Force:     force,
		},
	}
	if rec.info.Args == nil {
		rec.info.Args = []string{}
	}
	// A flake without a lock has nothing to compare a later run with.
	lock, err := os.ReadFile(filepath.Join(flakeDir, "flake.lock"))
	if err != nil && !os.IsNotExist(err) {
		rec.warn(err)
	}
	if err == nil {
		rec.info.LockSHA256 = lockSum(lock)
	}
	rec.info.GitRev, rec.info.GitDirty = gitState(flakeDir)

	runs := filepath.Join(rec.state, "runs")
	if err := os.MkdirAll(runs, 0o700); err != nil {
		rec.warn(err)
		return rec
	}
	id := runID(started, os.Getpid())
	for n := 1; ; n++ {
		err = os.Mkdir(filepath.Join(runs, id), 0o700)
		if !os.IsExist(err) || n == 10 {
			break
		}
		id = runID(started, os.Getpid()) + "-" + strconv.Itoa(n)
	}
	if err != nil {
		rec.warn(err)
		return rec
	}
	rec.dir = filepath.Join(runs, id)
	rec.info.ID = id
	// Taken first, so the directory is unlocked only in the moment between
	// its creation and this; it is the newest run then, which a prune keeps.
	rec.active, err = os.OpenFile(filepath.Join(rec.dir, runLockName), os.O_CREATE|os.O_RDWR, 0o600)
	if err == nil {
		err = syscall.Flock(int(rec.active.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	}
	if err == nil {
		err = os.Mkdir(filepath.Join(rec.dir, "logs"), 0o700)
	}
	if err != nil {
		rec.warn(err)
		rec.dir = ""
		return rec
	}
	rec.writeInfo()
	if lock != nil {
		if err := os.WriteFile(filepath.Join(rec.dir, "flake.lock"), lock, 0o600); err != nil {
			rec.warn(err)
		}
	}
	return rec
}

// noteEvaluatedLock records the lock the evaluation used. Nix rewrites a
// flake.lock that no longer matches flake.nix while it evaluates (an input
// added or removed, for example), so the lock at the start is not always the
// one deployed. A changed lock becomes the run's flake.lock and lockSha256,
// and the one it started with stays as flake.start.lock and startLockSha256.
func (rec *runRecord) noteEvaluatedLock() {
	if rec == nil || rec.dir == "" {
		return
	}
	lock, err := os.ReadFile(filepath.Join(rec.info.FlakeDir, "flake.lock"))
	if os.IsNotExist(err) {
		if _, statErr := os.Stat(filepath.Join(rec.info.FlakeDir, "flake.nix")); statErr == nil {
			err = fmt.Errorf("no flake.lock beside %s after evaluation; the record has no lock", filepath.Join(rec.info.FlakeDir, "flake.nix"))
		} else {
			return
		}
	}
	if err != nil {
		rec.warn(err)
		return
	}
	sum := lockSum(lock)
	if sum == rec.info.LockSHA256 {
		return
	}
	if rec.info.LockSHA256 != "" {
		if err := os.Rename(filepath.Join(rec.dir, "flake.lock"), filepath.Join(rec.dir, "flake.start.lock")); err != nil {
			rec.warn(err)
		}
	}
	rec.info.StartLockSHA256 = rec.info.LockSHA256
	rec.info.LockSHA256 = sum
	rec.writeInfo()
	if err := os.WriteFile(filepath.Join(rec.dir, "flake.lock"), lock, 0o600); err != nil {
		rec.warn(err)
	}
}

// findFlakeDir returns the directory of the flake Nix uses for "." from dir:
// the nearest directory, dir or above, that holds a flake.nix. Like Nix, the
// search stops at the root of the Git checkout dir is in (a directory with
// .git) and at the filesystem root. Without a flake it returns dir, where
// Nix's own evaluation then fails.
func findFlakeDir(dir string) string {
	if dir == "" {
		return dir
	}
	for current := dir; ; {
		if _, err := os.Stat(filepath.Join(current, "flake.nix")); err == nil {
			return current
		}
		if _, err := os.Lstat(filepath.Join(current, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(current)
		if parent == current {
			return dir
		}
		current = parent
	}
}

// gitState returns the flake checkout's commit and whether tracked files
// differ from it, or nothing when it is not a Git checkout. Untracked files
// are not part of a Git flake, so they do not count.
func gitState(dir string) (string, *bool) {
	git := func(args ...string) (string, bool) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
		// Reading the state must not take the index lock from the operator.
		cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
		output, err := cmd.Output()
		return strings.TrimSpace(string(output)), err == nil
	}
	rev, ok := git("rev-parse", "HEAD")
	if !ok {
		return "", nil
	}
	changes, ok := git("status", "--porcelain", "--untracked-files=no")
	if !ok {
		return rev, nil
	}
	dirty := changes != ""
	return rev, &dirty
}

// warn reports the first failure to keep the record; later ones would only
// repeat it.
func (rec *runRecord) warn(err error) {
	rec.warned.Do(func() {
		fmt.Fprintf(os.Stderr, "deploy: warning: run record: %v\n", err)
	})
}

func (rec *runRecord) writeInfo() {
	data, err := json.MarshalIndent(rec.info, "", "  ")
	if err == nil {
		err = replaceFile(filepath.Join(rec.dir, "run.json"), append(data, '\n'))
	}
	if err != nil {
		rec.warn(err)
	}
}

// stepLog appends one step's output to its file under logs/. Its writes never
// fail, so a full disk cannot stop the command it records. The step closes it
// when it ends, so a run's open files do not grow with its targets; a step
// that runs again appends to the same file.
type stepLog struct {
	rec  *runRecord
	file *os.File
}

// log opens logs/name, or returns nil without a record.
func (rec *runRecord) log(name string) *stepLog {
	if rec == nil || rec.dir == "" {
		return nil
	}
	file, err := os.OpenFile(filepath.Join(rec.dir, "logs", name), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		rec.warn(err)
		return nil
	}
	return &stepLog{rec: rec, file: file}
}

func (l *stepLog) Write(data []byte) (int, error) {
	if _, err := l.file.Write(data); err != nil {
		l.rec.warn(err)
	}
	return len(data), nil
}

// Close closes the log; it does nothing without one.
func (l *stepLog) Close() {
	if l == nil {
		return
	}
	if err := l.file.Close(); err != nil {
		l.rec.warn(err)
	}
}

// tee copies output to log as well, leaving dst alone without a log so a
// terminal stays attached to the command.
func tee(dst io.Writer, log *stepLog) io.Writer {
	if log == nil {
		return dst
	}
	return io.MultiWriter(dst, log)
}

// logged returns streams that also copy a target step's output to its log,
// logs/<phase>-<host>-<step>.log, and the log, which the caller closes when
// the step ends.
func (d *deployer) logged(streams stdio, t target, step string) (stdio, *stepLog) {
	log := d.record.log(t.phase + "-" + t.host + "-" + step + ".log")
	return stdio{in: streams.in, out: tee(streams.out, log), err: tee(streams.err, log)}, log
}

// switchLogged returns the terminal's streams for a system activation that
// keeps them, with the step log set for the switch output activateSystem
// already reads through a buffer, and the log, which the caller closes. A
// boot-mode or nix-darwin activation writes to the terminal and has no log.
func (d *deployer) switchLogged(t target) (stdio, *stepLog) {
	if !d.readsSwitchOutput(t.host) {
		return attached, nil
	}
	log := d.record.log(t.phase + "-" + t.host + "-activate.log")
	streams := attached
	if log != nil {
		streams.log = log
	}
	return streams, log
}

// Build logs looked up after a failed build: at most maxBuildLogs are kept,
// at most maxBuildLogLookups derivations are tried, since most of what a mass
// failure leaves to build never started and has no log, and listing what is
// left to build and the lookups together stop after buildLogBudget, so a
// failed run does not linger over its evidence.
const (
	maxBuildLogs       = 20
	maxBuildLogLookups = 200
	buildLogBudget     = 60 * time.Second
	buildLogTimeout    = 10 * time.Second
)

// recordBuild writes logs/build.log, a summary of the shared build, and,
// when targets failed, the build logs Nix kept of the derivations they still
// need, as logs/build-<derivation name>.log. The build keeps the terminal, so
// its output is not copied; Nix's log store has the whole log of a failed
// derivation, where the terminal showed only its tail. What is left to build
// is what `nix build --dry-run` lists under "will be built": the derivations
// that failed and the ones that waited on them. With --keep-going the build
// attempted every one whose inputs were all there, so the attempted ones are
// those none of whose input derivations is pending, to build or to fetch (a
// download that failed); only their logs are looked up, since Nix may hold an
// old log of a waiting one from an earlier build.
func (d *deployer) recordBuild(argv []string, status int, failed []target, evaluated map[string]evaluation) {
	rec := d.record
	if rec == nil || rec.dir == "" {
		return
	}
	var summary strings.Builder
	fmt.Fprintf(&summary, "command: %s\nexit status: %d\n", strings.Join(argv, " "), status)
	if len(failed) > 0 {
		var keys, installables []string
		for _, t := range failed {
			keys = append(keys, t.key())
			installables = append(installables, evaluated[t.key()].Drv+"^out")
		}
		fmt.Fprintf(&summary, "failed targets: %s\n", strings.Join(keys, " "))
		deadline := time.Now().Add(buildLogBudget)
		drvs, fetches, err := pendingBuilds(installables, buildLogBudget)
		if err != nil {
			fmt.Fprintf(&summary, "cannot list the derivations left to build: %v\n", err)
		} else {
			fmt.Fprintf(&summary, "derivations left to build: %d, paths left to fetch: %d\n", len(drvs), len(fetches))
			if len(drvs) > 0 {
				facts, err := pendingInputs(drvs, fetches, deadline)
				if err != nil {
					fmt.Fprintf(&summary, "cannot tell which were attempted (%v); looking up all of them\n", err)
				} else {
					drvs = attemptedDrvs(drvs, fetches, facts)
					fmt.Fprintf(&summary, "attempted, with no pending input: %d\n", len(drvs))
				}
			}
			logs, tried := collectBuildLogs(drvs, deadline, nixBuildLog)
			if tried < len(drvs) {
				fmt.Fprintf(&summary, "looked up the logs of the first %d\n", tried)
			}
			for _, found := range logs {
				name := buildLogName(found.drv)
				if log := rec.log(name); log != nil {
					log.Write(found.log)
					log.Close()
					fmt.Fprintf(&summary, "log of %s: %s\n", found.drv, name)
				}
			}
		}
	}
	if log := rec.log("build.log"); log != nil {
		io.WriteString(log, summary.String())
		log.Close()
	}
}

// derivationFacts are, by store path name, a derivation's input derivations
// with the output names it takes from each, and its output paths by output
// name.
type derivationFacts struct {
	inputs  map[string][]string
	outputs map[string]string
}

// pendingInputs shows the pending derivations and, when paths are left to
// fetch, their other input derivations too, whose outputs tell which of them
// wait on a download.
func pendingInputs(drvs, fetches []string, deadline time.Time) (map[string]derivationFacts, error) {
	facts, err := showDerivations(drvs, time.Until(deadline))
	if err != nil || len(fetches) == 0 {
		return facts, err
	}
	store := filepath.Dir(drvs[0])
	seen := map[string]bool{}
	var others []string
	for _, f := range facts {
		for input := range f.inputs {
			if _, shown := facts[input]; !shown && !seen[input] {
				seen[input] = true
				others = append(others, filepath.Join(store, input))
			}
		}
	}
	if len(others) == 0 {
		return facts, nil
	}
	more, err := showDerivations(others, time.Until(deadline))
	if err != nil {
		return nil, err
	}
	for name, f := range more {
		facts[name] = f
	}
	return facts, nil
}

// showDerivations runs one `nix derivation show` of drvs within timeout.
func showDerivations(drvs []string, timeout time.Duration) (map[string]derivationFacts, error) {
	if timeout <= 0 {
		return nil, fmt.Errorf("no time left")
	}
	var output bytes.Buffer
	show := command{argv: append([]string{"nix", "derivation", "show"}, drvs...), stdin: bytes.NewReader(nil), stdout: &output, stderr: io.Discard, timeout: timeout}
	if status := show.run(); status != 0 {
		return nil, fmt.Errorf("nix derivation show exited with %d", status)
	}
	return parseDerivations(output.Bytes())
}

// parseDerivations reads `nix derivation show`. Recent Nix (2.35 here) wraps
// the derivations in {"derivations": ..., "version": N}, names inputs (under
// inputs.drvs, each with the outputs it takes) and output paths by store path
// name, and gives a fixed-output path only in env; older Nix and Lix key them
// by store path, with inputs under inputDrvs as a list of outputs or the same
// object. All become store path names.
func parseDerivations(data []byte) (map[string]derivationFacts, error) {
	type derivation struct {
		InputDrvs map[string]json.RawMessage `json:"inputDrvs"`
		Inputs    struct {
			Drvs map[string]json.RawMessage `json:"drvs"`
		} `json:"inputs"`
		Outputs map[string]struct {
			Path string `json:"path"`
		} `json:"outputs"`
		Env map[string]string `json:"env"`
	}
	var wrapped struct {
		Derivations map[string]derivation `json:"derivations"`
	}
	if err := json.Unmarshal(data, &wrapped); err != nil {
		return nil, err
	}
	derivations := wrapped.Derivations
	if derivations == nil {
		if err := json.Unmarshal(data, &derivations); err != nil {
			return nil, err
		}
	}
	if len(derivations) == 0 {
		return nil, fmt.Errorf("no derivations in the output")
	}
	facts := map[string]derivationFacts{}
	for path, drv := range derivations {
		f := derivationFacts{inputs: map[string][]string{}, outputs: map[string]string{}}
		for _, inputs := range []map[string]json.RawMessage{drv.InputDrvs, drv.Inputs.Drvs} {
			for input, used := range inputs {
				outputs, err := inputOutputs(used)
				if err != nil {
					return nil, fmt.Errorf("inputs of %s: %w", path, err)
				}
				f.inputs[filepath.Base(input)] = outputs
			}
		}
		for name, output := range drv.Outputs {
			out := output.Path
			if out == "" && strings.HasPrefix(drv.Env[name], "/") {
				out = drv.Env[name]
			}
			if out != "" {
				f.outputs[name] = filepath.Base(out)
			}
		}
		facts[filepath.Base(path)] = f
	}
	return facts, nil
}

// inputOutputs reads the outputs a derivation takes from one input: a list of
// output names, or an object with them under outputs.
func inputOutputs(used json.RawMessage) ([]string, error) {
	var names []string
	if json.Unmarshal(used, &names) == nil {
		return names, nil
	}
	var object struct {
		Outputs []string `json:"outputs"`
	}
	if err := json.Unmarshal(used, &object); err != nil {
		return nil, err
	}
	return object.Outputs, nil
}

// attemptedDrvs keeps, in order, the pending derivations none of whose inputs
// is pending: an input derivation left to build, or an output it takes that
// is left to fetch. Those are the ones the build could start.
func attemptedDrvs(pending, fetches []string, facts map[string]derivationFacts) []string {
	waiting := map[string]bool{}
	for _, drv := range pending {
		waiting[filepath.Base(drv)] = true
	}
	fetching := map[string]bool{}
	for _, path := range fetches {
		fetching[filepath.Base(path)] = true
	}
	var attempted []string
	for _, drv := range pending {
		ready := true
		for input, outputs := range facts[filepath.Base(drv)].inputs {
			if waiting[input] {
				ready = false
				break
			}
			for _, output := range outputs {
				if fetching[facts[input].outputs[output]] {
					ready = false
				}
			}
			if !ready {
				break
			}
		}
		if ready {
			attempted = append(attempted, drv)
		}
	}
	return attempted
}

type buildLog struct {
	drv string
	log []byte
}

// collectBuildLogs looks up the build log of each derivation in order until
// it has maxBuildLogs of them, has tried maxBuildLogLookups or reaches the
// deadline, and returns the logs found and how many it tried. Each lookup
// gets at most the time left.
func collectBuildLogs(drvs []string, deadline time.Time, lookup func(drv string, timeout time.Duration) []byte) ([]buildLog, int) {
	var found []buildLog
	tried := 0
	for _, drv := range drvs {
		left := time.Until(deadline)
		if len(found) == maxBuildLogs || tried == maxBuildLogLookups || left <= 0 {
			break
		}
		tried++
		if left > buildLogTimeout {
			left = buildLogTimeout
		}
		if log := lookup(drv, left); len(log) > 0 {
			found = append(found, buildLog{drv: drv, log: log})
		}
	}
	return found, tried
}

// nixBuildLog returns the build log this machine's Nix kept of a derivation,
// or nothing. With no substituters, nix log asks only the local store (or its
// daemon), so a miss costs no network round trip.
func nixBuildLog(drv string, timeout time.Duration) []byte {
	var output bytes.Buffer
	argv := []string{"nix", "log", "--option", "substituters", "", drv}
	lookup := command{argv: argv, stdin: bytes.NewReader(nil), stdout: &output, stderr: io.Discard, timeout: timeout}
	if lookup.run() != 0 {
		return nil
	}
	return output.Bytes()
}

// buildLogName names the log file of a derivation's build.
func buildLogName(drv string) string {
	return "build-" + strings.TrimSuffix(filepath.Base(drv), ".drv") + ".log"
}

// finishRecord completes the run's record: report.json, the end of run.json
// and the pruning of old runs. A failed run names its record.
func (d *deployer) finishRecord(selected []string, r *report, failed bool) {
	rec := d.record
	if rec == nil || rec.dir == "" {
		return
	}
	data, err := d.reportData(selected, r)
	if err == nil {
		err = replaceFile(filepath.Join(rec.dir, "report.json"), data)
	}
	if err != nil {
		rec.warn(err)
	}
	rec.finish(failed)
	if failed {
		fmt.Fprintf(os.Stderr, "deploy: run record: %s\n", rec.dir)
	}
}

func (rec *runRecord) finish(failed bool) {
	ended := time.Now().UTC()
	rec.info.Ended = &ended
	rec.info.Failed = &failed
	rec.writeInfo()
	if err := rec.prune(); err != nil {
		rec.warn(err)
	}
}

// removeIdleRun removes a run directory unless its run still holds it, and
// reports whether it did.
func removeIdleRun(dir string) (bool, error) {
	file, err := os.OpenFile(filepath.Join(dir, runLockName), os.O_RDWR, 0)
	if err == nil {
		defer file.Close()
		if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			if errors.Is(err, syscall.EWOULDBLOCK) {
				return false, nil
			}
			return false, err
		}
	} else if !os.IsNotExist(err) {
		return false, err
	}
	// A run without a lock file ended before it took one.
	return true, os.RemoveAll(dir)
}

// prune removes run directories beyond the newest rec.keep, except this one
// and any whose run is still going on.
func (rec *runRecord) prune() error {
	runs := filepath.Join(rec.state, "runs")
	entries, err := os.ReadDir(runs)
	if err != nil {
		return err
	}
	var ids []string
	for _, entry := range entries {
		if entry.IsDir() && runIDPattern.MatchString(entry.Name()) {
			ids = append(ids, entry.Name())
		}
	}
	sort.Strings(ids)
	kept := map[string]bool{rec.info.ID: true}
	for i := len(ids) - 1; i >= 0 && i >= len(ids)-rec.keep; i-- {
		kept[ids[i]] = true
	}
	var firstErr error
	for _, id := range ids {
		if kept[id] {
			continue
		}
		if _, err := removeIdleRun(filepath.Join(runs, id)); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
