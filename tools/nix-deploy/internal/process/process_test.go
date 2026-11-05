package process

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestBackgroundControllerDoesNotStealTerminal(t *testing.T) {
	if os.Getenv("NIXFLEET_TERMINAL_TEST") == "background" {
		cmd := exec.Command("sh", "-c", "exit 0")
		cmd.Stdin = os.Stdin
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		restore := attachTerminal(cmd)
		defer restore()
		// Check before starting, so a regression cannot strand a stopped child.
		if cmd.SysProcAttr.Foreground {
			t.Fatal("background controller claimed terminal ownership")
		}
		if err := cmd.Run(); err != nil {
			t.Fatal(err)
		}
		return
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	// Only a private synthetic PTY and harmless child processes are used.
	script := `import fcntl, os, pty, signal, subprocess, sys, termios
master, slave = pty.openpty()
os.setsid()
signal.signal(signal.SIGHUP, signal.SIG_IGN)
fcntl.ioctl(slave, termios.TIOCSCTTY, 0)
child = subprocess.Popen([sys.argv[1], '-test.run=^TestBackgroundControllerDoesNotStealTerminal$'],
    stdin=slave, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
    env={**os.environ, 'NIXFLEET_TERMINAL_TEST':'background'}, preexec_fn=lambda: os.setpgid(0,0))
try:
    output, _ = child.communicate(timeout=10)
    sys.stdout.buffer.write(output)
    sys.exit(child.returncode)
finally:
    if child.poll() is None:
        os.killpg(child.pid, signal.SIGKILL)
        child.wait()
    os.close(master)
    os.close(slave)
`
	if output, err := exec.Command(python, "-c", script, os.Args[0]).CombinedOutput(); err != nil {
		t.Fatalf("background terminal fixture: %v\n%s", err, output)
	}
}

func TestCancellationStopsDescendantAfterParentExits(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "survived")
	// The parent dies on TERM; the descendant ignores it and closes inherited
	// output, so waiting for the immediate child alone would release too soon.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	cmd := exec.Command("sh", "-c", `(trap '' TERM; sleep 7; echo survived > "$1") >/dev/null 2>&1 & wait`, "sh", marker)
	start := time.Now()
	if err := Run(ctx, cmd); err == nil {
		t.Fatal("cancellation succeeded")
	}
	if time.Since(start) < Grace {
		t.Fatal("returned before descendant escalation")
	}
	// A follow-up command lasts beyond the would-be write, without polling.
	if err := exec.Command("sleep", "3").Run(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("descendant survived cancellation")
	}
}

func TestSuccessfulParentWithInheritedPipeRemainsSuccessful(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 30 & exit 0")
	var output bytes.Buffer
	cmd.Stdout = &output
	if err := Run(context.Background(), cmd); err != nil {
		t.Fatalf("successful parent: %v", err)
	}
}
