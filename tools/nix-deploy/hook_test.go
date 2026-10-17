package main

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// hookEvents is each recorded hook call as "event host outcome".
func (f *rebootFake) hookEvents() []string {
	var events []string
	for _, call := range f.hooks {
		fields := map[string]string{}
		_, env, _ := strings.Cut(call, ": ")
		for _, pair := range strings.Split(env, " NIXFLEET_") {
			name, value, _ := strings.Cut(strings.TrimPrefix(pair, "NIXFLEET_"), "=")
			fields[name] = value
		}
		event := fields["EVENT"]
		switch event {
		case hookRunStart:
			event += " " + fields["HOSTS"]
		case hookHostStart:
			event += " " + fields["HOST"] + " " + fields["KIND"]
		case hookHostFinish:
			event += " " + fields["HOST"] + " " + fields["OUTCOME"]
		case hookRunFinish:
			event += " failed=" + fields["FAILED"]
		}
		events = append(events, event)
	}
	return events
}

// A run that changes hosts calls the hook at its start and end and around
// every host, in pairs, with the hook's command and the event's variables.
func TestRebootHookEvents(t *testing.T) {
	f := newRebootFake(t)
	f.inv.RebootHookCommand = "notify --quiet"
	if status := f.run("server-a", "server-b", "host-a", "--yes"); status != 0 {
		t.Fatalf("status %d:\n%s", status, f.out.String())
	}
	events := f.hookEvents()
	if events[0] != "run-start server-a server-b host-a" && events[0] != "run-start server-b server-a host-a" {
		t.Errorf("first event %q", events[0])
	}
	if events[len(events)-1] != "run-finish failed=false" {
		t.Errorf("last event %q", events[len(events)-1])
	}
	for _, want := range []string{"host-start server-a node", "host-finish server-a rebooted", "host-start server-b node",
		"host-finish server-b rebooted", "host-start host-a host", "host-finish host-a rebooted"} {
		if !slices.Contains(events, want) {
			t.Errorf("no %q in %v", want, events)
		}
	}
	// The nodes take turns: a node finishes before the next starts.
	a, b := slices.Index(events, "host-finish server-a rebooted"), slices.Index(events, "host-start server-b node")
	c, d := slices.Index(events, "host-finish server-b rebooted"), slices.Index(events, "host-start server-a node")
	if !(a < b || c < d) {
		t.Errorf("node events overlap: %v", events)
	}
	for _, call := range f.hooks {
		if strings.Contains(call, "HOST=server-a") && !strings.Contains(call, "NIXFLEET_GROUP=serial-1") {
			t.Errorf("no serial group in %q", call)
		}
	}
	if !strings.HasPrefix(f.hooks[0], "notify --quiet: NIXFLEET_EVENT=run-start") {
		t.Errorf("call %q", f.hooks[0])
	}
	finish := ""
	for _, call := range f.hooks {
		if strings.Contains(call, "EVENT=host-finish") && strings.Contains(call, "HOST=host-a") {
			finish = call
		}
	}
	for _, want := range []string{"NIXFLEET_DURATION=", "NIXFLEET_GROUP= ", "NIXFLEET_KIND=host", "NIXFLEET_OUTCOME=rebooted", "NIXFLEET_REASON="} {
		if !strings.Contains(finish, want) {
			t.Errorf("no %q in %q", want, finish)
		}
	}
	if !strings.Contains(f.hooks[len(f.hooks)-1], "NIXFLEET_SUMMARY=") {
		t.Errorf("no summary in %q", f.hooks[len(f.hooks)-1])
	}
}

