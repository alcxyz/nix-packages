package main

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// A host other than the inventory's triageHost hands its runs to that host
// (ADR-0016): it checks SSH both ways, copies the run record there with
// nixfleet triage --receive, and runs nixfleet triage --from-host there on
// the copy, over ssh -t, so the triage host's own nixfleet, inventory,
// agent command and checkouts do the triage.

// handoffFile is the file a hand-off adds to the copied run record.
const handoffFile = "handoff.json"

// handoffDir is where the triage host keeps copied records, under the
// nix-deploy state directory: <handoffDir>/<source host>/<run id>.
const handoffDir = "handoffs"

const (
	// handoffCheckTimeout bounds each SSH check, the reach-back included.
	handoffCheckTimeout = 30 * time.Second
	// handoffCopyTimeout bounds copying the run record.
	handoffCopyTimeout = 10 * time.Minute
	// maxHandoffBytes bounds what a received record may unpack to.
	maxHandoffBytes = 1 << 30
)

// handoffInfo is handoff.json: what the triage host needs to know about
// where the run happened.
type handoffInfo struct {
	// SourceHost is the inventory name of the host that recorded the run,
	// and SourceSSH the SSH destination the triage host reaches it by.
	SourceHost string `json:"sourceHost"`
	SourceSSH  string `json:"sourceSsh"`
	// SourceRecord is the run directory on the source host.
	SourceRecord string `json:"sourceRecord"`
	RunID        string `json:"runId"`
	// FlakeDirInHome is the run's flake directory relative to the source
	// host's home directory; the triage host uses the checkout at the same
	// place in its own home directory.
	FlakeDirInHome string `json:"flakeDirInHome"`
	// Reachable tells whether the triage host reached the source host over
	// SSH when the run was handed over, and ReachError why not.
	Reachable  bool   `json:"reachable"`
	ReachError string `json:"reachError,omitempty"`
}

// triageHandoff hands runs from this host to the triage host.
type triageHandoff struct {
	self, host       string // inventory names of this host and the triage host
	selfSSH, hostSSH string // their SSH destinations
	sshArgs          []string
	connectTimeout   string
	hint             string
	stderr           io.Writer
	// run runs a command with the input given (none when nil) and returns
	// its output, error output and status; terminal runs one on this
	// terminal and returns its status. Tests replace both.
	run      func(timeout time.Duration, stdin io.Reader, argv ...string) (string, string, int)
	terminal func(argv []string) int
}

// triageHostname is this host's short name; tests replace it.
var triageHostname = func() (string, bool) {
	output, status := capture(0, "hostname", "-s")
	name := strings.TrimSpace(output)
	return name, status == 0 && name != ""
}

var digitsOnly = regexp.MustCompile(`^[0-9]+$`)

// newTriageHandoff returns the hand-off for this host, or nil when triage
// runs here: no triageHost is set, or this host is the triage host. Like
// nixfleet status, it names this host by `hostname -s` through the
// inventory's aliases and reaches hosts by their sshHosts destination, as
// the operator's own user rather than a system user.
func newTriageHandoff(cfg *triageConfig, configFile string) (*triageHandoff, error) {
	if cfg == nil || cfg.TriageHost == "" {
		return nil, nil
	}
	inv, err := loadInventory(configFile)
	if err != nil {
		return nil, fmt.Errorf("the triage host %s cannot be resolved: %v", cfg.TriageHost, err)
	}
	name, ok := triageHostname()
	if !ok {
		return nil, errors.New("cannot determine this host's name to compare it with the triage host")
	}
	self, host := inv.canonical(name), inv.canonical(cfg.TriageHost)
	if self == host {
		return nil, nil
	}
	if !contains(inv.KnownHosts, host) {
		return nil, fmt.Errorf("the triage host %s is not a known host of the inventory", cfg.TriageHost)
	}
	if !safeName.MatchString(self) {
		return nil, fmt.Errorf("this host's name '%s' cannot be passed to the triage host", self)
	}
	sshOptions := os.Getenv("NIX_SSHOPTS")
	if sshOptions == "" {
		sshOptions = defaultSSHOptions
	}
	timeout := envOr("DEPLOY_SSH_CHECK_TIMEOUT", "5")
	if !digitsOnly.MatchString(timeout) {
		timeout = "5"
	}
	return &triageHandoff{
		self: self, host: host, selfSSH: inv.sshHost(self), hostSSH: inv.sshHost(host),
		sshArgs: strings.Fields(sshOptions), connectTimeout: timeout, hint: cfg.SSHHint,
		stderr: os.Stderr, run: runWithInput,
		terminal: func(argv []string) int { return runOnTerminal(argv, "", os.Environ(), "ssh") },
	}, nil
}

