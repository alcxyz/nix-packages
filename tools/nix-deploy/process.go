package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// killGrace matches `timeout --kill-after=5`: a command that ignores SIGTERM
// at its deadline is killed five seconds later.
const killGrace = 5 * time.Second

// exitTimedOut is the status `timeout` reports for a command it stopped.
const exitTimedOut = 124

// command describes one external program run. Zero values attach the
// program's input and output to this process.
type command struct {
	argv    []string
	stdin   io.Reader
	stdout  io.Writer
	stderr  io.Writer
	timeout time.Duration
}

// run executes the command and returns its exit status: 127 when it cannot
// start, exitTimedOut when its deadline stopped it, and 128 plus the signal
// number when a signal ended it.
func (c command) run() int {
	ctx := context.Background()
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, c.argv[0], c.argv[1:]...)
	cmd.Stdin = c.stdin
	if c.stdin == nil {
		cmd.Stdin = os.Stdin
	}
	cmd.Stdout = c.stdout
	if c.stdout == nil {
		cmd.Stdout = os.Stdout
	}
	cmd.Stderr = c.stderr
	if c.stderr == nil {
		cmd.Stderr = os.Stderr
	}
	// Only a deadline limits the wait. Without one, wait for output like the
	// shell would, even when a helper the command started keeps its pipes
	// open.
	if c.timeout > 0 {
		cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
		cmd.WaitDelay = killGrace
	}

	err := cmd.Run()
	if c.timeout > 0 && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return exitTimedOut
	}
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 128 + int(status.Signal())
		}
		return exitErr.ExitCode()
	}
	fmt.Fprintf(c.errorWriter(), "deploy: %s: %v\n", c.argv[0], err)
	return 127
}

func (c command) errorWriter() io.Writer {
	if c.stderr != nil {
		return c.stderr
	}
	return os.Stderr
}

// capture runs the command without input and returns its combined output.
func capture(timeout time.Duration, argv ...string) (string, int) {
	var output bytes.Buffer
	status := command{argv: argv, stdin: bytes.NewReader(nil), stdout: &output, stderr: &output, timeout: timeout}.run()
	return output.String(), status
}

// oneLine collapses command output into a single line for an error message.
func oneLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// lastLine returns the last non-empty line of the output.
func lastLine(text string) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	return lines[len(lines)-1]
}

// lockedBuffer collects output from several writers at once, such as a
// command's stdout and stderr, which os/exec copies concurrently.
type lockedBuffer struct {
	lock sync.Mutex
	data bytes.Buffer
}

func (b *lockedBuffer) Write(data []byte) (int, error) {
	b.lock.Lock()
	defer b.lock.Unlock()
	return b.data.Write(data)
}

func (b *lockedBuffer) String() string {
	b.lock.Lock()
	defer b.lock.Unlock()
	return b.data.String()
}

// prefixWriter writes each complete line to its destination with a tag, so
// output from parallel jobs stays attributable. Lines from different writers
// sharing one lock never interleave. Lines starting with skip are dropped.
type prefixWriter struct {
	lock    *sync.Mutex
	dst     io.Writer
	prefix  string
	skip    string
	pending []byte
}

func (w *prefixWriter) Write(data []byte) (int, error) {
	w.pending = append(w.pending, data...)
	for {
		end := bytes.IndexByte(w.pending, '\n')
		if end < 0 {
			return len(data), nil
		}
		w.emit(w.pending[:end+1])
		w.pending = w.pending[end+1:]
	}
}

// flush writes a final line that had no newline.
func (w *prefixWriter) flush() {
	if len(w.pending) > 0 {
		w.emit(append(w.pending, '\n'))
		w.pending = nil
	}
}

func (w *prefixWriter) emit(line []byte) {
	if w.skip != "" && bytes.HasPrefix(line, []byte(w.skip)) {
		return
	}
	w.lock.Lock()
	defer w.lock.Unlock()
	fmt.Fprintf(w.dst, "%s\t%s", w.prefix, line)
}
