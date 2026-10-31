package review

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// hangGuard bounds a wait that only stops a hung test. CI can pause a whole
// job for minutes, so a hang guard must outlast such a pause rather than
// measure speed.
const hangGuard = 15 * time.Minute

// The fake clients record what they see in $REVKIT_TEST_LOG and reply the
// way the real ones do; $REVKIT_TEST_MODE selects a failure.
const fakeClaude = `#!/bin/sh
cat > "$REVKIT_TEST_LOG.prompt"
{
  echo "HOME=$HOME"
  echo "CLAUDE_CONFIG_DIR=${CLAUDE_CONFIG_DIR-unset}"
  echo "GITHUB_TOKEN=${GITHUB_TOKEN-unset}"
  echo "OPENAI_API_KEY=${OPENAI_API_KEY-unset}"
  echo "ANTHROPIC_API_KEY=${ANTHROPIC_API_KEY-unset}"
  echo "PWD=$(pwd)"
  echo "ARGS=$*"
} > "$REVKIT_TEST_LOG"
case "$REVKIT_TEST_MODE" in
fail) echo "something broke" >&2; exit 3 ;;
hang) sleep 300 & echo $! > "$REVKIT_TEST_LOG.child"; wait; exit 0 ;;
error)
  echo '{"type":"result","result":"Not logged in","is_error":true}'
  exit 1 ;;
esac
echo '{"type":"system"}'
echo '{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"1","content":"x"},{"type":"tool_result","tool_use_id":"2","is_error":true,"content":"denied"}]}}'
echo '{"type":"rate_limit_event","rate_limit_info":{"unifiedWindows":{"five_hour":{"utilization":0.18,"resetsAt":1792202400},"seven_day":{"utilization":0.1,"resetsAt":1792753200}}}}'
echo '{"type":"result","result":"reply text","is_error":false,"usage":{"input_tokens":5,"cache_read_input_tokens":1000,"cache_creation_input_tokens":200,"output_tokens":50},"modelUsage":{"claude-b":{},"claude-a":{}}}'
`

const fakeCodex = `#!/bin/sh
args="$*"
out=""
while [ $# -gt 0 ]; do
  case "$1" in -o) out="$2"; shift ;; esac
  shift
done
cat > /dev/null
{
  echo "HOME=$HOME"
  echo "CODEX_HOME=$CODEX_HOME"
  echo "PATH=$PATH"
  echo "ARGS=$args"
  ls -A "$CODEX_HOME"
} > "$REVKIT_TEST_LOG"
if [ "$REVKIT_TEST_MODE" = replace ]; then
  rm "$CODEX_HOME/auth.json"
  printf 'login-2' > "$CODEX_HOME/auth.json"
fi
mkdir -p "$CODEX_HOME/sessions/2026/10/17"
s="$CODEX_HOME/sessions/2026/10/17/rollout.jsonl"
echo '{"type":"turn_context","payload":{"model":"gpt-test"}}' > "$s"
echo '{"type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":28.0,"window_minutes":10080,"resets_at":1792651633},"secondary":null}}}' >> "$s"
echo '{"type":"item.completed","item":{"type":"command_execution","exit_code":0,"status":"completed"}}'
echo '{"type":"item.completed","item":{"type":"command_execution","exit_code":1,"status":"failed"}}'
echo '{"type":"item.completed","item":{"type":"mcp_tool_call","status":"failed"}}'
echo '{"type":"turn.completed","usage":{"input_tokens":1000,"cached_input_tokens":900,"cache_write_input_tokens":0,"output_tokens":20,"reasoning_output_tokens":5}}'
printf 'codex reply' > "$out"
`

type fixture struct {
	user, credentials, bin, tree, dir, log string
	environ                                []string
}

func newFixture(t *testing.T, mode string) fixture {
	t.Helper()
	bin := resolvedTempDir(t)
	writeFile(t, filepath.Join(bin, "claude"), fakeClaude, 0o755)
	writeFile(t, filepath.Join(bin, "codex"), fakeCodex, 0o755)
	// Reach the fake clients through a linked directory, as a profile would.
	linked := filepath.Join(resolvedTempDir(t), "profile-bin")
	if err := os.Symlink(bin, linked); err != nil {
		t.Fatal(err)
	}
	user, credentials := userHome(t)
	f := fixture{user: user, credentials: credentials, bin: bin, tree: resolvedTempDir(t), dir: resolvedTempDir(t)}
	f.log = filepath.Join(resolvedTempDir(t), "seen")
	f.environ = []string{
		"PATH=" + linked + ":" + os.Getenv("PATH"), "HOME=" + user,
		"GITHUB_TOKEN=forge", "OPENAI_API_KEY=other-client", "ANTHROPIC_API_KEY=own-client",
		"CLAUDE_CODE_MESSAGING_TOKEN=session", "REVKIT_TEST_LOG=" + f.log, "REVKIT_TEST_MODE=" + mode,
	}
	return f
}

