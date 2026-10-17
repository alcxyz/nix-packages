// Package review is the shared core of revkit: it writes the reviewed tree
// from raw git objects and runs read-only, sandboxed reviewers of either
// client in a home of their own, recording their usage and the capacity
// windows they report.
package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// Reviewer is one configured model of one client.
type Reviewer struct {
	Name   string `json:"name"`
	Client string `json:"client"`
	Model  string `json:"model"`
	Effort string `json:"effort"`
}

var (
	namePattern   = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	modelPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/\[\]-]*$`)
	effortPattern = regexp.MustCompile(`^[a-z]+$`)
)

// Validate rejects names and values that do not fit the client command line.
func (r Reviewer) Validate() error {
	if !namePattern.MatchString(r.Name) {
		return fmt.Errorf("reviewer name %q must match [A-Za-z0-9_-]+", r.Name)
	}
	if r.Client != "claude" && r.Client != "codex" {
		return fmt.Errorf("reviewer %s: client %q must be claude or codex", r.Name, r.Client)
	}
	if !modelPattern.MatchString(r.Model) {
		return fmt.Errorf("reviewer %s: model %q is not a model name", r.Name, r.Model)
	}
	if !effortPattern.MatchString(r.Effort) {
		return fmt.Errorf("reviewer %s: effort %q is not an effort level", r.Name, r.Effort)
	}
	return nil
}

// passedVariables reach every reviewer when set. Everything else, notably
// forge and other tokens, is left out.
var passedVariables = []string{
	"PATH", "LANG", "LANGUAGE", "LC_ALL", "LC_CTYPE", "LC_MESSAGES", "TZ",
	"SSL_CERT_FILE", "SSL_CERT_DIR", "NIX_SSL_CERT_FILE",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY",
	"http_proxy", "https_proxy", "no_proxy", "all_proxy",
}

// clientVariables are the sign-in variables of each client, passed only to
// that client.
var clientVariables = map[string][]string{
	"claude": {"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"},
	"codex":  {"OPENAI_API_KEY", "CODEX_API_KEY"},
}

// Environment returns the reviewer's environment: the allowed variables of
// environ and extra (names a consumer allows). Codex gets its home's
// variables, and PATH entries resolved through symbolic links, since its
// sandbox cannot read the user's home where profile directories often
// live. Claude keeps the user's HOME and configuration directory.
func Environment(client string, environ, extra []string, home *Home) []string {
	allowed := map[string]bool{}
	for _, name := range passedVariables {
		allowed[name] = true
	}
	for _, name := range clientVariables[client] {
		allowed[name] = true
	}
	for _, name := range extra {
		allowed[name] = true
	}
	if client == "claude" {
		allowed["HOME"], allowed["CLAUDE_CONFIG_DIR"] = true, true
	}
	var result []string
	for _, entry := range environ {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || !allowed[name] {
			continue
		}
		if name == "PATH" {
			entry = "PATH=" + reviewerPath(value, client == "codex")
		}
		result = append(result, entry)
	}
	if home != nil {
		result = append(result, home.Environment()...)
	}
	return result
}

// defaultPath is the reviewer's PATH when the caller's has no absolute
// entry.
const defaultPath = "/usr/local/bin:/usr/bin:/bin"

// reviewerPath keeps the absolute entries of PATH: an empty or relative
// entry would look commands up in the working directory, which is the
// reviewed tree. With resolve, each entry is resolved through symbolic
// links, keeping those that cannot be resolved as they are.
func reviewerPath(value string, resolve bool) string {
	var entries []string
	for _, entry := range filepath.SplitList(value) {
		if !filepath.IsAbs(entry) {
			continue
		}
		if resolve {
			if resolved, err := filepath.EvalSymlinks(entry); err == nil {
				entry = resolved
			}
		}
		entries = append(entries, entry)
	}
	if len(entries) == 0 {
		return defaultPath
	}
	return strings.Join(entries, string(filepath.ListSeparator))
}

// Job is one reviewer run on one prompt.
type Job struct {
	Reviewer Reviewer
	Prompt   string
	// Tree is the reviewed checkout and the reviewer's working directory.
	Tree string
	// Dir receives the prompt, the client's output and log, and the
	// reviewer's temporary home; Name prefixes their file names.
	Dir     string
	Name    string
	Timeout time.Duration
	// Environ is the caller's environment; Extra names further variables
	// the reviewer may see; HomeLinks names entries of the user's home to
	// link into a Codex reviewer's (see NewHome); Readable names paths a
	// Codex reviewer may read besides the tree and the platform minimum.
	Environ   []string
	Extra     []string
	HomeLinks []string
	Readable  []string
}

// Result is what a reviewer run produced. Only StatusOK is a review: a
// failed, timed-out or interrupted reviewer is a missing review, never one
// without findings.
type Result struct {
	Status        string `json:"status"`
	Problem       string `json:"problem,omitempty"`
	Exit          *int   `json:"exit"`
	Seconds       int    `json:"seconds"`
	ReportedModel string `json:"reportedModel,omitempty"`
	// Reads counts the reviewer's tool calls that succeeded: Claude's file
	// tools, Codex's commands that exited 0. A reviewer whose tools were all
	// blocked may still reply as if it found nothing; one successful call
	// does not prove it read what it needed.
	Reads   int       `json:"reads"`
	Usage   Usage     `json:"usage"`
	Windows []Window  `json:"windows,omitempty"`
	Notes   []string  `json:"notes,omitempty"`
	Started time.Time `json:"started"`
	// Text is the reviewer's final reply; Output and Log are its files.
	Text   string `json:"-"`
	Output string `json:"output"`
	Log    string `json:"log"`
}

const (
	StatusOK          = "ok"
	StatusFailed      = "failed"
	StatusTimeout     = "timeout"
	StatusInterrupted = "interrupted"
)

// Command returns the read-only reviewer command line. Neither client may
// write, search the web or use the network, MCP servers, plugins or app
// connectors. Claude's
// restricted mode keeps the file tools inside the working directory and
// ignores user and project settings, so hooks in the tree do not run; safe
// mode also turns off skills, plugins and agents. Codex runs under a
// permission profile that denies the file system except the platform
// minimum, the tree and readable, keeps its login in a file, and keeps a
// session log in its temporary home, which is where it reports its usage
// windows. Its commands do not inherit its sign-in variables.
func Command(r Reviewer, tree, output string, readable []string) []string {
	if r.Client == "codex" {
		filesystem := []string{`":root"="deny"`, `":minimal"="read"`}
		for _, path := range readable {
			filesystem = append(filesystem, fmt.Sprintf("%q=\"read\"", path))
		}
		filesystem = append(filesystem, `":tmpdir"="deny"`, `":slash_tmp"="deny"`, `":workspace_roots"="read"`)
		// Codex signs in with these, but the commands it runs for the
		// reviewer must not see them, or a prompt in the tree could have
		// one printed into a finding.
		hidden := make([]string, len(clientVariables["codex"]))
		for i, name := range clientVariables["codex"] {
			hidden[i] = fmt.Sprintf("%q", name)
		}
		return []string{
			"codex", "exec", "-m", r.Model, "-c", fmt.Sprintf("model_reasoning_effort=%q", r.Effort),
			"-c", `cli_auth_credentials_store="file"`, "-c", `mcp_oauth_credentials_store="file"`,
			"-c", `default_permissions="review"`, "-c", `permissions.review.extends=":read-only"`,
			"-c", "permissions.review.filesystem={" + strings.Join(filesystem, ", ") + "}",
			"-c", "allow_login_shell=false", "-c", `web_search="disabled"`,
			"-c", "shell_environment_policy.exclude=[" + strings.Join(hidden, ", ") + "]",
			"-C", tree, "--skip-git-repo-check", "--ignore-user-config", "--ignore-rules",
			"--disable", "apps", "--disable", "plugins", "--disable", "remote_plugin",
			"--json", "--color", "never", "-o", output, "-",
		}
	}
	return []string{
		"claude", "-p", "--model", r.Model, "--effort", r.Effort, "--restricted", "--safe-mode",
		"--tools", "Read,Grep,Glob", "--strict-mcp-config", "--no-session-persistence",
		"--permission-mode", "dontAsk", "--output-format", "stream-json", "--verbose",
	}
}

// ValidReadablePath reports whether path can be named in Codex's
// permission profile: absolute, clean, and free of quoting characters.
func ValidReadablePath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && path != "/" &&
		!strings.ContainsAny(path, "\"\\\n\r\t") && !strings.HasPrefix(path, ":")
}

// Run runs job and returns its result; errors that stop the run before the
// reviewer starts are reported as a failed result too.
func Run(ctx context.Context, job Job) (result Result) {
	started := time.Now()
	base := filepath.Join(job.Dir, job.Name)
	result = Result{Status: StatusFailed, Started: started, Output: base + ".md", Log: base + ".log"}
	defer func() { result.Seconds = int(time.Since(started).Round(time.Second) / time.Second) }()

	fail := func(err error) Result {
		result.Problem = err.Error()
		return result
	}
	if err := os.WriteFile(base+".prompt", []byte(job.Prompt), 0o644); err != nil {
		return fail(err)
	}
	os.Remove(result.Output)
	var home *Home
	if job.Reviewer.Client == "codex" {
		var err error
		if home, err = NewHome(base+".home", job.Environ, job.HomeLinks); err != nil {
			return fail(err)
		}
		defer func() {
			if note := home.CheckLogin(); note != "" {
				result.Notes = append(result.Notes, note)
			}
			if err := home.Remove(); err != nil {
				result.Notes = append(result.Notes, "cannot remove the reviewer's home: "+err.Error())
			}
		}()
	}

	stream := base + ".jsonl"
	codexReply := base + ".reply"
	argv := Command(job.Reviewer, job.Tree, codexReply, job.Readable)
	exit, status, err := execute(ctx, argv, job, stream, result.Log, Environment(job.Reviewer.Client, job.Environ, job.Extra, home))
	result.Exit = exit
	if err != nil {
		return fail(err)
	}

	var text string
	var problem string
	if job.Reviewer.Client == "claude" {
		text, problem = readClaude(stream, &result)
	} else {
		text, problem = readCodex(stream, codexReply, home.ConfigDir(), &result)
	}
	if strings.TrimSpace(text) != "" {
		if !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		if err := os.WriteFile(result.Output, []byte(text), 0o644); err != nil {
			return fail(err)
		}
	}
	result.Text = text
	switch {
	case status != StatusOK:
		result.Status = status
		result.Problem = status
	case exit == nil || *exit != 0:
		result.Problem = fmt.Sprintf("exit status %d", deref(exit))
	case problem != "":
		result.Problem = problem
	case strings.TrimSpace(text) == "":
		result.Problem = "no reply"
	default:
		result.Status = StatusOK
	}
	return result
}

// execute runs argv in its own process group with job's prompt on standard
// input, and kills the whole group at the deadline, on interruption, and
// when the client exits, so no helper it started outlives it.
func execute(ctx context.Context, argv []string, job Job, stdoutPath, logPath string, environ []string) (*int, string, error) {
	stdin, err := os.Open(filepath.Join(job.Dir, job.Name+".prompt"))
	if err != nil {
		return nil, StatusFailed, err
	}
	defer stdin.Close()
	stdout, err := os.Create(stdoutPath)
	if err != nil {
		return nil, StatusFailed, err
	}
	defer stdout.Close()
	logFile, err := os.Create(logPath)
	if err != nil {
		return nil, StatusFailed, err
	}
	defer logFile.Close()

	program, err := lookPath(argv[0], environ)
	if err != nil {
		return nil, StatusFailed, fmt.Errorf("cannot start %s: %w", argv[0], err)
	}
	runCtx, cancel := context.WithTimeout(ctx, job.Timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, program, argv[1:]...)
	cmd.Dir = job.Tree
	cmd.Env = environ
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killGroup(cmd.Process.Pid) }
	cmd.WaitDelay = 10 * time.Second
	if err := cmd.Start(); err != nil {
		return nil, StatusFailed, fmt.Errorf("cannot start %s: %w", argv[0], err)
	}
	waitErr := cmd.Wait()
	killGroup(cmd.Process.Pid)

	var exit *int
	if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
		code := cmd.ProcessState.ExitCode()
		exit = &code
	}
	switch {
	case ctx.Err() != nil:
		return exit, StatusInterrupted, nil
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		return exit, StatusTimeout, nil
	}
	var exitErr *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exitErr) {
		fmt.Fprintf(logFile, "%s: %v\n", argv[0], waitErr)
	}
	return exit, StatusOK, nil
}

// lookPath finds name in the reviewer's PATH, which is the one it runs with.
func lookPath(name string, environ []string) (string, error) {
	for _, dir := range filepath.SplitList(lookup(environ, "PATH")) {
		if dir == "" || !filepath.IsAbs(dir) {
			continue
		}
		path := filepath.Join(dir, name)
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return path, nil
		}
	}
	return "", fmt.Errorf("%s is not in PATH", name)
}

func killGroup(pid int) error {
	err := syscall.Kill(-pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func deref(value *int) int {
	if value == nil {
		return -1
	}
	return *value
}

// readClaude reads Claude's stream: the final result event and the last
// rate-limit event.
func readClaude(path string, result *Result) (string, string) {
	content, err := os.ReadFile(path)
	if err != nil {
		return "", "cannot read the reviewer's output"
	}
	var text string
	found, isError := false, false
	for _, line := range strings.Split(string(content), "\n") {
		var event map[string]any
		if json.Unmarshal([]byte(line), &event) != nil {
			continue // an interrupted final event is not a zero-token run
		}
		switch event["type"] {
		case "result":
			found = true
			text, _ = event["result"].(string)
			isError, _ = event["is_error"].(bool)
			result.Usage = claudeUsage(event["usage"])
			if models, ok := event["modelUsage"].(map[string]any); ok {
				// Claude Code also uses a helper model, and token counts
				// cannot tell which one reviewed, so record every model.
				result.ReportedModel = strings.Join(sortedKeys(models), ", ")
			}
		case "rate_limit_event":
			if windows := claudeWindows(event["rate_limit_info"]); windows != nil {
				result.Windows = windows
			}
		case "user":
			message, _ := event["message"].(map[string]any)
			content, _ := message["content"].([]any)
			for _, item := range content {
				fields, _ := item.(map[string]any)
				if failed, _ := fields["is_error"].(bool); fields["type"] == "tool_result" && !failed {
					result.Reads++
				}
			}
		}
	}
	switch {
	case !found:
		return "", "no result event"
	case isError:
		return text, "the client reported an error"
	}
	return text, ""
}

// readCodex reads Codex's final reply from its output file, its usage from
// the last completed turn, and its model and windows from the session log
// in its home.
func readCodex(stream, reply, configDir string, result *Result) (string, string) {
	if content, err := os.ReadFile(stream); err == nil {
		for _, line := range strings.Split(string(content), "\n") {
			var event map[string]any
			if json.Unmarshal([]byte(line), &event) != nil {
				continue
			}
			if event["type"] == "turn.completed" {
				result.Usage = codexUsage(event["usage"])
			}
			if item, _ := event["item"].(map[string]any); event["type"] == "item.completed" &&
				item["type"] == "command_execution" && item["exit_code"] == float64(0) {
				result.Reads++
			}
		}
	}
	model, windows := codexSession(filepath.Join(configDir, "sessions"))
	result.ReportedModel, result.Windows = model, windows
	text, err := os.ReadFile(reply)
	if err != nil {
		return "", "no reply file"
	}
	return string(text), ""
}

// codexSession returns the model and the last usage windows recorded in the
// session logs under dir.
func codexSession(dir string) (string, []Window) {
	var model string
	var windows []Window
	filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		for _, line := range strings.Split(string(content), "\n") {
			var record struct {
				Type    string         `json:"type"`
				Payload map[string]any `json:"payload"`
			}
			if json.Unmarshal([]byte(line), &record) != nil || record.Payload == nil {
				continue
			}
			if record.Type == "turn_context" {
				if name, ok := record.Payload["model"].(string); ok && name != "" {
					model = name
				}
			}
			if record.Type == "event_msg" && record.Payload["type"] == "token_count" {
				if found := codexWindows(record.Payload["rate_limits"]); found != nil {
					windows = found
				}
			}
		}
		return nil
	})
	return model, windows
}
