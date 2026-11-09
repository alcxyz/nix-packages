package main

import (
	"os"
	"syscall"
	"testing"

	"nix-deploy/k8snode"
)

// An interrupted run exits with the signal's status, unless the reboot
// command's outcome is unknown: a reboot that may still happen wins.
func TestInterruptedStatus(t *testing.T) {
	for _, c := range []struct {
		status int
		sig    os.Signal
		want   int
	}{
		{1, os.Interrupt, 130},
		{1, syscall.SIGTERM, 143},
		{0, os.Interrupt, 130},
		{k8snode.StatusRebootUncertain, os.Interrupt, k8snode.StatusRebootUncertain},
		{k8snode.StatusRebootUncertain, syscall.SIGTERM, k8snode.StatusRebootUncertain},
	} {
		if got := interruptedStatus(c.status, c.sig); got != c.want {
			t.Errorf("interruptedStatus(%d, %v) = %d, want %d", c.status, c.sig, got, c.want)
		}
	}
}