// A failed host still finishes, with its reason, and the run's end says one
// failed; a hook that fails is only a warning.
func TestRebootHookFailures(t *testing.T) {
	f := newRebootFake(t)
	f.inv.RebootHookCommand = "notify"
	f.hosts["host-a"].rebootFails = true
	f.hookFails = true
	if status := f.run("host-a", "host-b", "--yes"); status != 1 {
		t.Fatalf("status %d", status)
	}
	f.expectOutcomes(map[string]string{"host-a": "failed", "host-b": "rebooted"})
	events := f.hookEvents()
	for _, want := range []string{"host-start host-a host", "host-finish host-a failed", "run-finish failed=true"} {
		if !slices.Contains(events, want) {
			t.Errorf("no %q in %v", want, events)
		}
	}
	for _, call := range f.hooks {
		if strings.Contains(call, "HOST=host-a") && strings.Contains(call, "OUTCOME=failed") && !strings.Contains(call, "REASON=") {
			t.Errorf("no reason in %q", call)
		}
	}
	if !strings.Contains(f.errOut.String(), "nixfleet reboot: warning: the reboot hook failed on host-start: exit status 1: no route to the chat\n") {
		t.Errorf("no warning in:\n%s", f.errOut.String())
	}
}

// An interrupted host still finishes, so whatever its start paused is
// resumed.
func TestRebootHookInterrupted(t *testing.T) {
	f := newRebootFake(t)
	f.inv.RebootHookCommand = "notify"
	f.stopCancels = true
	f.run("host-a", "--yes")
	events := f.hookEvents()
	if !slices.Contains(events, "host-start host-a host") || !slices.Contains(events, "host-finish host-a failed") {
		t.Errorf("events: %v", events)
	}
}

// Nothing calls the hook without one, in a check-only or dry run, or when
// no host is rebooted.
func TestRebootHookNotCalled(t *testing.T) {
	cases := []struct {
		name string
		hook string
		args []string
		edit func(*rebootFake)
	}{
		{"no hook", "", []string{"host-a", "--yes"}, nil},
		{"check only", "notify", []string{"host-a", "--check-only"}, nil},
		{"dry run", "notify", []string{"host-a", "--dry-run"}, nil},
		{"nothing to reboot", "notify", []string{"host-a", "--needed", "--yes"}, func(f *rebootFake) { f.hosts["host-a"].verdict = "no" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newRebootFake(t)
			f.inv.RebootHookCommand = c.hook
			if c.edit != nil {
				c.edit(f)
			}
			f.run(c.args...)
			if len(f.hooks) != 0 {
				t.Errorf("hook calls: %v", f.hooks)
			}
		})
	}
}

// This machine only starts in the run: its finish comes after its boot. The
// run's end comes before, with the summary.
func TestRebootHookSelf(t *testing.T) {
	f := newRebootFake(t)
	f.interactive()
	f.inv.InteractiveHosts = append(f.inv.InteractiveHosts, "op")
	f.inv.RebootHookCommand = "notify"
	f.terminal = true
	f.confirmation = "y\n"
	if status := f.run("op", "host-b", "--include-self", "--yes"); status != 0 {
		t.Fatalf("status %d:\n%s", status, f.out.String())
	}
	events := f.hookEvents()
	if events[len(events)-1] != "host-start op self" || events[len(events)-2] != "run-finish failed=false" {
		t.Errorf("events: %v", events)
	}
}

// The hook command runs through sh with the event in its environment, and
// its failure carries its last line.
func TestRunHookCommand(t *testing.T) {
	if err := runHookCommand(context.Background(), `test "$NIXFLEET_EVENT" = run-start`, []string{"NIXFLEET_EVENT=run-start"}); err != nil {
		t.Errorf("event not in the environment: %v", err)
	}
	err := runHookCommand(context.Background(), "echo first; echo 'no chat' >&2; exit 3", nil)
	if err == nil || !strings.Contains(err.Error(), "exit status 3: no chat") {
		t.Errorf("failure: %v", err)
	}
}

// The deadline stops every process the hook started, so none acts late.
func TestRunHookCommandStopsItsProcesses(t *testing.T) {
	marker := t.TempDir() + "/late"
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := runHookCommand(ctx, "(sleep 1; touch "+marker+") & wait", nil); err == nil {
		t.Fatal("the hook did not time out")
	}
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Error("a process the hook started acted after its deadline")
	}
}

