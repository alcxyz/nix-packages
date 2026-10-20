package main

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// killGrace is how long a command's output may stay open after it exits or
// is cancelled before deck gives up on it.
var killGrace = 5 * time.Second

// runGroup runs cmd in its own process group. Cancelling its context kills
// the whole group. A child that outlives the command and still holds its
// output after killGrace has its group killed too, whatever the command's
// exit status; while it holds the output it keeps the group's ID in use, so
// the kill cannot reach another process. A command that ends on its own and
// releases its output leaves its group alone: by then the ID may already
// belong to another process.
//
// deck makes the output pipes itself, rather than leaving them to exec, so
// it sees when they are still held after the command exits: exec reports
// that only for a command that exited successfully.
func runGroup(cmd *exec.Cmd) error {
	var readers, writers []*os.File
	var copying sync.WaitGroup
	for _, out := range []*io.Writer{&cmd.Stdout, &cmd.Stderr} {
		if *out == nil {
			continue
		}
		r, w, err := os.Pipe()
		if err != nil {
			closeFiles(readers)
			closeFiles(writers)
			return err
		}
		readers = append(readers, r)
		writers = append(writers, w)
		dst := *out
		*out = w
		copying.Add(1)
		go func() {
			defer copying.Done()
			io.Copy(dst, r)
		}()
	}
	defer closeFiles(readers)

	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killGroup(cmd.Process.Pid) }
	// WaitDelay still bounds the stdin copy, which exec makes.
	cmd.WaitDelay = killGrace
	err := cmd.Start()
	closeFiles(writers)
	if err != nil {
		copying.Wait()
		return err
	}
	err = cmd.Wait()

	copied := make(chan struct{})
	go func() {
		copying.Wait()
		close(copied)
	}()
	held := false
	select {
	case <-copied:
	case <-time.After(killGrace):
		held = true
	}
	if held || errors.Is(err, exec.ErrWaitDelay) {
		killGroup(cmd.Process.Pid)
	}
	if held {
		// The kill ends the holders; closing the pipes also ends the copy
		// should one have left the group.
		closeFiles(readers)
		<-copied
		if err == nil {
			err = exec.ErrWaitDelay
		}
	}
	return err
}

func closeFiles(files []*os.File) {
	for _, f := range files {
		f.Close()
	}
}

func killGroup(pid int) error {
	err := syscall.Kill(-pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}