// handOverRun hands the run in dir to the triage host when this host is not
// it. It returns the remote triage's status and true once the run was
// handed over, or false when triage goes on here; launch false only runs
// the checks.
func handOverRun(cfg *triageConfig, opts triageOptions, dir string, launch bool) (int, bool) {
	h, err := newTriageHandoff(cfg, opts.configFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "nixfleet triage: %v; running triage here\n", err)
		return 0, false
	}
	if h == nil {
		return 0, false
	}
	reachable, why, ok := h.check()
	if !ok || !launch {
		return 0, false
	}
	info, err := readRunInfo(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "nixfleet triage: cannot read the run's run.json: %v; running triage here\n", err)
		return 0, false
	}
	return h.handOver(dir, info, reachable, why, opts.warnings)
}

func runWithInput(timeout time.Duration, stdin io.Reader, argv ...string) (string, string, int) {
	var stdout, stderr strings.Builder
	var input io.Reader = strings.NewReader("")
	if stdin != nil {
		input = stdin
	}
	status := command{argv: argv, stdin: input, stdout: &stdout, stderr: &stderr, timeout: timeout}.run()
	return stdout.String(), stderr.String(), status
}

// sshArgv runs remote on dest without a password prompt, ConnectTimeout
// first so it wins over the one in the default SSH options.
func (h *triageHandoff) sshArgv(dest, remote string, terminal bool) []string {
	argv := []string{"ssh"}
	if terminal {
		argv = append(argv, "-t")
	}
	argv = append(argv, "-o", "BatchMode=yes", "-o", "ConnectTimeout="+h.connectTimeout, "-o", "ConnectionAttempts=1")
	argv = append(argv, h.sshArgs...)
	return append(argv, dest, remote)
}

func (h *triageHandoff) say(format string, args ...any) {
	fmt.Fprintf(h.stderr, "nixfleet triage: "+format+"\n", args...)
}

func elapsedSeconds(started time.Time) string {
	return fmt.Sprintf("%.1fs", time.Since(started).Seconds())
}

// failure is the first line of a command's error output, or its status.
func failure(stderr string, status int) string {
	for _, line := range strings.Split(stderr, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	if status == exitTimedOut {
		return "no answer in time"
	}
	return fmt.Sprintf("ssh exited with status %d", status)
}

// check runs the two SSH checks, each reported in one line with its
// duration: this host reaches the triage host, and the triage host reaches
// back. ok is false when the triage host is unreachable; reachable and why
// tell about the reach-back, and the hint is shown when it failed.
func (h *triageHandoff) check() (reachable bool, why string, ok bool) {
	started := time.Now()
	_, stderr, status := h.run(handoffCheckTimeout, nil, h.sshArgv(h.hostSSH, "true", false)...)
	if status != 0 {
		h.say("the triage host %s is not reachable over SSH as %s (%s): %s; running triage here", h.host, h.hostSSH, elapsedSeconds(started), failure(stderr, status))
		return false, "", false
	}
	h.say("the triage host %s is reachable over SSH as %s (%s)", h.host, h.hostSSH, elapsedSeconds(started))
	started = time.Now()
	back := strings.Join([]string{"ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=" + h.connectTimeout, "-o", "ConnectionAttempts=1", h.selfSSH, "true"}, " ")
	_, stderr, status = h.run(handoffCheckTimeout, nil, h.sshArgv(h.hostSSH, back, false)...)
	if status == 0 {
		h.say("%s reaches this host over SSH as %s (%s)", h.host, h.selfSSH, elapsedSeconds(started))
		return true, "", true
	}
	why = failure(stderr, status)
	h.say("%s cannot reach this host over SSH as %s (%s): %s", h.host, h.selfSSH, elapsedSeconds(started), why)
	if h.hint != "" {
		h.say("to let %s reach this host: %s", h.host, h.hint)
	}
	return false, why, true
}

// handOver copies the run record in dir to the triage host and runs triage
// there on the copy. It returns the remote triage's status and true, or
// false when triage should run here; it has then said why.
func (h *triageHandoff) handOver(dir string, info runInfo, reachable bool, why string, warnings bool) (int, bool) {
	if !runIDPattern.MatchString(info.ID) {
		h.say("the run's ID '%s' is not a run ID; running triage here", info.ID)
		return 0, false
	}
	home, _ := os.UserHomeDir()
	inHome, err := filepath.Rel(home, info.FlakeDir)
	if home == "" || info.FlakeDir == "" || err != nil || !filepath.IsLocal(inHome) {
		h.say("the run's flake directory %s is not inside the home directory, so %s has no checkout at the same place; running triage here", info.FlakeDir, h.host)
		return 0, false
	}
	if !reachable {
		h.say("handing the run over anyway; the agent is told this host could not be reached")
	}
	handoff := handoffInfo{
		SourceHost: h.self, SourceSSH: h.selfSSH, SourceRecord: dir, RunID: info.ID,
		FlakeDirInHome: filepath.ToSlash(inHome), Reachable: reachable, ReachError: why,
	}
	// The archive walks the resolved directory: WalkDir does not follow a
	// record directory given as a symbolic link.
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		h.say("cannot resolve the run record %s: %v; running triage here", dir, err)
		return 0, false
	}
	started := time.Now()
	reader, writer := io.Pipe()
	go func() { writer.CloseWithError(writeHandoffArchive(writer, root, handoff)) }()
	stdout, stderr, status := h.run(handoffCopyTimeout, reader, h.sshArgv(h.hostSSH, "nixfleet triage --receive "+h.self+" "+info.ID, false)...)
	// Unblocks the archive writer when ssh stopped reading early.
	reader.Close()
	// The path is the last line, in case the remote shell prints more.
	copied := strings.TrimSpace(lastLine(stdout))
	if status == 0 && (!filepath.IsAbs(copied) || strings.ContainsAny(copied, "\n\r")) {
		status, stderr = 1, "it printed no copy path"
	}
	if status != 0 {
		h.say("cannot copy the run record to %s (%s): %s; running triage here", h.host, elapsedSeconds(started), failure(stderr, status))
		return 0, false
	}
	h.say("copied the run record to %s:%s (%s); triage runs there", h.host, copied, elapsedSeconds(started))
	remote := "nixfleet triage --from-host " + h.self
	if warnings {
		remote += " --warnings"
	}
	return h.terminal(h.sshArgv(h.hostSSH, remote+" "+shellQuote(copied), true)), true
}

