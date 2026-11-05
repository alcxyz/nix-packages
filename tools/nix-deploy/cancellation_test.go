package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type enteredReader struct {
	input   io.Reader
	entered chan struct{}
}

func (r *enteredReader) Read(data []byte) (int, error) { close(r.entered); return r.input.Read(data) }

func TestBlockedPromptsObserveCancellation(t *testing.T) {
	for _, kind := range []string{"runner", "triage"} {
		t.Run(kind, func(t *testing.T) {
			d := failedRun(t, map[string]any{"agentCommand": []string{"synthetic-agent"}})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			previous := runContext
			runContext = ctx
			defer func() { runContext = previous }()
			reader, writer := io.Pipe()
			defer reader.Close()
			defer writer.Close()
			input := &enteredReader{input: reader, entered: make(chan struct{})}
			done := make(chan struct{})
			go func() {
				if kind == "runner" {
					if askLine(input, io.Discard)("Continue? ") {
						t.Error("canceled prompt agreed")
					}
				} else {
					d.offerTriage(1, true, input, io.Discard, func([]string) int { t.Error("triage launched after cancellation"); return 0 })
				}
				close(done)
			}()
			<-input.entered
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("prompt stayed blocked after cancellation")
			}
		})
	}
}

func TestRebootMarkCleanupOutlivesCancellation(t *testing.T) {
	bin := t.TempDir()
	log := filepath.Join(bin, "calls")
	os.WriteFile(filepath.Join(bin, "hostname"), []byte("#!/bin/sh\necho controller\n"), 0700)
	os.WriteFile(filepath.Join(bin, "kubectl"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$CLEANUP_LOG\"\n"), 0700)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("CLEANUP_LOG", log)
	status, _, err := openStatusCheck(writeInventory(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	previous := runContext
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runContext = ctx
	defer func() { runContext = previous }()
	for name, client := range map[string]*statusCheck{
		"inspection": status,
		"reboot":     makeStatusCheck(status.inv, status.self, "", status.connectTimeout),
	} {
		if _, _, code := client.run(time.Second, "kubectl", "get", "nodes"); code == 0 {
			t.Fatal("ordinary inspection ran after cancellation")
		}
		f := newFleetReboot(client.inv, client.self, rebootOptions{}, client)
		f.marked = []string{"node"}
		f.out = io.Discard
		f.clearMarks()
		calls, err := os.ReadFile(log)
		if err != nil || !strings.Contains(string(calls), "taint nodes node") || len(f.marked) != 0 {
			t.Fatalf("%s mark cleanup refused: %q %v %v", name, calls, err, f.marked)
		}
	}

}

func TestTriageInterruptDoesNotCancelFinishedController(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	previous := stopControllerSignals
	stopControllerSignals = controllerSignals(ctx, cancel)
	defer func() { stopControllerSignals(); stopControllerSignals = previous }()
	agent := filepath.Join(t.TempDir(), "agent")
	if err := os.WriteFile(agent, []byte("#!/bin/sh\nkill -INT \"$PPID\"\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if status := launchTriage(&triageConfig{AgentCommand: []string{agent}}, "/fixture/context", t.TempDir(), "/fixture/playbook", false); status != 0 {
		t.Fatalf("agent exited %d", status)
	}
	if ctx.Err() != nil || canceledStatus.Load() != 0 {
		t.Fatal("triage interrupt rewrote the controller's result")
	}
}
