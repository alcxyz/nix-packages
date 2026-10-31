package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestParseInventory(t *testing.T) {
	inv, err := parseInventory(readFixture(t, "inventory.json"))
	if err != nil {
		t.Fatal(err)
	}
	if inv.EnvironmentID != "env-example" || len(inv.Threads) != 5 {
		t.Fatalf("got env %q and %d threads", inv.EnvironmentID, len(inv.Threads))
	}
	b2 := inv.Threads[1]
	if b2.ThreadID != "thread-b2" || len(b2.PRs) != 2 || b2.PRs[1].State != "merged" {
		t.Fatalf("thread-b2 parsed as %+v", b2)
	}
	if _, ok := b2.raw["extraField"]; !ok {
		t.Fatal("unknown inventory fields must be kept for the report")
	}
	if inv.Threads[2].WaitingOn != nil {
		t.Fatal("null waitingOn should stay nil")
	}
	if !inv.Threads[3].settled() || inv.Threads[3].SettledAt != "2026-10-20T10:00:00Z" {
		t.Fatalf("thread-d4 should be settled: %+v", inv.Threads[3])
	}
	if !inv.Threads[4].Acknowledged {
		t.Fatal("thread-e5 should be acknowledged")
	}
}

func TestParseInventoryRejectsBadIDs(t *testing.T) {
	for _, id := range []string{"", "-rf", "a b", "a;b", "a/b"} {
		data, _ := json.Marshal(map[string]any{"threads": []any{map[string]any{"threadId": id}}})
		if _, err := parseInventory(data); err == nil {
			t.Errorf("thread ID %q was accepted", id)
		}
	}
	if _, err := parseInventory([]byte("not json")); err == nil {
		t.Error("invalid JSON was accepted")
	}
}