// writeHandoffArchive writes the run record in dir as a tar stream, with
// handoff.json added and run.lock left out. Only directories and regular
// files are copied.
func writeHandoffArchive(w io.Writer, dir string, handoff handoffInfo) error {
	archive := tar.NewWriter(w)
	data, err := json.MarshalIndent(handoff, "", "  ")
	if err != nil {
		return err
	}
	if err := archive.WriteHeader(&tar.Header{Name: handoffFile, Mode: 0o600, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	if _, err := archive.Write(data); err != nil {
		return err
	}
	err = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil || rel == "." || rel == runLockName || rel == handoffFile {
			return err
		}
		name := filepath.ToSlash(rel)
		if entry.IsDir() {
			return archive.WriteHeader(&tar.Header{Name: name + "/", Mode: 0o700, Typeflag: tar.TypeDir})
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		stat, err := file.Stat()
		if err != nil {
			return err
		}
		if err := archive.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: stat.Size(), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err = io.CopyN(archive, file, stat.Size())
		return err
	})
	if err != nil {
		return err
	}
	return archive.Close()
}

// readHandoffArchive unpacks a tar stream of directories and regular files
// into dir, refusing names that leave it and more than maxHandoffBytes.
func readHandoffArchive(r io.Reader, dir string) error {
	archive := tar.NewReader(r)
	var total int64
	for {
		header, err := archive.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.FromSlash(strings.TrimSuffix(header.Name, "/"))
		if !filepath.IsLocal(name) {
			return fmt.Errorf("the archive names '%s', outside the record", header.Name)
		}
		path := filepath.Join(dir, name)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if total += header.Size; total > maxHandoffBytes {
				return fmt.Errorf("the record is larger than %d bytes", maxHandoffBytes)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return err
			}
			file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return err
			}
			_, err = io.Copy(file, archive)
			if closeErr := file.Close(); err == nil {
				err = closeErr
			}
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("the archive holds '%s', which is neither a file nor a directory", header.Name)
		}
	}
}

func readHandoffInfo(dir string) (handoffInfo, error) {
	var handoff handoffInfo
	data, err := os.ReadFile(filepath.Join(dir, handoffFile))
	if err == nil {
		err = json.Unmarshal(data, &handoff)
	}
	if err != nil {
		return handoff, fmt.Errorf("%s is not a handed-over run record: %v", dir, err)
	}
	if !filepath.IsLocal(filepath.FromSlash(handoff.FlakeDirInHome)) {
		return handoff, fmt.Errorf("%s names the flake directory '%s', which is not inside the home directory", filepath.Join(dir, handoffFile), handoff.FlakeDirInHome)
	}
	return handoff, nil
}

