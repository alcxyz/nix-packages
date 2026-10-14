package main

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPrefixWriterTagsCompleteLines(t *testing.T) {
	var out bytes.Buffer
	w := &prefixWriter{lock: &sync.Mutex{}, dst: &out, prefix: "[hm] [node]"}
	w.Write([]byte("first\nsec"))
	w.Write([]byte("ond\nunterminated"))
	w.flush()
	want := "[hm] [node]\tfirst\n[hm] [node]\tsecond\n[hm] [node]\tunterminated\n"
	if out.String() != want {
		t.Errorf("got %q, want %q", out.String(), want)
	}
}

func TestPrefixWriterSkipsLines(t *testing.T) {
	var out bytes.Buffer
	w := &prefixWriter{lock: &sync.Mutex{}, dst: &out, prefix: "[hm] [node]", skip: fetchLockWait}
	w.Write([]byte(fetchLockWait + " 'git+file:///src'...\nevaluation warning: kept\n"))
	w.Write([]byte(fetchLockWait + " 'git+file:///src'..."))
	w.flush()
	want := "[hm] [node]\tevaluation warning: kept\n"
	if out.String() != want {
		t.Errorf("got %q, want %q", out.String(), want)
	}
}

func TestCommandStatus(t *testing.T) {
	if _, status := capture(0, "sh", "-c", "exit 7"); status != 7 {
		t.Errorf("exit status: got %d", status)
	}
	if _, status := capture(0, "nix-deploy-test-missing-command"); status != 127 {
		t.Errorf("missing command: got %d", status)
	}
	started := time.Now()
	if _, status := capture(200*time.Millisecond, "sleep", "30"); status != exitTimedOut {
		t.Errorf("timeout: got %d", status)
	}
	if time.Since(started) > 10*time.Second {
		t.Errorf("timeout did not stop the command")
	}
}

func TestFormatGiB(t *testing.T) {
	if got := formatGiB(4194304); got != "4.0 GiB" {
		t.Errorf("got %q", got)
	}
	if got := formatGiB(1572864); got != "1.5 GiB" {
		t.Errorf("got %q", got)
	}
}

func TestLockedBufferAcceptsConcurrentStreams(t *testing.T) {
	var output lockedBuffer
	status := command{
		argv:  []string{"sh", "-c", "for i in $(seq 200); do echo out; echo err >&2; done; echo marker >&2"},
		stdin: bytes.NewReader(nil),
		// Distinct writers, as in a switch, make os/exec copy both streams
		// concurrently.
		stdout: io.MultiWriter(&output),
		stderr: io.MultiWriter(&output),
	}.run()
	if status != 0 {
		t.Fatalf("status %d", status)
	}
	if !strings.Contains(output.String(), "marker") {
		t.Errorf("output lost the last stderr line")
	}
}
