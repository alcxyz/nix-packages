package main

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
)

func TestPrivateStateRoundTrip(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	want := &State{RecorderPID: 123, OutputFile: "recording.mp4"}
	if err := saveState(want); err != nil {
		t.Fatal(err)
	}
	got, err := loadState()
	if err != nil || *got != *want {
		t.Fatalf("state = %v, %v", got, err)
	}
	for path, mode := range map[string]os.FileMode{stateDir(): 0700, filepath.Join(stateDir(), "state.json"): 0600} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("%s must have mode %o: %v, %v", path, mode, info, err)
		}
	}
	removeState()
	if _, err := loadState(); !errors.Is(err, errNoRecording) {
		t.Fatalf("removed state: %v", err)
	}
}

func TestFallbackIsPerUser(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("TMPDIR", t.TempDir())
	if want := filepath.Join(os.TempDir(), "wcap-"+strconv.Itoa(os.Getuid())); stateDir() != want {
		t.Fatalf("stateDir = %s, want %s", stateDir(), want)
	}
	if err := saveState(&State{RecorderPID: 123}); err != nil {
		t.Fatal(err)
	}
}

func TestPrivateOwnershipCheck(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !privateOwned(info) {
		t.Fatalf("private fixture directory rejected: mode=%v owner=%d user=%d", info.Mode(), info.Sys().(*syscall.Stat_t).Uid, os.Getuid())
	}
	info.Sys().(*syscall.Stat_t).Uid++
	if privateOwned(info) {
		t.Fatal("directory owned by another user accepted")
	}
}

func TestMissingRuntimeParentRefusesStart(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(t.TempDir(), "missing"))
	t.Setenv("PATH", t.TempDir())
	if _, err := loadState(); err == nil || errors.Is(err, errNoRecording) {
		t.Fatalf("missing runtime directory treated as absent state: %v", err)
	}
	if status := runStart(nil); status != 1 {
		t.Fatalf("start status = %d, want 1", status)
	}
}

func TestStopClearsInvalidPrivateState(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	if err := saveState(&State{RecorderPID: 123}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir(), "state.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if status := runStop(nil); status != 1 {
		t.Fatalf("stop must report incomplete restoration, got %d", status)
	}
	if _, err := loadState(); !errors.Is(err, errNoRecording) {
		t.Fatalf("invalid state not cleared: %v", err)
	}
}

func TestStateDirectoryRefusesExistingUnsuitablePaths(t *testing.T) {
	for _, kind := range []string{"symlink", "shared", "file", "foreign-owner"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
			path := stateDir()
			switch kind {
			case "symlink":
				if err := os.Symlink(t.TempDir(), path); err != nil {
					t.Fatal(err)
				}
			case "file":
				if err := os.WriteFile(path, []byte("unchanged"), 0600); err != nil {
					t.Fatal(err)
				}
			default:
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
				if kind == "shared" {
					if err := os.Chmod(path, 0777); err != nil {
						t.Fatal(err)
					}
				} else {
					if os.Getuid() != 0 {
						t.Skip("changing a fixture owner requires root")
					}
					if err := os.Chown(path, 1, -1); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := saveState(&State{RecorderPID: 123}); err == nil {
				t.Fatal("unsuitable directory accepted for write")
			}
			if _, err := loadState(); err == nil {
				t.Fatal("unsuitable directory accepted for read")
			}
		})
	}
}

func TestStateReplacementDoesNotFollowExistingLinks(t *testing.T) {
	for _, link := range []string{"symbolic", "hard"} {
		t.Run(link, func(t *testing.T) {
			t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
			target := filepath.Join(t.TempDir(), "target")
			if err := os.WriteFile(target, []byte("unchanged"), 0600); err != nil {
				t.Fatal(err)
			}
			dir, err := openStateDir()
			if err != nil {
				t.Fatal(err)
			}
			dir.Close()
			path := filepath.Join(stateDir(), "state.json")
			makeLink := os.Symlink
			if link == "hard" {
				makeLink = os.Link
			}
			if err := makeLink(target, path); err != nil {
				t.Fatal(err)
			}
			if err := saveState(&State{RecorderPID: 123}); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(target)
			if err != nil || string(data) != "unchanged" {
				t.Fatalf("target changed: %q, %v", data, err)
			}
			if _, err := loadState(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLoadStateRejectsNonprivateAndNonregularFiles(t *testing.T) {
	for _, kind := range []string{"symlink", "readable", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
			if err := saveState(&State{RecorderPID: 123}); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(stateDir(), "state.json")
			if kind == "readable" {
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if kind == "fifo" {
					if err := syscall.Mkfifo(path, 0600); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Symlink("missing", path); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := loadState(); err == nil {
				t.Fatal("unsuitable file accepted")
			}
			if status := runStart(nil); status != 1 {
				t.Fatalf("start status = %d, want 1", status)
			}
		})
	}
}