func (f fixture) job(client string, timeout time.Duration) Job {
	return Job{
		Reviewer: Reviewer{Name: "r", Client: client, Model: "m", Effort: "high"},
		Prompt:   "review this\n", Tree: f.tree, Dir: f.dir, Name: "find-r", Timeout: timeout,
		Environ: f.environ, Extra: []string{"REVKIT_TEST_LOG", "REVKIT_TEST_MODE"},
		Readable: []string{"/nix/store"},
	}
}

func (f fixture) seen(t *testing.T) string {
	t.Helper()
	content, err := os.ReadFile(f.log)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

func TestEnvironmentAllowsOnlyListedVariables(t *testing.T) {
	// A directory with no link on its path, so resolving PATH keeps it.
	bin := resolvedTempDir(t)
	environ := []string{
		"PATH=" + bin, "GITHUB_TOKEN=x", "FORGEJO_TOKEN=x", "GH_TOKEN=x", "CLAUDE_CODE_MESSAGING_TOKEN=x",
		"OPENAI_API_KEY=x", "ANTHROPIC_API_KEY=y", "SSH_AUTH_SOCK=x", "DBUS_SESSION_BUS_ADDRESS=x",
		"HOME=/home/user", "XDG_CONFIG_HOME=/home/user/.config", "EXTRA=1",
	}
	got := Environment("claude", environ, []string{"EXTRA"}, nil)
	want := []string{"PATH=" + bin, "ANTHROPIC_API_KEY=y", "HOME=/home/user", "EXTRA=1"}
	if !slices.Equal(got, want) {
		t.Fatalf("claude environment = %v\nwant %v", got, want)
	}
	home := &Home{Dir: "/h"}
	got = Environment("codex", environ, nil, home)
	want = append([]string{"PATH=" + bin, "OPENAI_API_KEY=x"}, home.Environment()...)
	if !slices.Equal(got, want) {
		t.Fatalf("codex environment = %v\nwant %v", got, want)
	}
}

func TestReviewerPathDropsRelativeEntries(t *testing.T) {
	bin := resolvedTempDir(t)
	for _, client := range []string{"claude", "codex"} {
		got := Environment(client, []string{"PATH=.:" + bin + "::bin"}, nil, nil)
		if !slices.Equal(got, []string{"PATH=" + bin}) {
			t.Fatalf("%s environment = %v", client, got)
		}
		got = Environment(client, []string{"PATH=.:bin"}, nil, nil)
		if !slices.Equal(got, []string{"PATH=" + defaultPath}) {
			t.Fatalf("%s environment without absolute entries = %v", client, got)
		}
	}
}

func TestValidReadablePath(t *testing.T) {
	for path, want := range map[string]bool{
		"/nix/store": true, "/opt/tools": true, "relative": false, "/": false, "/a/../b": false,
		`/a"b`: false, "/a\\b": false, ":root": false,
	} {
		if ValidReadablePath(path) != want {
			t.Errorf("ValidReadablePath(%q) = %v", path, !want)
		}
	}
}

func TestRunClaude(t *testing.T) {
	f := newFixture(t, "")
	result := Run(context.Background(), f.job("claude", hangGuard))
	if result.Status != StatusOK || result.Text != "reply text\n" {
		t.Fatalf("result = %+v", result)
	}
	seen := f.seen(t)
	// Claude keeps the user's home and configuration: --restricted and
	// --safe-mode keep the user's instructions out.
	for _, want := range []string{
		"HOME=" + f.user, "CLAUDE_CONFIG_DIR=unset",
		"GITHUB_TOKEN=unset", "OPENAI_API_KEY=unset", "ANTHROPIC_API_KEY=own-client", "PWD=" + f.tree,
		"--restricted --safe-mode", "--tools Read,Grep,Glob", "--strict-mcp-config",
	} {
		if !strings.Contains(seen, want) {
			t.Fatalf("the reviewer saw\n%s\nwithout %q", seen, want)
		}
	}
	if _, err := os.Stat(filepath.Join(f.dir, "find-r.home")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a Claude run made a home: %v", err)
	}
	if prompt, _ := os.ReadFile(f.log + ".prompt"); string(prompt) != "review this\n" {
		t.Fatalf("prompt = %q", prompt)
	}
	if total, ok := result.Usage.TotalInput("claude"); !ok || total != 1205 {
		t.Fatalf("total input = %d, %v", total, ok)
	}
	if result.Reads != 1 {
		t.Fatalf("reads = %d, want the one successful tool result", result.Reads)
	}
	if result.ReportedModel != "claude-a, claude-b" {
		t.Fatalf("reported model = %q", result.ReportedModel)
	}
	if len(result.Windows) != 2 || result.Windows[0].Name != "five_hour" || result.Windows[0].UsedPercent != 18 {
		t.Fatalf("windows = %+v", result.Windows)
	}
	if content, _ := os.ReadFile(result.Output); string(content) != "reply text\n" {
		t.Fatalf("output = %q", content)
	}
}

func TestRunCodex(t *testing.T) {
	f := newFixture(t, "")
	result := Run(context.Background(), f.job("codex", hangGuard))
	if result.Status != StatusOK || result.Text != "codex reply\n" {
		t.Fatalf("result = %+v", result)
	}
	seen := f.seen(t)
	home := filepath.Join(f.dir, "find-r.home")
	for _, want := range []string{
		"HOME=" + home, "CODEX_HOME=" + filepath.Join(home, ".codex"),
		// The linked PATH entry is resolved, since the sandbox cannot follow it.
		"PATH=" + f.bin + ":",
		`default_permissions="review"`, `":root"="deny"`, `"/nix/store"="read"`, `":workspace_roots"="read"`,
		`cli_auth_credentials_store="file"`, "--disable plugins", "--ignore-user-config",
		`web_search="disabled"`, `shell_environment_policy.exclude=["OPENAI_API_KEY", "CODEX_API_KEY"]`,
	} {
		if !strings.Contains(seen, want) {
			t.Fatalf("the reviewer saw\n%s\nwithout %q", seen, want)
		}
	}
	listing := strings.Fields(seen[strings.Index(seen, "\nARGS=")+1:])
	if listing[len(listing)-1] != "auth.json" || slices.Contains(listing, "AGENTS.md") {
		t.Fatalf("the client's directory held %v", listing)
	}
	if _, err := os.Stat(home); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the reviewer's home was not removed: %v", err)
	}
	if total, ok := result.Usage.TotalInput("codex"); !ok || total != 1000 {
		t.Fatalf("total input = %d, %v", total, ok)
	}
	if result.Reads != 1 {
		t.Fatalf("reads = %d, want the one command that exited 0", result.Reads)
	}
	if result.ReportedModel != "gpt-test" {
		t.Fatalf("reported model = %q", result.ReportedModel)
	}
	if len(result.Windows) != 1 || result.Windows[0].Name != "seven_day" || result.Windows[0].UsedPercent != 28 {
		t.Fatalf("windows = %+v", result.Windows)
	}
}

