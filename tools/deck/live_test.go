package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// liveTestAlerts is a pages-only deck with one tile page, live/runners, at
// the given status, and alerts that record each command instead of running
// it, failing while fail is set.
type liveTestAlerts struct {
	t     *testing.T
	d     *Deck
	a     *liveAlerts
	clock time.Time
	sent  [][]string
	fail  bool
}

func newLiveTestAlerts(t *testing.T, status string) *liveTestAlerts {
	t.Helper()
	d := newPagesDeck(t)
	d.cfg.Live = &LiveConfig{
		Notify:    []string{"/bin/notify", "--class", "live", "{text}", "--link", "{link}", "{text}x"},
		StateFile: filepath.Join(t.TempDir(), "state", "deck", "live.json"),
	}
	l := &liveTestAlerts{t: t, d: d, clock: testClock}
	d.now = func() time.Time { return l.clock }
	writeTilePage(t, d.cfg.ContentDir, "live/runners", "Runners", "2026-10-02")
	l.status(status)
	l.restart()
	return l
}

func (l *liveTestAlerts) status(status string) {
	l.t.Helper()
	writeTileStatus(l.t, l.d.cfg.ContentDir, "live/runners", status)
}

// restart starts the alerts afresh, as a new process would.
func (l *liveTestAlerts) restart() {
	l.a = newLiveAlerts(l.d)
	l.a.run = func(ctx context.Context, argv []string) error {
		l.sent = append(l.sent, argv)
		if l.fail {
			return errors.New("exit status 1")
		}
		return nil
	}
}

// pass advances the clock and runs one pass, and reports how many commands
// it ran.
func (l *liveTestAlerts) pass(after time.Duration) int {
	l.clock = l.clock.Add(after)
	before := len(l.sent)
	l.a.pass(context.Background())
	return len(l.sent) - before
}

func (l *liveTestAlerts) recorded() liveState {
	l.t.Helper()
	data, err := os.ReadFile(l.d.cfg.Live.StateFile)
	if err != nil {
		l.t.Fatal(err)
	}
	var state liveState
	if err := json.Unmarshal(data, &state); err != nil {
		l.t.Fatal(err)
	}
	return state
}

func TestLiveAlertTransitions(t *testing.T) {
	l := newLiveTestAlerts(t, "ok")
	if n := l.pass(0); n != 0 {
		t.Fatalf("first pass sent %d", n)
	}
	l.status("act")
	if n := l.pass(time.Minute); n != 1 {
		t.Fatalf("ok to act sent %d, want 1", n)
	}
	want := []string{"/bin/notify", "--class", "live", "Runners needs action: Status act", "--link", "https://deck.example.com/live/runners/", "{text}x"}
	if strings.Join(l.sent[0], "|") != strings.Join(want, "|") {
		t.Errorf("argv:\n got %q\nwant %q", l.sent[0], want)
	}
	if n := l.pass(time.Minute); n != 0 {
		t.Errorf("staying act sent %d", n)
	}
	// Back to ok and to act within the hour: no second alert.
	l.status("ok")
	l.pass(time.Minute)
	l.status("act")
	if n := l.pass(time.Minute); n != 0 {
		t.Errorf("act again within the hour sent %d", n)
	}
	// After the hour a new turn to act alerts again.
	l.status("ok")
	l.pass(time.Hour)
	l.status("act")
	if n := l.pass(time.Minute); n != 1 {
		t.Errorf("act again after the hour sent %d, want 1", n)
	}
	// An unreadable tile sends nothing and leaves the record as it was.
	l.status("ok")
	l.pass(2 * time.Hour)
	writeContentFile(t, l.d.cfg.ContentDir, "live/runners/tile.json", `{`)
	if n := l.pass(time.Minute); n != 0 {
		t.Errorf("an unreadable tile sent %d", n)
	}
	if got := l.recorded().Pages["/live/runners/"].Status; got != tileOK {
		t.Errorf("recorded status %q, want ok", got)
	}
}

// stale writes the page's tile.json at the status, made two hours before
// the clock, so it is stale.
func (l *liveTestAlerts) stale(status string) {
	l.t.Helper()
	writeContentFile(l.t, l.d.cfg.ContentDir, "live/runners/tile.json", `{"status": "`+status+`", "headline": "Old", "updated": "`+
		l.clock.Add(-2*time.Hour).Format(time.RFC3339)+`", "staleAfter": 15}`)
}