// The deadline bounds the hook even when a helper it started keeps its
// output open.
func TestRunHookCommandDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := runHookCommand(ctx, "sleep 30 & sleep 30", nil)
	if err == nil || !strings.Contains(err.Error(), "did not finish within") {
		t.Errorf("err %v", err)
	}
	if elapsed := time.Since(start); elapsed > killGrace+5*time.Second {
		t.Errorf("took %s", elapsed)
	}
}

// When this machine's reboot does not happen, its turn finishes in the
// run; an interrupt during its start leaves its runners running.
func TestRebootHookSelfFailures(t *testing.T) {
	setup := func(t *testing.T) *rebootFake {
		f := newRebootFake(t)
		f.interactive()
		f.inv.InteractiveHosts = append(f.inv.InteractiveHosts, "op")
		f.hosts["op"].runners = map[string]string{"forgejo-actions-runner.service": "active"}
		f.inv.RebootHookCommand = "notify"
		f.terminal = true
		f.confirmation = "y\n"
		return f
	}
	f := setup(t)
	f.hosts["op"].rebootFails = true
	f.run("op", "host-b", "--include-self", "--yes")
	if events := f.hookEvents(); events[len(events)-1] != "host-finish op failed" {
		t.Errorf("events: %v", events)
	}

	f = setup(t)
	f.hookCancels = "NIXFLEET_KIND=self"
	f.run("op", "host-b", "--include-self", "--yes")
	if events := f.hookEvents(); events[len(events)-1] != "host-finish op failed" {
		t.Errorf("events: %v", events)
	}
	if slices.Contains(f.changes(), "reboot op") || slices.Contains(f.changes(), "stop op") {
		t.Errorf("changes: %v", f.changes())
	}
	// Terminated while the reboot command runs, which may be the shutdown
	// itself, it does not finish.
	f = setup(t)
	f.rebootTerminates = true
	f.run("op", "--include-self", "--yes")
	if events := f.hookEvents(); events[len(events)-1] != "host-start op self" {
		t.Errorf("events: %v", events)
	}

	// The operator's interrupt then leaves it up: its runners start again
	// and its turn finishes.
	f = setup(t)
	f.rebootCancels = true
	f.run("op", "--include-self", "--yes")
	if events := f.hookEvents(); events[len(events)-1] != "host-finish op failed" {
		t.Errorf("events: %v", events)
	}
	if !slices.Contains(f.changes(), "start op") {
		t.Errorf("changes: %v", f.changes())
	}

	// A queued reboot stands, whatever interrupts after it: its turn ends
	// with the reboot.
	f = setup(t)
	f.rebootQueuedCancels = true
	f.run("op", "--include-self", "--yes")
	if events := f.hookEvents(); events[len(events)-1] != "host-start op self" || slices.Contains(f.changes(), "start op") {
		t.Errorf("events: %v, changes %v", events, f.changes())
	}

	// Interrupted while its runners stop, it starts them again and does
	// not reboot.
	f = setup(t)
	f.stopCancels = true
	f.run("op", "host-b", "--include-self", "--yes")
	changes := f.changes()
	if slices.Contains(changes, "reboot op") || !slices.Contains(changes, "start op") {
		t.Errorf("changes: %v", changes)
	}
}

// A run whose only group fails before it starts has neither a start nor a
// finish, though this machine was asked for.
func TestRebootHookRunEventsPair(t *testing.T) {
	f := newRebootFake(t)
	f.interactive()
	f.inv.InteractiveHosts = append(f.inv.InteractiveHosts, "op")
	f.inv.RebootHookCommand = "notify"
	f.terminal = true
	f.confirmation = "y\n"
	f.hosts["host-b"].unreachable = true
	f.run("op", "host-b", "--include-self", "--yes")
	if len(f.hooks) != 0 {
		t.Errorf("hook calls: %v", f.hookEvents())
	}
}

// A process the hook started that ignores SIGTERM still ends with the hook.
func TestRunHookCommandKillsStubbornProcesses(t *testing.T) {
	marker := t.TempDir() + "/late"
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	runHookCommand(ctx, "(trap '' TERM; sleep 7; touch "+marker+") & wait", nil)
	time.Sleep(2500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Error("a process ignoring SIGTERM acted after the hook")
	}
}
