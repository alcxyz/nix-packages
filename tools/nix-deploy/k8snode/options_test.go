package k8snode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func jsonUnmarshal(text string, v any) error {
	return json.Unmarshal([]byte(text), v)
}

func TestParseArgsDefaults(t *testing.T) {
	t.Setenv("K8S_NODE_NETWORK_AUDIT_SCRIPT", "/audit")
	t.Setenv("K8S_NODE_SSH_USER", "")
	opts, err := ParseArgs("reboot", []string{"server-a"})
	if err != nil {
		t.Fatal(err)
	}
	want := Options{
		Action: "reboot", Host: "server-a", Node: "server-a", SSHTarget: "root@server-a",
		DrainTimeout: "10m", ReadyTimeout: "10m", FenceTimeout: "10m", SettleTimeout: "90m",
		SSHTimeoutSeconds: 900, MinFreePodSlots: 20, NetworkAuditScript: "/audit", NodeSSHUser: "root",
	}
	if opts != want {
		t.Fatalf("got %+v", opts)
	}

	opts, err = ParseArgs("off", []string{"--node", "n1", "--ssh-target", "admin@h1", "--min-free-pod-slots", "5", "--ssh-timeout", "30", "h1"})
	if err != nil {
		t.Fatal(err)
	}
	// Power-off always leaves the node cordoned.
	if opts.Node != "n1" || opts.SSHTarget != "admin@h1" || opts.MinFreePodSlots != 5 || opts.SSHTimeoutSeconds != 30 || !opts.LeaveCordoned {
		t.Fatalf("got %+v", opts)
	}
}

func TestParseArgsErrors(t *testing.T) {
	t.Setenv("K8S_NODE_SSH_USER", "")
	cases := []struct {
		action string
		args   []string
		want   string
	}{
		{"reboot", []string{"--resume-maintenance", "--skip-drain", "h"}, "--resume-maintenance and --skip-drain cannot be combined"},
		{"on", []string{"--resume-maintenance", "h"}, "--resume-maintenance is for reboot/off cycles"},
		{"reboot", []string{"a", "b"}, "only one host can be supplied"},
		{"reboot", []string{"--bogus", "h"}, "unknown option: --bogus"},
		{"reboot", []string{"-oProxyCommand=x"}, "unknown option: -oProxyCommand=x"},
		{"reboot", []string{"h", "--node"}, "--node requires a value"},
		{"reboot", []string{"--min-free-pod-slots", "0", "h"}, "--min-free-pod-slots must be a positive integer"},
		{"reboot", []string{"--settle-timeout", "1h30m", "h"}, "unsupported duration: 1h30m"},
		{"reboot", []string{"--drain-timeout", "soon", "h"}, "unsupported duration: soon"},
		{"reboot", []string{"--ssh-timeout", "-1", "h"}, "--ssh-timeout must be a whole number of seconds"},
		{"restart", []string{"h"}, "unsupported action: restart"},
	}
	for _, c := range cases {
		_, err := ParseArgs(c.action, c.args)
		expectFailure(t, err, c.want)
	}

	t.Setenv("K8S_NODE_SSH_USER", "root;true")
	_, err := ParseArgs("reboot", []string{"h"})
	expectFailure(t, err, "unsafe Kubernetes node SSH user: root;true")
	t.Setenv("K8S_NODE_SSH_USER", "")

	if _, err := ParseArgs("reboot", nil); !errors.Is(err, ErrUsage) {
		t.Fatalf("no host: %v", err)
	}
	if _, err := ParseArgs("reboot", []string{"--help"}); !errors.Is(err, ErrHelp) {
		t.Fatalf("help: %v", err)
	}
}

func TestParseDuration(t *testing.T) {
	for text, want := range map[string]time.Duration{"0": 0, "45": 45 * time.Second, "30s": 30 * time.Second, "10m": 10 * time.Minute, "2h": 2 * time.Hour} {
		if got, err := parseDuration(text); err != nil || got != want {
			t.Errorf("%s: got %v, %v", text, got, err)
		}
	}
}

func TestRunnerStatuses(t *testing.T) {
	var stdout, stderr bytes.Buffer
	r := &Runner{Stdout: &stdout, Stderr: &stderr}
	ctx := context.Background()
	if result := r.Run(ctx, Command{Argv: []string{"sh", "-c", "cat; echo err >&2; exit 7"}, Stdin: "in", Quiet: true}); result.Status != 7 || result.Stdout != "in" || result.Stderr != "err\n" {
		t.Errorf("got %+v", result)
	}
	if result := r.Run(ctx, Command{Argv: []string{"sh", "-c", "echo shown"}, Passthrough: true}); result.Status != 0 || stdout.String() != "shown\n" {
		t.Errorf("got %+v, %q", result, stdout.String())
	}
	if result := r.Run(ctx, Command{Argv: []string{"k8s-node-power-test-missing-command"}}); result.Status != 127 {
		t.Errorf("missing command: %+v", result)
	}
	started := time.Now()
	if result := r.Run(ctx, Command{Argv: []string{"sleep", "30"}, Timeout: 200 * time.Millisecond}); result.Status != 124 {
		t.Errorf("timeout: %+v", result)
	}
	if time.Since(started) > 10*time.Second {
		t.Error("timeout did not stop the command")
	}

	// Nothing starts after an interrupt.
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if result := r.Run(canceled, Command{Argv: []string{"sh", "-c", "echo started"}, Passthrough: true}); result.Status != statusNotStarted || stdout.String() != "shown\n" {
		t.Errorf("after interrupt: %+v", result)
	}
	fake := &Runner{Stdout: &stdout, Stderr: &stderr, Fake: func(Command) Result {
		t.Error("fake ran after an interrupt")
		return Result{}
	}}
	if result := fake.Run(canceled, Command{Argv: []string{"kubectl"}}); result.Status != statusNotStarted {
		t.Errorf("fake after interrupt: %+v", result)
	}
}
