package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

// failedActivations remembers, on this machine, generations whose activation
// did not finish. A failed or interrupted switch can still leave
// /run/current-system and the profile pointing at the new generation, so the
// links alone would call the next run unchanged and skip the retry.
//
// An entry is written before an activation starts, and the activation does not
// start unless it was written. Entries are removed only when an activation of
// that target succeeds, so a run that is interrupted or dies still leaves them
// behind. Each target keeps every generation that did not finish, since a
// later failure must not hide an earlier one the host may still point to.
// Every change re-reads the record under a file lock, so concurrent runs on
// this machine do not drop each other's entries, and each target is locked
// across runs from begin to finish, so one run's success cannot clear
// another run's activation of the same target while it is in flight. A
// record that cannot be read counts as unfinished: the target is then
// activated, which needs a saved entry, so it fails instead of passing as
// unchanged.
type failedActivations struct {
	lock    sync.Mutex
	path    string
	holders map[string]*os.File
}

func loadFailedActivations() *failedActivations {
	state := os.Getenv("XDG_STATE_HOME")
	if state == "" {
		home, _ := os.UserHomeDir()
		state = filepath.Join(home, ".local/state")
	}
	return &failedActivations{
		path:    filepath.Join(state, "nix-deploy", "failed-activations.json"),
		holders: map[string]*os.File{},
	}
}

// failed reports whether an activation of path on the target did not finish.
func (f *failedActivations) failed(t target, path string) bool {
	unfinished := true
	f.withRecord(false, func(entries map[string][]string) bool {
		unfinished = contains(entries[t.key()], path)
		return false
	})
	return unfinished
}

// begin takes the target's lock across runs, waiting while another run
// activates it, and marks an activation of path as unfinished. The
// activation must not start when this fails; finish releases the lock.
func (f *failedActivations) begin(t target, path string) bool {
	if !f.hold(t) {
		return false
	}
	ok := f.withRecord(true, func(entries map[string][]string) bool {
		if contains(entries[t.key()], path) {
			return false
		}
		entries[t.key()] = append(entries[t.key()], path)
		return true
	})
	if !ok {
		f.release(t)
	}
	return ok
}

// finish records the result and releases the target. A success means the
// host now runs the generation fully, so every earlier entry for the target
// is obsolete. A failure keeps the entries for the next run to retry.
func (f *failedActivations) finish(t target, ok bool) {
	defer f.release(t)
	if !ok {
		return
	}
	f.withRecord(true, func(entries map[string][]string) bool {
		if _, present := entries[t.key()]; !present {
			return false
		}
		delete(entries, t.key())
		return true
	})
}

// hold locks the target across runs until release. The kernel drops the
// lock if this run dies.
func (f *failedActivations) hold(t target) bool {
	dir := filepath.Join(filepath.Dir(f.path), "targets")
	err := os.MkdirAll(dir, 0o755)
	var file *os.File
	if err == nil {
		file, err = os.OpenFile(filepath.Join(dir, t.phase+"-"+t.host+".lock"), os.O_CREATE|os.O_RDWR, 0o644)
	}
	if err == nil {
		if syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
			fmt.Fprintf(os.Stderr, "deploy: waiting for another deploy run to finish activating %s %s\n", t.phase, t.host)
			err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX)
		}
		if err != nil {
			file.Close()
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "deploy: cannot lock %s %s for activation: %v\n", t.phase, t.host, err)
		return false
	}
	f.lock.Lock()
	f.holders[t.key()] = file
	f.lock.Unlock()
	return true
}

func (f *failedActivations) release(t target) {
	f.lock.Lock()
	file := f.holders[t.key()]
	delete(f.holders, t.key())
	f.lock.Unlock()
	if file != nil {
		file.Close()
	}
}

// withRecord reads the record under a lock shared with other deploy runs,
// lets change modify it and, when change reports a modification, replaces
// the file atomically. It returns false when the record could not be read
// or written.
func (f *failedActivations) withRecord(write bool, change func(map[string][]string) bool) bool {
	f.lock.Lock()
	defer f.lock.Unlock()

	err := os.MkdirAll(filepath.Dir(f.path), 0o755)
	var lockFile *os.File
	if err == nil {
		lockFile, err = os.OpenFile(f.path+".lock", os.O_CREATE|os.O_RDWR, 0o644)
	}
	if err == nil {
		defer lockFile.Close()
		mode := syscall.LOCK_SH
		if write {
			mode = syscall.LOCK_EX
		}
		err = syscall.Flock(int(lockFile.Fd()), mode)
	}

	entries := map[string][]string{}
	if err == nil {
		data, readErr := os.ReadFile(f.path)
		switch {
		case readErr == nil:
			// A damaged record must not pass for an empty one.
			err = json.Unmarshal(data, &entries)
			if err == nil && entries == nil {
				err = fmt.Errorf("the record is null")
			}
		case !os.IsNotExist(readErr):
			err = readErr
		}
	}
	if err == nil && change(entries) && write {
		err = f.save(entries)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "deploy: cannot use the activation record %s: %v\n", f.path, err)
		return false
	}
	return true
}

// save replaces the record atomically, so an interrupted write cannot lose
// it.
func (f *failedActivations) save(entries map[string][]string) error {
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(f.path), ".failed-activations-*")
	if err != nil {
		return err
	}
	_, err = temp.Write(append(data, '\n'))
	if err == nil {
		// The record must survive a crash right after the rename.
		err = temp.Sync()
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(temp.Name(), f.path)
	}
	if err != nil {
		os.Remove(temp.Name())
		return err
	}
	// The rename itself must also survive a crash.
	dir, err := os.Open(filepath.Dir(f.path))
	if err != nil {
		return err
	}
	err = dir.Sync()
	if closeErr := dir.Close(); err == nil {
		err = closeErr
	}
	return err
}