func TestLiveAlertStale(t *testing.T) {
	l := newLiveTestAlerts(t, "ok")
	l.pass(0)
	// A turn to act on stale data is no turn.
	l.stale("act")
	if n := l.pass(time.Minute); n != 0 {
		t.Errorf("stale act sent %d", n)
	}
	if got := l.recorded().Pages["/live/runners/"].Status; got != tileOK {
		t.Errorf("stale data changed the record to %q", got)
	}
	// A failed alert is not retried while the data is stale.
	l.status("act")
	l.fail = true
	if n := l.pass(time.Minute); n != 1 {
		t.Fatalf("fresh act: %d", n)
	}
	l.fail = false
	l.stale("act")
	if n := l.pass(11 * time.Minute); n != 0 {
		t.Errorf("retried on stale data: %d", n)
	}
	if rec := l.recorded().Pages["/live/runners/"]; !rec.Pending {
		t.Errorf("the retry should wait for fresh data: %+v", rec)
	}
	l.status("act")
	if n := l.pass(time.Minute); n != 1 {
		t.Errorf("no retry once fresh and still act: %d", n)
	}
}

// TestLiveAlertBlips checks that a page alerted for act, then missing,
// unreadable or stale for a pass, then act again sends nothing more, within
// the hour or after it.
func TestLiveAlertBlips(t *testing.T) {
	for name, blip := range map[string]func(l *liveTestAlerts) func(){
		"unreadable bundle": func(l *liveTestAlerts) func() {
			writeContentFile(l.t, l.d.cfg.ContentDir, "live/runners/metadata.json", `{`)
			return func() {
				writeTilePage(l.t, l.d.cfg.ContentDir, "live/runners", "Runners", "2026-10-02")
				l.status("act")
			}
		},
		"unreadable contentDir": func(l *liveTestAlerts) func() {
			dir := l.d.cfg.ContentDir
			if err := os.Rename(dir, dir+".away"); err != nil {
				l.t.Fatal(err)
			}
			return func() {
				if err := os.Rename(dir+".away", dir); err != nil {
					l.t.Fatal(err)
				}
			}
		},
		"error tile": func(l *liveTestAlerts) func() {
			writeContentFile(l.t, l.d.cfg.ContentDir, "live/runners/tile.json", `{`)
			return func() { l.status("act") }
		},
		"missing tile": func(l *liveTestAlerts) func() {
			if err := os.Remove(filepath.Join(l.d.cfg.ContentDir, "live/runners/tile.json")); err != nil {
				l.t.Fatal(err)
			}
			return func() { l.status("act") }
		},
		"stale ok": func(l *liveTestAlerts) func() {
			l.stale("ok")
			return func() { l.status("act") }
		},
	} {
		t.Run(name, func(t *testing.T) {
			l := newLiveTestAlerts(t, "ok")
			l.t = t
			l.pass(0)
			l.status("act")
			if n := l.pass(time.Minute); n != 1 {
				t.Fatalf("ok to act sent %d", n)
			}
			// Within the hour the hourly limit holds; after it, only the
			// record kept through the blip stops a second alert.
			for _, gap := range []time.Duration{time.Minute, time.Hour} {
				restore := blip(l)
				if n := l.pass(gap); n != 0 {
					t.Errorf("the blip after %s sent %d", gap, n)
				}
				restore()
				if n := l.pass(time.Minute); n != 0 {
					t.Errorf("act after the blip after %s sent %d", gap, n)
				}
			}
		})
	}
}

func TestLiveAlertForget(t *testing.T) {
	l := newLiveTestAlerts(t, "act")
	l.pass(0)
	if err := os.RemoveAll(filepath.Join(l.d.cfg.ContentDir, "live/runners")); err != nil {
		t.Fatal(err)
	}
	l.pass(23 * time.Hour)
	if _, ok := l.recorded().Pages["/live/runners/"]; !ok {
		t.Error("a page absent for less than a day lost its record")
	}
	l.pass(2 * time.Hour)
	if _, ok := l.recorded().Pages["/live/runners/"]; ok {
		t.Error("a page absent for more than a day keeps its record")
	}
}

