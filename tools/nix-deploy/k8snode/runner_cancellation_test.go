package k8snode

import (
	"context"
	"io"
	"testing"
)

type cancelOnOutput struct{ cancel context.CancelFunc }

func (w cancelOnOutput) Write(data []byte) (int, error) {
	w.cancel()
	return len(data), nil
}

func TestCleanExitRacingCancellationIsAmbiguous(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runner := &Runner{Stdout: cancelOnOutput{cancel}, Stderr: io.Discard}
	// A harmless synthetic child can exit zero before cancellation wins its
	// acknowledgement. A remote reboot in that window may already be queued.
	result := runner.Run(ctx, Command{Argv: []string{"sh", "-c", "printf ready"}, Passthrough: true})
	if result.Status != 130 {
		t.Fatalf("cancelled command reported %d", result.Status)
	}
}

func TestAlreadyCancelledCommandDidNotStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := (&Runner{Stdout: io.Discard, Stderr: io.Discard}).Run(ctx, Command{Argv: []string{"synthetic-command-not-installed"}})
	if result.Status != StatusNotStarted {
		t.Fatalf("unstarted command reported %d", result.Status)
	}
}
