package k8snode

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// statusNotStarted is the status of a command refused because the run was
// interrupted. No external command starts after an interrupt, apart from the
// cleanup that restores cluster settings.
const statusNotStarted = -1

// StatusNotStarted is the status of a command refused because the run was
// already interrupted: it never ran.
const StatusNotStarted = statusNotStarted

// killGrace matches `timeout --kill-after=5`: a command that ignores SIGTERM
// at its deadline or interrupt is killed five seconds later.
const killGrace = 5 * time.Second

// Command is one external program run.
type Command struct {
	Argv []string
	// Stdin is the program's whole input; without it the program reads an
	// empty input, never the terminal.
	Stdin string
	// Timeout, when set, stops the program at its deadline.
	Timeout time.Duration
	// Passthrough shows standard output to the operator instead of
	// capturing it.
	Passthrough bool
	// Quiet captures standard error instead of showing it.
	Quiet bool
}

// Result is what a command left behind. Stderr is set only for Quiet
// commands.
type Result struct {
	Stdout string
	Stderr string
	Status int
}

// Runner runs every external command of an operation, kubectl and ssh
// included.
type Runner struct {
	Stdout io.Writer
	Stderr io.Writer
	// Fake, when set, answers every command instead of running it. Tests use
	// it to serve canned cluster and host state and record the calls.
	Fake func(Command) Result
}

// Run executes the command and returns its output and exit status: 127 when
// it cannot start, 124 when its deadline stopped it, 128 plus the signal
// number when a signal ended it, and statusNotStarted when ctx was already
// done.
func (r *Runner) Run(ctx context.Context, c Command) Result {
	if ctx.Err() != nil {
		return Result{Status: statusNotStarted, Stderr: "interrupted"}
	}
	if r.Fake != nil {
		result := r.Fake(c)
		if c.Passthrough {
			io.WriteString(r.Stdout, result.Stdout)
			result.Stdout = ""
		}
		if !c.Quiet {
			io.WriteString(r.Stderr, result.Stderr)
			result.Stderr = ""
		}
		return result
	}

	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, c.Argv[0], c.Argv[1:]...)
	cmd.Stdin = strings.NewReader(c.Stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	if c.Passthrough {
		cmd.Stdout = r.Stdout
	}
	cmd.Stderr = r.Stderr
	if c.Quiet {
		cmd.Stderr = &stderr
	}
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = killGrace

	err := cmd.Run()
	result := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if c.Timeout > 0 && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		result.Status = 124
		return result
	}
	if err == nil {
		return result
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			result.Status = 128 + int(status.Signal())
			return result
		}
		result.Status = exitErr.ExitCode()
		return result
	}
	// Without a process, an interrupt after the first check kept the
	// command from starting; a command that ran and exited cleanly
	// succeeded, though the interrupt was reported.
	if cmd.Process == nil && ctx.Err() != nil {
		result.Status = statusNotStarted
		return result
	}
	if cmd.ProcessState != nil && cmd.ProcessState.Success() {
		return result
	}
	fmt.Fprintf(r.Stderr, "error: %s: %v\n", c.Argv[0], err)
	result.Status = 127
	return result
}

// Has reports whether a program is on the search path, as `command -v`
// does. A fake runner answers the `command -v` call itself.
func (r *Runner) Has(name string) bool {
	if r.Fake != nil {
		return r.Fake(Command{Argv: []string{"command", "-v", name}, Quiet: true}).Status == 0
	}
	_, err := exec.LookPath(name)
	return err == nil
}