// handoffCheckout is the checkout this host uses for a handed-over run: the
// same place relative to the home directory as the run's flake directory
// on the source host. It must exist.
func handoffCheckout(handoff handoffInfo) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", errors.New("cannot determine the home directory")
	}
	path := filepath.Join(home, filepath.FromSlash(handoff.FlakeDirInHome))
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		return "", fmt.Errorf("no checkout at %s, the place in this host's home directory of the run's flake directory on %s (~/%s); clone the flake there", path, handoff.SourceHost, handoff.FlakeDirInHome)
	}
	return path, nil
}

// receiveHandoff is nixfleet triage --receive: it unpacks a run record from
// in into <state>/nix-deploy/handoffs/<host>/<id>, replacing an earlier
// copy, checks that this host can triage it, rewrites the record's path in
// context.md and inputs.md to the copy, adds the hand-off section to
// context.md and returns the copy's path. Copies beyond the newest
// DEPLOY_RUN_KEEP of that host are removed, except the one received. An
// empty configFile means no inventory was given.
func receiveHandoff(host, id string, in io.Reader, configFile string, cfg *triageConfig) (string, error) {
	if !safeName.MatchString(host) {
		return "", fmt.Errorf("'%s' is not a host name", host)
	}
	if !runIDPattern.MatchString(id) {
		return "", fmt.Errorf("'%s' is not a run ID", id)
	}
	base := filepath.Join(stateHome(), "nix-deploy", handoffDir, host)
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", err
	}
	temp, err := os.MkdirTemp(base, ".receive-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(temp)
	if err := readHandoffArchive(in, temp); err != nil {
		return "", fmt.Errorf("cannot unpack the run record: %v", err)
	}
	handoff, err := readHandoffInfo(temp)
	if err != nil {
		return "", err
	}
	if handoff.SourceHost != host || handoff.RunID != id {
		return "", fmt.Errorf("the record is run %s from %s, not run %s from %s", handoff.RunID, handoff.SourceHost, id, host)
	}
	if _, err := os.Stat(filepath.Join(temp, "context.md")); err != nil {
		return "", errors.New("the copied run record has no context.md")
	}
	if configFile == "" {
		return "", errors.New("no inventory is configured here; give nixfleet NIX_DEPLOY_CONFIG in non-interactive SSH sessions, for example through a wrapper that sets it")
	}
	if cfg == nil || len(cfg.AgentCommand) == 0 {
		return "", errors.New("no agentCommand is set in the triage object of this host's inventory")
	}
	checkout, err := handoffCheckout(handoff)
	if err != nil {
		return "", err
	}
	dest := filepath.Join(base, id)
	if err := os.RemoveAll(dest); err != nil {
		return "", err
	}
	if err := os.Rename(temp, dest); err != nil {
		return "", err
	}
	if err := rewriteHandoffContext(dest, handoff, checkout, cfg.InputCheckouts); err != nil {
		return "", err
	}
	keep, err := positiveSetting("DEPLOY_RUN_KEEP", "30")
	if err != nil {
		keep = 30
	}
	pruneHandoffs(base, id, keep)
	return dest, nil
}

// rewriteHandoffContext points the paths of the copied record's context.md
// and inputs.md at the copy, and adds to context.md the hand-off section
// after its title and this host's own input checkouts at its end.
func rewriteHandoffContext(dest string, handoff handoffInfo, checkout string, checkouts map[string]string) error {
	for _, name := range []string{"context.md", "inputs.md"} {
		path := filepath.Join(dest, name)
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		text := string(data)
		if handoff.SourceRecord != "" {
			text = strings.ReplaceAll(text, handoff.SourceRecord, dest)
		}
		if name == "context.md" {
			info, _ := readRunInfo(dest)
			lock, _ := os.ReadFile(filepath.Join(dest, "flake.lock"))
			section := handoffSection(dest, handoff, info, checkout)
			if title, rest, found := strings.Cut(text, "\n\n"); found && strings.HasPrefix(title, "# ") {
				text = title + "\n\n" + section + "\n" + rest
			} else {
				text = section + "\n" + text
			}
			var own strings.Builder
			writeOwnInputs(&own, "Own inputs on this host", lock, checkouts)
			text = strings.TrimRight(text, "\n") + "\n" + own.String()
		}
		if err := replaceFile(path, []byte(text)); err != nil {
			return err
		}
	}
	return nil
}