func TestRunFailedReviewerIsNotAReview(t *testing.T) {
	for _, mode := range []string{"fail", "error"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t, mode)
			result := Run(context.Background(), f.job("claude", hangGuard))
			if result.Status != StatusFailed || result.Problem == "" {
				t.Fatalf("result = %+v", result)
			}
		})
	}
}

func TestRunMissingClient(t *testing.T) {
	f := newFixture(t, "")
	job := f.job("claude", time.Minute)
	job.Environ = append(job.Environ, "PATH="+resolvedTempDir(t))
	result := Run(context.Background(), job)
	if result.Status != StatusFailed || !strings.Contains(result.Problem, "cannot start claude") {
		t.Fatalf("result = %+v", result)
	}
}

func TestRunContextDoneBeforeStart(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	for _, test := range []struct {
		name    string
		ctx     context.Context
		timeout time.Duration
		status  string
	}{
		{"timeout", context.Background(), 0, StatusTimeout},
		{"cancelled", cancelled, time.Minute, StatusInterrupted},
		{"parent-deadline", expired, time.Minute, StatusInterrupted},
		{"cancelled-with-timeout", cancelled, 0, StatusInterrupted},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t, "")
			// A zero timeout expires synchronously in context.WithTimeout,
			// before cmd.Start, without depending on scheduler timing.
			result := Run(test.ctx, f.job("claude", test.timeout))
			if result.Status != test.status || result.Problem != test.status || result.Exit != nil {
				t.Fatalf("result = %+v", result)
			}
			if _, err := os.Stat(f.log + ".prompt"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("reviewer started with a finished context: %v", err)
			}
		})
	}
}