func TestLiveAlertCancelKeepsSends(t *testing.T) {
	l := newLiveTestAlerts(t, "ok")
	writeTilePage(t, l.d.cfg.ContentDir, "live/second", "Second", "2026-10-01")
	l.pass(0)
	l.status("act")
	writeTileStatus(t, l.d.cfg.ContentDir, "live/second", "act")
	// The first send succeeds; the second is cancelled under way.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var sent []string
	l.a.run = func(ctx context.Context, argv []string) error {
		sent = append(sent, argv[3])
		if len(sent) == 2 {
			cancel()
			return ctx.Err()
		}
		return nil
	}
	l.clock = l.clock.Add(time.Minute)
	l.a.pass(ctx)
	if len(sent) != 2 || !strings.HasPrefix(sent[0], "Runners") {
		t.Fatalf("sends: %q", sent)
	}
	if rec := l.recorded().Pages["/live/runners/"]; rec.SentAt.IsZero() || rec.Status != tileAct {
		t.Errorf("the earlier send was not saved: %+v", rec)
	}
	// After a restart only the cancelled alert goes out.
	l.restart()
	if n := l.pass(time.Minute); n != 1 || !strings.HasPrefix(l.sent[0][3], "Second") {
		t.Errorf("after the cancelled pass: %d %q", n, l.sent)
	}
}

func TestLiveStateWithoutLastSeen(t *testing.T) {
	l := newLiveTestAlerts(t, "act")
	// A state file from before deck recorded when it last saw a page.
	writeContentFile(t, filepath.Dir(l.d.cfg.Live.StateFile), "live.json",
		`{"pages": {"/live/runners/": {"status": "act", "sentAt": "2026-10-20T11:00:00Z"}, "/live/gone/": {"status": "ok"}}}`)
	if n := l.pass(time.Minute); n != 0 {
		t.Errorf("recorded act sent %d", n)
	}
	pages := l.recorded().Pages
	if len(pages) != 2 || !pages["/live/runners/"].SentAt.Equal(time.Date(2026, 10, 20, 11, 0, 0, 0, time.UTC)) {
		t.Errorf("the old records should load and stay: %+v", pages)
	}
	l.pass(25 * time.Hour)
	if _, ok := l.recorded().Pages["/live/gone/"]; ok {
		t.Error("an old record of an absent page is never dropped")
	}
}

func TestLiveAlertRetry(t *testing.T) {
	l := newLiveTestAlerts(t, "ok")
	l.pass(0)
	l.status("act")
	l.fail = true
	if n := l.pass(time.Minute); n != 1 {
		t.Fatalf("first attempt: %d", n)
	}
	if rec := l.recorded().Pages["/live/runners/"]; !rec.Pending || rec.Status != tileAct {
		t.Errorf("a failed alert stays pending: %+v", rec)
	}
	if n := l.pass(5 * time.Minute); n != 0 {
		t.Errorf("retried within 10 minutes: %d", n)
	}
	l.fail = false
	if n := l.pass(5 * time.Minute); n != 1 {
		t.Errorf("no retry after 10 minutes: %d", n)
	}
	if n := l.pass(10 * time.Minute); n != 0 {
		t.Errorf("sent again after success: %d", n)
	}
	if rec := l.recorded().Pages["/live/runners/"]; rec.Pending || !rec.SentAt.Equal(l.clock.Add(-10*time.Minute)) {
		t.Errorf("the sent alert is recorded: %+v", rec)
	}
}

func TestLiveAlertFirstStartAndRestart(t *testing.T) {
	// No state file: record the statuses, send nothing.
	l := newLiveTestAlerts(t, "act")
	if n := l.pass(0); n != 0 {
		t.Fatalf("first start sent %d", n)
	}
	info, err := os.Stat(l.d.cfg.Live.StateFile)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("state file: %v %v", info, err)
	}
	if dir, err := os.Stat(filepath.Dir(l.d.cfg.Live.StateFile)); err != nil || dir.Mode().Perm() != 0o700 {
		t.Fatalf("state directory: %v %v", dir, err)
	}
	if got := l.recorded().Pages["/live/runners/"].Status; got != tileAct {
		t.Errorf("recorded %q, want act", got)
	}
	// A restart with act recorded sends nothing either.
	l.restart()
	if n := l.pass(time.Minute); n != 0 {
		t.Errorf("restart sent %d", n)
	}
	// With a state file, a new page that needs action alerts.
	writeTilePage(t, l.d.cfg.ContentDir, "live/new", "New", "2026-10-03")
	writeTileStatus(t, l.d.cfg.ContentDir, "live/new", "act")
	l.restart()
	if n := l.pass(time.Minute); n != 1 || !strings.HasPrefix(l.sent[0][3], "New needs action") {
		t.Errorf("a new page that needs action: %d %q", n, l.sent)
	}
	// A page that no longer exists keeps its record for a day.
	if err := os.RemoveAll(filepath.Join(l.d.cfg.ContentDir, "live/new")); err != nil {
		t.Fatal(err)
	}
	l.pass(time.Minute)
	if pages := l.recorded().Pages; len(pages) != 2 || pages["/live/new/"].Status != tileAct {
		t.Errorf("recorded pages: %+v", pages)
	}
}