func TestParseStatus(t *testing.T) {
	st, err := parseStatus(readFixture(t, "status.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Threads) != 5 {
		t.Fatalf("got %d threads", len(st.Threads))
	}
	d4 := st.Threads[3]
	if d4.Verdict != "check" || d4.Settle == nil || d4.Settle.Line == "" {
		t.Fatalf("thread-d4 status: %+v", d4)
	}
	if got := leftForYouList(d4.Settle.LeftForYou); len(got) != 1 || got[0] != "Run the importer once" {
		t.Fatalf("left for you: %v", got)
	}
	if got := leftForYouList(json.RawMessage(`"Merge the PR"`)); len(got) != 1 {
		t.Fatalf("a single left-for-you string: %v", got)
	}
	if st.Threads[0].IdleMinutes == nil || *st.Threads[0].IdleMinutes != 180 {
		t.Fatal("idleMinutes not parsed")
	}
}

func TestParseMessages(t *testing.T) {
	data := strings.ReplaceAll(string(readFixture(t, "messages.json")), "__THREAD__", "thread-a1")
	m, err := parseMessages([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if m.Revision == "" || m.LastActivityAt != "2026-10-20T09:00:00.000Z" || m.MessageCount != 40 || len(m.Messages) != 3 {
		t.Fatalf("parsed %+v", m)
	}
	empty := strings.ReplaceAll(string(readFixture(t, "messages-empty.json")), "__THREAD__", "thread-a1")
	m, err = parseMessages([]byte(empty))
	if err != nil {
		t.Fatal(err)
	}
	if p := probeOf("x", m, testClock, 0); p.revision != "" {
		t.Fatalf("a thread without messages has no revision to summarise: %+v", p)
	}
}

func TestParseGrouped(t *testing.T) {
	g, err := parseGrouped(readFixture(t, "group.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Sections) != 4 || g.Sections[0].Key != "you" {
		t.Fatalf("sections: %+v", g.Sections)
	}
	row := g.Sections[0].Threads[0]
	if row.ThreadID != "thread-a1" || row.Group != "user" || row.Owner != "user" || row.ActionKind != "decide" {
		t.Fatalf("row: %+v", row)
	}
	if row.ProgressPct == nil || *row.ProgressPct != 60 || row.Facts.Verdict != "waiting" {
		t.Fatalf("row summary or facts: %+v", row)
	}
	if g.Summary.Unsettled != 4 || g.Summary.UnwatchedChecks != 1 || g.Summary.AvgPct == nil || *g.Summary.AvgPct != 27 {
		t.Fatalf("summary: %+v", g.Summary)
	}
	if row.UnwatchedChecks != nil {
		t.Fatalf("a row without the field has unwatched checks: %v", row.UnwatchedChecks)
	}
	agent := g.Sections[3].Threads
	if len(agent) != 2 || agent[1].ThreadID != "thread-f6" || len(agent[1].UnwatchedChecks) != 1 {
		t.Fatalf("flagged row: %+v", agent)
	}
	var nullRow GroupRow
	if err := json.Unmarshal([]byte(`{"threadId":"x","unwatchedChecks":null}`), &nullRow); err != nil || nullRow.UnwatchedChecks != nil {
		t.Fatalf("a null field: %v %v", nullRow.UnwatchedChecks, err)
	}
	if _, err := parseGrouped([]byte(`{"sections":[{"title":"x"}]}`)); err == nil {
		t.Fatal("a section without a key was accepted")
	}
}

func TestAckArgs(t *testing.T) {
	args, err := ackArgs([]string{"thread-d4", "a:b%2F.c"}, []string{"2026-10-20T10:00:00Z", "2026-10-20T10:00:00.123Z"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(args, " ") != "thread-d4@2026-10-20T10:00:00Z a:b%2F.c@2026-10-20T10:00:00.123Z" {
		t.Fatalf("args: %v", args)
	}
	bad := []struct{ id, at string }{
		{"", "2026-10-20"},
		{"-x", "2026-10-20"},
		{"a b", "2026-10-20"},
		{"a@b", "2026-10-20"},
		{"a;rm", "2026-10-20"},
		{"ok", ""},
		{"ok", "x2026"},
		{"ok", "2026-10-20 10:00"},
		{"ok", "2026;id"},
	}
	for _, b := range bad {
		if _, err := ackArgs([]string{b.id}, []string{b.at}); err == nil {
			t.Errorf("ackArgs accepted %q@%q", b.id, b.at)
		}
	}
	if _, err := ackArgs([]string{"a", "b"}, []string{"2026"}); err == nil {
		t.Error("mismatched lists were accepted")
	}
	if _, err := ackArgs(nil, nil); err == nil {
		t.Error("an empty ack was accepted")
	}

	args, err = forgetArgs([]string{"thread-e5"})
	if err != nil || strings.Join(args, " ") != "--forget thread-e5" {
		t.Fatalf("forget args %v, %v", args, err)
	}
	if _, err := forgetArgs([]string{"--list"}); err == nil {
		t.Error("forget accepted an option as an ID")
	}
}

func TestCommandsPassArgumentsSeparately(t *testing.T) {
	d, argsLog := newTestDeck(t)
	in := d.cfg.Threads.Instances[0]
	ctx := context.Background()
	if _, err := d.cmds.inventory(ctx, in, 3); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.cmds.status(ctx, in, []string{"thread-a1", "thread-b2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.cmds.messages(ctx, in, "thread-a1", 3, 30, 1500); err != nil {
		t.Fatal(err)
	}
	if err := d.cmds.ack(ctx, []string{"thread-d4@2026-10-20T10:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"inventory --days 3 --db /var/lib/example/state.sqlite",
		"status --db /var/lib/example/state.sqlite thread-a1 thread-b2",
		"messages --db /var/lib/example/state.sqlite --head 3 --tail 30 --max-chars 1500 thread-a1",
		"ack thread-d4@2026-10-20T10:00:00Z",
	}
	got := readArgs(t, argsLog)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("args:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if _, err := d.cmds.messages(ctx, in, "-x", 1, 1, 1); err == nil {
		t.Fatal("messages accepted an option as a thread ID")
	}
}

func TestCommandFailureAndTimeout(t *testing.T) {
	d, _ := newTestDeck(t)
	in := d.cfg.Threads.Instances[0]
	t.Setenv("DECK_FAIL", "1")
	_, err := d.cmds.inventory(context.Background(), in, 3)
	if err == nil || !strings.Contains(err.Error(), "inventory failed on purpose") {
		t.Fatalf("error should carry the command's message: %v", err)
	}

	dir := t.TempDir()
	slow := filepath.Join(dir, "slow")
	if err := os.WriteFile(slow, []byte("#!/bin/sh\nexec sleep 5\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := Commands{cfg: CommandsConfig{Inventory: slow}, timeout: 100 * time.Millisecond}
	start := time.Now()
	if _, err := c.inventory(context.Background(), in, 3); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("slow command: %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("timeout did not stop the command")
	}
}

func TestTimeoutKillsChildrenHoldingOutput(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	script := filepath.Join(dir, "spawn")
	// The child keeps stdout open and would outlive its parent.
	body := "#!/bin/sh\nsleep 30 &\necho $! > " + pidFile + "\nexec sleep 30\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	c := Commands{cfg: CommandsConfig{Inventory: script}, timeout: 300 * time.Millisecond}
	start := time.Now()
	_, err := c.inventory(context.Background(), InstanceConfig{DB: "x"}, 3)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("the timeout took %s", time.Since(start))
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	deadline := time.Now().Add(2 * time.Second)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			syscall.Kill(pid, syscall.SIGKILL)
			t.Fatal("the child outlived the timeout")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// processAlive reports whether pid still runs. A killed child whose parent
// died is reparented to PID 1, and in a CI container PID 1 may never reap it,
// so a zombie counts as gone.
func processAlive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	// The state follows the parenthesised command name.
	i := strings.LastIndexByte(string(stat), ')')
	return i < 0 || i+2 >= len(stat) || stat[i+2] != 'Z'
}

func waitForPID(t *testing.T, pidFile string) int {
	t.Helper()
	deadline := time.Now().Add(hangGuard)
	for {
		data, err := os.ReadFile(pidFile)
		if pid, convErr := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && convErr == nil {
			return pid
		}
		if time.Now().After(deadline) {
			t.Fatal("no child PID")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSuccessfulRunLeavesTheGroupAlone(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	// The child releases the output, so the command ends on its own.
	script := writeScript(t, dir, "spawn", "sleep 30 >/dev/null 2>&1 </dev/null &\necho $! > "+pidFile+"\n")
	c := Commands{cfg: CommandsConfig{Inventory: script}, timeout: hangGuard}
	_, err := c.inventory(context.Background(), InstanceConfig{DB: "x"}, 3)
	pid := waitForPID(t, pidFile)
	defer syscall.Kill(pid, syscall.SIGKILL)
	if err == nil || !strings.Contains(err.Error(), "parse inventory") {
		t.Fatalf("err %v", err)
	}
	// Give a kill, if one was sent, time to take effect.
	time.Sleep(200 * time.Millisecond)
	if !processAlive(pid) {
		t.Fatal("the group was killed after the command ended on its own")
	}
}

func TestHeldOutputKillsTheGroup(t *testing.T) {
	old := killGrace
	killGrace = 200 * time.Millisecond
	defer func() { killGrace = old }()
	// The command exits at once, successfully or not, but its child keeps
	// stdout open.
	for name, exit := range map[string]string{"success": "exit 0", "failure": "exit 1"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			pidFile := filepath.Join(dir, "child.pid")
			script := writeScript(t, dir, "spawn", "sleep 30 &\necho $! > "+pidFile+"\n"+exit+"\n")
			c := Commands{cfg: CommandsConfig{Inventory: script}, timeout: hangGuard}
			start := time.Now()
			_, err := c.inventory(context.Background(), InstanceConfig{DB: "x"}, 3)
			if err == nil {
				t.Fatal("expected an error")
			}
			if exit == "exit 0" && !errors.Is(err, exec.ErrWaitDelay) {
				t.Fatalf("err %v", err)
			}
			if time.Since(start) > 3*time.Second {
				t.Fatalf("waited %s", time.Since(start))
			}
			pid := waitForPID(t, pidFile)
			deadline := time.Now().Add(2 * time.Second)
			for processAlive(pid) {
				if time.Now().After(deadline) {
					syscall.Kill(pid, syscall.SIGKILL)
					t.Fatal("the child holding the output outlived the command")
				}
				time.Sleep(20 * time.Millisecond)
			}
		})
	}
}

func TestCallerDeadlineIsNotReportedAsTimeout(t *testing.T) {
	script := writeScript(t, t.TempDir(), "slow", "sleep 5\n")
	c := Commands{cfg: CommandsConfig{Inventory: script}, timeout: hangGuard}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := c.inventory(ctx, InstanceConfig{DB: "x"}, 3)
	if err == nil || strings.Contains(err.Error(), "timed out after") || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v", err)
	}
}
