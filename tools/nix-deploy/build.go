package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// target is one phase of one host: its system ("system") or its Home
// Manager configuration ("hm").
type target struct {
	phase string
	host  string
}

func (t target) key() string {
	return t.phase + ":" + t.host
}

// attribute returns the flake attribute that builds the target.
func (d *deployer) attribute(t target) string {
	// Names are quoted because inventory names may contain dots.
	if t.phase == "hm" {
		return `.#homeConfigurations."` + d.inv.HomeOutputPrefix + t.host + `".activationPackage`
	}
	if t.host == d.self && d.darwin {
		return `.#darwinConfigurations."` + t.host + `".system`
	}
	return `.#nixosConfigurations."` + t.host + `".config.system.build.toplevel`
}

func (d *deployer) label(t target) string {
	return d.ui.phase(t.phase) + " " + d.ui.host(t.host)
}

// buildTargets builds the given targets in one Nix invocation, so Nix
// schedules and deduplicates the work across hosts, and returns the output
// path of each target that built. Failures are recorded per target.
func (d *deployer) buildTargets(targets []target, evaluated map[string]evaluation, r *report) map[string]string {
	built := map[string]string{}
	if len(targets) == 0 {
		return built
	}

	d.ui.section("build")
	if !d.makeRootDir() {
		for _, t := range targets {
			r.record(t.phase, t.host, "failed")
		}
		return built
	}
	options := d.buildOptions(d.chooseProfile(targets, evaluated, r))
	// When every target builds, these links root all outputs as the build
	// ends; Nix creates none when one fails, so each target is rooted again
	// below.
	argv := []string{"nix", "build", "--keep-going", "--out-link", filepath.Join(d.rootDir, "build")}
	argv = append(argv, options...)
	for _, t := range targets {
		argv = append(argv, evaluated[t.key()].Drv+"^out")
	}
	started := time.Now()
	// A failed derivation fails only the targets that need it; the
	// per-target lookup below tells them apart.
	// The build keeps the terminal, so Nix draws its progress bar; the run
	// record takes a failed build's logs from Nix afterwards.
	buildStatus := command{argv: argv, stdin: bytes.NewReader(nil)}.run()
	r.build += time.Since(started)

	var failed []target
	for _, t := range targets {
		drv := evaluated[t.key()].Drv
		output, status := capture(0, "nix", "path-info", drv+"^out")
		if status != 0 {
			fmt.Fprintf(os.Stderr, "%s build failed\n", d.label(t))
			r.record(t.phase, t.host, "failed")
			failed = append(failed, t)
			continue
		}
		path := strings.TrimSpace(lastLine(output))
		if !d.addRoot(t, drv, options) {
			fmt.Fprintf(os.Stderr, "%s cannot protect the build from garbage collection\n", d.label(t))
			r.record(t.phase, t.host, "failed")
			failed = append(failed, t)
			continue
		}
		built[t.key()] = path
	}
	d.recordBuild(argv, buildStatus, failed, evaluated)
	return built
}

// addRoot keeps a built path from garbage collection until the run ends.
// Activation can come minutes after the build, behind serial ready gates.
// Rooting goes through the derivation, so an output a collection removed
// after the build is built again, under the run's build profile, instead of
// being lost.
func (d *deployer) addRoot(t target, drv string, options []string) bool {
	link := filepath.Join(d.rootDir, t.phase+"-"+t.host)
	argv := append([]string{"nix", "build", "--out-link", link}, options...)
	_, status := capture(0, append(argv, drv+"^out")...)
	return status == 0
}

func (d *deployer) makeRootDir() bool {
	if d.rootDir != "" {
		return true
	}
	dir, err := os.MkdirTemp("", "nix-deploy-roots-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "deploy: cannot create a directory for build roots: %v\n", err)
		return false
	}
	d.rootDir = dir
	return true
}

// releaseRoots removes the run's garbage-collector roots.
func (d *deployer) releaseRoots() {
	if d.rootDir != "" {
		os.RemoveAll(d.rootDir)
	}
}

// evaluation is what a target evaluates to without building it: its
// derivation and the output path that derivation builds. (Inputs read during
// evaluation are still realised, and a content-addressed derivation would
// have no output path yet, so it would always plan as changed.)
type evaluation struct {
	Drv string `json:"drv"`
	Out string `json:"out"`
}

// evaluateOutputs is applied to each target's derivation. Its keys are not
// drvPath and outPath because Nix prints an attribute set with an outPath as
// that path alone.
const evaluateOutputs = "d: { drv = d.drvPath; out = d.outPath; }"

// fetchLockWait starts the line Nix prints while another process fetches the
// same input. Nix takes that lock for every evaluation, even of a flake
// already in the store, so parallel evaluations queue briefly and report each
// wait; the evaluate phase drops the line.
const fetchLockWait = "waiting for another Nix process to finish fetching input"