func TestRunStartFailure(t *testing.T) {
	f := newFixture(t, "")
	// lookPath accepts this executable file, but cmd.Start cannot execute it.
	writeFile(t, filepath.Join(f.bin, "claude"), "not an executable format\n", 0o755)
	result := Run(context.Background(), f.job("claude", hangGuard))
	if result.Status != StatusFailed || !strings.Contains(result.Problem, "cannot start claude") || result.Exit != nil {
		t.Fatalf("result = %+v", result)
	}
}

func TestRunTimeout(t *testing.T) {
	f := newFixture(t, "hang")
	// Startup is part of the timeout. Do not assume the fixture has spawned
	// its child before this deadline; test group cleanup after readiness below.
	result := Run(context.Background(), f.job("claude", 50*time.Millisecond))
	if result.Status != StatusTimeout {
		t.Fatalf("result = %+v", result)
	}
	if result.Text != "" {
		t.Fatalf("timed-out reviewer produced a reply: %q", result.Text)
	}
}

func TestRunInterruptedKillsTheProcessGroup(t *testing.T) {
	f := newFixture(t, "hang")
	ctx, cancel := context.WithCancel(context.Background())
	var result Result
	done := make(chan struct{})
	go func() {
		result = Run(ctx, f.job("claude", hangGuard))
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(hangGuard):
			t.Error("the reviewer did not finish after cancellation")
		}
	})

	// The PID file is the fixture's readiness signal. Cancellation must
	// follow child startup, even when the build machine is heavily loaded.
	startup := time.NewTimer(hangGuard)
	defer startup.Stop()
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	var pid int
	for pid == 0 {
		select {
		case <-done:
			t.Fatalf("reviewer exited before its child was ready: %+v", result)
		case <-startup.C:
			t.Fatal("timed out waiting for the reviewer's child")
		case <-poll.C:
			content, err := os.ReadFile(f.log + ".child")
			if errors.Is(err, os.ErrNotExist) || (err == nil && len(content) == 0) {
				continue // The shell may have opened the file but not written it.
			}
			if err != nil {
				t.Fatal(err)
			}
			pid, err = strconv.Atoi(strings.TrimSpace(string(content)))
			if err != nil || pid <= 0 {
				t.Fatalf("invalid child PID %q: %v", content, err)
			}
		}
	}
	// Keep a failed cleanup assertion from leaving the fixture running.
	t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })
	if !alive(pid) {
		t.Fatalf("the reviewer's child %d exited before cancellation", pid)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(hangGuard):
		t.Fatal("the reviewer did not finish after cancellation")
	}
	if result.Status != StatusInterrupted {
		t.Fatalf("result = %+v", result)
	}
	deadline := time.Now().Add(5 * time.Second)
	for alive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("the reviewer's child %d survived", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestRunNotesAReplacedCodexLogin(t *testing.T) {
	f := newFixture(t, "replace")
	result := Run(context.Background(), f.job("codex", hangGuard))
	if result.Status != StatusOK {
		t.Fatalf("result = %+v", result)
	}
	if content, _ := os.ReadFile(f.credentials); string(content) != "login-1" {
		t.Fatalf("the user's login changed: %q", content)
	}
	if len(result.Notes) != 1 || !strings.Contains(result.Notes[0], "log in again") {
		t.Fatalf("notes = %v", result.Notes)
	}
}

// alive reports whether pid runs. A killed process whose parent is gone
// stays a zombie where nothing reaps orphans, as in some containers.
func alive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return true
	}
	// The state follows the command name, which is in parentheses.
	fields := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+1:]))
	return len(fields) == 0 || fields[0] != "Z"
}

// resolvedTempDir is a temporary directory with links on its path resolved,
// as PATH resolution resolves them (TMPDIR may pass through a link, as
// /var does on macOS).
func resolvedTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}