func TestLiveText(t *testing.T) {
	if got := liveText("Runners", "Two queued."); got != "Runners needs action: Two queued." {
		t.Errorf("text: %q", got)
	}
	got := liveText("Runners", strings.Repeat("é", 600))
	if n := len([]rune(got)); n != 500 || !strings.HasSuffix(got, "é…") || !strings.HasPrefix(got, "Runners needs action: é") {
		t.Errorf("cut text: %d runes, %q", n, got)
	}
	if got := liveText("R", strings.Repeat("x", 500-len("R needs action: "))); len([]rune(got)) != 500 || strings.HasSuffix(got, "…") {
		t.Errorf("500 characters stay whole: %q", got)
	}
}

func TestRunNotify(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "args")
	script := filepath.Join(dir, "notify")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \""+out+"\"\necho noise\nexit \"$EXIT\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EXIT", "0")
	if err := runNotify(context.Background(), []string{script, "a b", "$HOME"}); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(out); string(data) != "a b\n$HOME\n" {
		t.Errorf("arguments reach the command as given, without a shell: %q", data)
	}
	t.Setenv("EXIT", "3")
	if err := runNotify(context.Background(), []string{script}); err == nil || err.Error() != "exit status 3" {
		t.Errorf("a failure reports its exit status only: %v", err)
	}
}

func TestLiveConfig(t *testing.T) {
	base := func() Config {
		return Config{PublicOrigin: "https://deck.example.com", Live: &LiveConfig{Notify: []string{"/bin/notify", "{text}"}}}
	}
	t.Setenv("XDG_STATE_HOME", "/state")
	cfg := base()
	if err := cfg.normalize(); err != nil || cfg.Live.StateFile != "/state/deck/live.json" {
		t.Errorf("default state file: %q %v", cfg.Live.StateFile, err)
	}
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "/home/u")
	cfg = base()
	if err := cfg.normalize(); err != nil || cfg.Live.StateFile != "/home/u/.local/state/deck/live.json" {
		t.Errorf("fallback state file: %q %v", cfg.Live.StateFile, err)
	}
	for _, tc := range []struct {
		name, err string
		change    func(*Config)
	}{
		{"no publicOrigin", "publicOrigin", func(c *Config) { c.PublicOrigin = "" }},
		{"no notify", "live.notify", func(c *Config) { c.Live.Notify = nil }},
		{"empty command", "live.notify", func(c *Config) { c.Live.Notify = []string{""} }},
		{"relative state file", "live.stateFile", func(c *Config) { c.Live.StateFile = "live.json" }},
		{"collections off", "live.notify needs the collections", func(c *Config) {
			off := false
			c.Collections = &off
			c.Threads.Instances = testConfig(t).Threads.Instances
		}},
	} {
		cfg := base()
		tc.change(&cfg)
		if err := cfg.normalize(); err == nil || !strings.Contains(err.Error(), tc.err) {
			t.Errorf("%s: %v", tc.name, err)
		}
	}

	path := filepath.Join(t.TempDir(), "deck.toml")
	if err := os.WriteFile(path, []byte(`publicOrigin = "https://deck.example.com"
[live]
notify = ["/bin/notify", "--class", "live", "{text}", "--link", "{link}"]
stateFile = "/var/lib/deck/live.json"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil || len(cfg.Live.Notify) != 6 || cfg.Live.StateFile != "/var/lib/deck/live.json" {
		t.Errorf("loaded [live]: %+v %v", cfg.Live, err)
	}
}
