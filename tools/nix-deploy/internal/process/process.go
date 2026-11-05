// Package process waits for a command's process group to stop on cancellation.
package process

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
	"time"
)

const Grace = 5 * time.Second

// Run starts a separate process group, terminates the group on cancellation,
// escalates even when the immediate child has exited, and waits before returning.
func Run(ctx context.Context, cmd *exec.Cmd) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	restore := attachTerminal(cmd)
	defer restore()
	if err := ctx.Err(); err != nil {
		return err
	}
	cmd.WaitDelay = Grace
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	waited := false
	select {
	case err := <-done:
		waited = true
		if ctx.Err() == nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState.Success() {
				return nil
			}
			return err
		}
	case <-ctx.Done():
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	timer := time.NewTimer(Grace)
	defer timer.Stop()
	<-timer.C
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if !waited {
		<-done
	}
	return ctx.Err()
}