// handoffSection is the hand-off section of a copied record's context.md.
func handoffSection(dest string, handoff handoffInfo, info runInfo, checkout string) string {
	var out strings.Builder
	host := handoff.SourceHost
	fmt.Fprintf(&out, "## Hand-off from %s\n\n", host)
	fmt.Fprintf(&out, "The run was recorded on %s and handed to this host for triage.\n\n", host)
	if handoff.Reachable {
		fmt.Fprintf(&out, "- %s is reachable from here with `ssh %s`; this host reached it so when triage started.\n", host, handoff.SourceSSH)
	} else {
		fmt.Fprintf(&out, "- %s could not be reached from here with `ssh %s` when triage started: %s. Check again before relying on it, and while it stays unreachable give the operator the exact commands to run there.\n", host, handoff.SourceSSH, handoff.ReachError)
	}
	fmt.Fprintf(&out, "- The run record was copied from %s:%s to %s, and the paths under it in this file and in inputs.md now name the copy.\n", host, handoff.SourceRecord, dest)
	fmt.Fprintf(&out, "- Everything else this file names is on %s: store paths (derivations, logs Nix kept, whether an input's store path is in the local store), the flake directory %s, the earlier runs, and the checkouts and Git state under \"Own inputs\". Read them there over SSH.\n", host, info.FlakeDir)
	head := "unknown"
	if value, ok := readGit(checkout, "rev-parse", "--verify", "-q", "HEAD"); ok {
		head = strings.TrimSpace(string(value))
	}
	revision := info.GitRev
	if revision == "" {
		revision = "none recorded"
	}
	fmt.Fprintf(&out, "- The flake checkout on this host is %s, at the place of the run's flake directory relative to the home directory. Its HEAD is %s; the run's revision is %s. The agent runs there.\n", checkout, head, revision)
	out.WriteString("- \"Own inputs on this host\" at the end lists this host's checkouts of the own inputs, where fixes are made.\n")
	return out.String()
}

// handoffPrompt is what the prompt adds for a handed-over run.
func handoffPrompt(handoff handoffInfo) string {
	prompt := "The run was recorded on " + handoff.SourceHost + " and handed to this host: only its record was copied here. Store paths, the flake directory it ran from and every other file outside the copied record are on " + handoff.SourceHost + ", reachable with `ssh " + handoff.SourceSSH + "`, as the Hand-off section of the context and the playbook's section on runs from another host say."
	if !handoff.Reachable {
		prompt += " When triage started, " + handoff.SourceHost + " could not be reached over SSH from here (" + handoff.ReachError + "); tell the operator, and give them the commands to run there while it stays unreachable."
	}
	return prompt
}

// pruneHandoffs removes copied records in base beyond the newest keep,
// except the one just received.
func pruneHandoffs(base, current string, keep int) {
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	var ids []string
	for _, entry := range entries {
		if entry.IsDir() && runIDPattern.MatchString(entry.Name()) {
			ids = append(ids, entry.Name())
		}
	}
	sort.Strings(ids)
	for i := 0; i < len(ids)-keep; i++ {
		if ids[i] != current {
			os.RemoveAll(filepath.Join(base, ids[i]))
		}
	}
}

// triageFromHost is nixfleet triage --from-host: it launches this host's
// agent on a run record another host handed over, in this host's checkout
// of the flake. The context was written on the source host.
func triageFromHost(opts triageOptions, cfg *triageConfig, unreadable bool) int {
	if opts.run == "" {
		fmt.Fprintln(os.Stderr, "nixfleet triage: --from-host needs the copied run record")
		return 2
	}
	dir, err := filepath.Abs(opts.run)
	if err != nil {
		fmt.Fprintf(os.Stderr, "nixfleet triage: %v\n", err)
		return 1
	}
	handoff, err := readHandoffInfo(dir)
	if err == nil && handoff.SourceHost != opts.fromHost {
		err = fmt.Errorf("%s was handed over from %s, not %s", dir, handoff.SourceHost, opts.fromHost)
	}
	context := filepath.Join(dir, "context.md")
	if err == nil && !fileExists(context) {
		err = fmt.Errorf("%s has no context.md", dir)
	}
	checkout := ""
	if err == nil {
		checkout, err = handoffCheckout(handoff)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "nixfleet triage: %v\n", err)
		return 1
	}
	fmt.Println(context)
	launch, reason := launchDecision(cfg, opts.noLaunch, isTerminal(os.Stdin), isTerminal(os.Stdout))
	if unreadable {
		launch, reason = false, "the inventory could not be read"
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
	return launchTriage(cfg, context, checkout, playbook, opts.warnings, handoffPrompt(handoff))
}