// evaluate returns the evaluation of every target, evaluating up to
// DEPLOY_EVAL_JOBS targets at once. Each evaluation is separate, so an error
// is attributed to its host instead of stopping the others.
func (d *deployer) evaluate(targets []target, r *report) map[string]evaluation {
	evaluated := map[string]evaluation{}
	if len(targets) == 0 {
		return evaluated
	}
	d.ui.section("evaluate")
	var lock sync.Mutex
	statuses := make([]int, len(targets))
	elapsed := make([]time.Duration, len(targets))
	slots := make(chan struct{}, d.settings.evalJobs)
	var wait sync.WaitGroup
	for i, t := range targets {
		wait.Add(1)
		go func() {
			defer wait.Done()
			slots <- struct{}{}
			defer func() { <-slots }()

			var out bytes.Buffer
			errOut := &prefixWriter{lock: &lock, dst: os.Stderr, prefix: d.label(t), skip: fetchLockWait}
			// The log keeps the raw output, without the tag.
			log := d.record.log(t.phase + "-" + t.host + "-evaluate.log")
			errStream := tee(errOut, log)
			started := time.Now()
			statuses[i] = command{
				// The evaluation cache does not keep derivation paths, and
				// parallel evaluations would only contend for it.
				argv:   []string{"nix", "eval", "--no-eval-cache", "--json", d.attribute(t), "--apply", evaluateOutputs},
				stdin:  bytes.NewReader(nil),
				stdout: tee(&out, log),
				stderr: errStream,
			}.run()
			elapsed[i] = time.Since(started)
			var e evaluation
			if statuses[i] == 0 {
				if err := json.Unmarshal(out.Bytes(), &e); err != nil || e.Drv == "" || e.Out == "" {
					fmt.Fprintf(errStream, "deploy: unexpected evaluation result: %s\n", strings.TrimSpace(out.String()))
					statuses[i] = 1
				}
			}
			errOut.flush()
			log.Close()
			if statuses[i] == 0 {
				lock.Lock()
				evaluated[t.key()] = e
				lock.Unlock()
			}
		}()
	}
	wait.Wait()
	d.record.noteEvaluatedLock()

	for i, t := range targets {
		r.addTime(t, "evaluate", elapsed[i])
		if statuses[i] != 0 {
			fmt.Fprintf(os.Stderr, "%s evaluation failed\n", d.label(t))
			r.record(t.phase, t.host, "failed")
		}
	}
	return evaluated
}

// Remote scripts print two store paths, one per line, or "-" for a missing
// one. Home Manager moves its profile before activation finishes and its
// current-home root only after every step succeeded, so the root stands for
// the running generation.
const (
	systemGenerationScript = `for p in /run/current-system /nix/var/nix/profiles/system; do readlink -e "$p" || echo -; done`
	homeGenerationScript   = `s="${XDG_STATE_HOME:-$HOME/.local/state}"; readlink -e "$s/home-manager/gcroots/current-home" || echo -; readlink -e "$s/nix/profiles/home-manager" 2>/dev/null || readlink -e "/nix/var/nix/profiles/per-user/$(id -un)/home-manager" 2>/dev/null || echo -`
)

// generation returns what the target's host runs now and its profile: for a
// system the running system and the system profile, for Home Manager the last
// fully activated generation and the profile. An empty path means unknown.
func (d *deployer) generation(t target) (running, profile string) {
	if t.host == d.self {
		if t.phase == "hm" {
			state := stateHome()
			profile = resolve(filepath.Join(state, "nix/profiles/home-manager"))
			if profile == "" {
				profile = resolve(filepath.Join("/nix/var/nix/profiles/per-user", os.Getenv("USER"), "home-manager"))
			}
			return resolve(filepath.Join(state, "home-manager/gcroots/current-home")), profile
		}
		return resolve("/run/current-system"), resolve("/nix/var/nix/profiles/system")
	}

	endpoint, script := d.inv.systemEndpoint(t.host), systemGenerationScript
	if t.phase == "hm" {
		endpoint, script = d.inv.homeEndpoint(t.host), homeGenerationScript
	}
	output, status := d.sshCheck(endpoint, posixShell(script))
	if status != 0 {
		return "", ""
	}
	var paths []string
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "-" || !strings.HasPrefix(line, "/") {
			line = ""
		}
		paths = append(paths, line)
	}
	if len(paths) != 2 {
		return "", ""
	}
	return paths[0], paths[1]
}

func resolve(path string) string {
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return ""
	}
	return target
}

// plan decides which evaluated targets need building and activation, and
// prints the decision. A target is unchanged when the host already runs the
// path it evaluates to and its profile points to it; a host staged with 'boot'
// only needs its profile. Unchanged targets are not built: their output may
// no longer be in this machine's store, and the host already has it.
func (d *deployer) plan(targets []target, evaluated map[string]evaluation, r *report) []target {
	d.ui.section("plan")
	var changed []target
	for _, t := range targets {
		e, ok := evaluated[t.key()]
		if !ok {
			continue
		}
		path := e.Out
		running, profile := d.generation(t)
		boot := t.phase == "system" && t.host != d.self && d.inv.activationMode(t.host) == "boot"
		current := running
		if boot {
			current = profile
		}
		retry := current == path && profile == path && d.failed.failed(t, path)
		switch {
		case retry:
			fmt.Printf("%s last activation of %s from this machine failed; activating again\n", d.label(t), storeName(path))
			changed = append(changed, t)
		case current == path && profile == path && !d.force:
			fmt.Printf("%s unchanged: %s\n", d.label(t), storeName(path))
			r.record(t.phase, t.host, "unchanged")
		case current == path && profile == path:
			fmt.Printf("%s unchanged, activating anyway (--force): %s\n", d.label(t), storeName(path))
			changed = append(changed, t)
		case current == "":
			fmt.Printf("%s current generation unknown; activating %s\n", d.label(t), storeName(path))
			changed = append(changed, t)
		default:
			fmt.Printf("%s changed: %s -> %s\n", d.label(t), storeName(current), storeName(path))
			changed = append(changed, t)
		}
	}
	return changed
}

// storeName drops the store directory and hash from a store path.
func storeName(path string) string {
	name := filepath.Base(path)
	if _, rest, ok := strings.Cut(name, "-"); ok && strings.HasPrefix(path, "/nix/store/") {
		return rest
	}
	return name
}
