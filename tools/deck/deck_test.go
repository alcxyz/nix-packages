package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func candidateKeys(d *Deck) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var keys []string
	for _, c := range d.candidatesLocked() {
		keys = append(keys, c.key)
	}
	return keys
}

func TestRefreshBuildsReportAndProbes(t *testing.T) {
	d, argsLog := newTestDeck(t)
	report := filepath.Join(t.TempDir(), "report.json")
	t.Setenv("DECK_REPORT_COPY", report)
	refreshAndProbe(d, d.cfg.Threads.Instances[0])

	d.mu.Lock()
	f := d.facts["main"]
	if f.err != "" || f.grouped == nil || len(f.inventory.Threads) != 5 || !f.asOf.Equal(testClock) {
		d.mu.Unlock()
		t.Fatalf("facts: err %q grouped %v", f.err, f.grouped != nil)
	}
	d.mu.Unlock()

	var r struct {
		GeneratedAt   string                       `json:"generatedAt"`
		WindowDays    int                          `json:"windowDays"`
		EnvironmentID string                       `json:"environmentId"`
		Threads       []map[string]json.RawMessage `json:"threads"`
	}
	data, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	if r.GeneratedAt == "" || r.WindowDays != 3 || r.EnvironmentID != "env-example" || len(r.Threads) != 5 {
		t.Fatalf("report header: %+v", r)
	}
	if _, ok := r.Threads[1]["extraField"]; !ok {
		t.Fatal("inventory rows must pass through unchanged")
	}
	if _, ok := r.Threads[0]["progressPct"]; ok {
		t.Fatal("a thread without a summary must not get summary fields")
	}

	// Probes run for threads that may need a summary: not for the
	// acknowledged thread-e5.
	var probed []string
	for _, line := range readArgs(t, argsLog) {
		if strings.HasPrefix(line, "messages ") {
			if !strings.Contains(line, "--head 0 --tail 0 --max-chars 0") {
				t.Errorf("a probe should ask for no message text: %s", line)
			}
			probed = append(probed, line[strings.LastIndex(line, " ")+1:])
		}
	}
	sort.Strings(probed)
	if strings.Join(probed, ",") != "thread-a1,thread-b2,thread-c3,thread-d4" {
		t.Fatalf("probed %v", probed)
	}
}

func TestEligibilityAndInvalidation(t *testing.T) {
	d, _ := newTestDeck(t)
	zero := 0 // the interval has its own tests
	d.cfg.Threads.SummaryIntervalMinutes = &zero
	refreshAndProbe(d, d.cfg.Threads.Instances[0])

	// The fixture's last activity is 09:00 and the clock 12:00, so every
	// probed thread is quiet; thread-e5 is acknowledged and needs nothing.
	want := "main/thread-a1,main/thread-b2,main/thread-c3,main/thread-d4"
	if got := strings.Join(candidateKeys(d), ","); got != want {
		t.Fatalf("candidates %s, want %s", got, want)
	}

	// Active within the quiet window: not yet, unless settled.
	d.now = func() time.Time { return time.Date(2026, 10, 20, 9, 5, 0, 0, time.UTC) }
	if got := strings.Join(candidateKeys(d), ","); got != "main/thread-d4" {
		t.Fatalf("within the quiet window only the settled thread is eligible, got %s", got)
	}
	d.now = func() time.Time { return testClock }

	// A summary made from the current revision is up to date.
	rev := d.probes["main/thread-a1"].revision
	if rev == "" {
		t.Fatal("probe has no revision")
	}
	if err := d.cache.put("main/thread-a1", CacheEntry{Basis: basisOf(rev, thread(t, d, "thread-a1")), AttemptBasis: basisOf(rev, thread(t, d, "thread-a1")), AttemptedAt: testClock}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(candidateKeys(d), ","), "thread-a1") {
		t.Fatal("an up-to-date thread was queued")
	}

	// A new revision makes it stale again, even when latest did not move.
	p := d.probes["main/thread-a1"]
	p.revision = "41:later"
	d.probes["main/thread-a1"] = p
	if !strings.Contains(strings.Join(candidateKeys(d), ","), "thread-a1") {
		t.Fatal("a changed revision did not queue the thread")
	}

	// No messages: nothing to summarise.
	p.revision = ""
	d.probes["main/thread-a1"] = p
	if strings.Contains(strings.Join(candidateKeys(d), ","), "thread-a1") {
		t.Fatal("a thread without messages was queued")
	}
}

func TestNeedsSummaryByFacts(t *testing.T) {
	cases := []struct {
		name string
		t    InventoryThread
		st   StatusThread
		want bool
	}{
		{"unsettled", InventoryThread{Bucket: "unsettled"}, StatusThread{Verdict: "waiting"}, true},
		{"acknowledged", InventoryThread{Bucket: "recently-settled", Acknowledged: true}, StatusThread{Verdict: "check"}, false},
		{"settled done", InventoryThread{Bucket: "recently-settled"}, StatusThread{State: "settled", Verdict: "done"}, false},
		{"settled left for you", InventoryThread{Bucket: "recently-settled"}, StatusThread{State: "settled", Verdict: "left-for-you"}, false},
		{"settled check", InventoryThread{Bucket: "recently-settled"}, StatusThread{State: "settled", Verdict: "check"}, true},
	}
	for _, c := range cases {
		if got := needsSummaryByFacts(c.t, c.st); got != c.want {
			t.Errorf("%s: got %v", c.name, got)
		}
	}
}

func TestProbeSeededFromCache(t *testing.T) {
	d, argsLog := newTestDeck(t)
	// A summary made at the thread's current updatedAt needs no new probe
	// after a restart.
	if err := d.cache.put("main/thread-a1", CacheEntry{Basis: Basis{Revision: "r"}, AttemptBasis: Basis{Revision: "r"}, ThreadUpdatedAt: "2026-10-20T09:00:00Z", AttemptedAt: testClock.Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	refreshAndProbe(d, d.cfg.Threads.Instances[0])
	for _, line := range readArgs(t, argsLog) {
		if strings.HasPrefix(line, "messages ") && strings.HasSuffix(line, " thread-a1") {
			t.Fatal("an unchanged thread was probed again")
		}
	}
}

func TestBudget(t *testing.T) {
	d, _ := newTestDeck(t)
	d.cfg.Threads.CallsPerHour = 3
	now := testClock
	d.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		if !d.takeCall() {
			t.Fatalf("call %d refused", i+1)
		}
		now = now.Add(10 * time.Minute)
	}
	if d.takeCall() {
		t.Fatal("a fourth call in the hour was allowed")
	}
	now = testClock.Add(61 * time.Minute) // the first call left the window
	if !d.takeCall() {
		t.Fatal("the rolling hour did not free a call")
	}
	d.mu.Lock()
	left := d.budgetLeftLocked()
	d.mu.Unlock()
	if left != 0 {
		t.Fatalf("budget left %d", left)
	}
}

func TestCallModelUsesBackupAndBudget(t *testing.T) {
	models := []ModelConfig{{Provider: "anthropic", Model: "a"}, {Provider: "anthropic", Model: "b"}}
	var calls []string
	run := func(ctx context.Context, m ModelConfig, prompt string) ([]byte, error) {
		calls = append(calls, m.Model)
		if m.Model == "a" {
			return nil, errors.New("primary down")
		}
		return []byte("ok"), nil
	}
	budget := 2
	allow := func() bool {
		if budget == 0 {
			return false
		}
		budget--
		return true
	}
	out, model, err := callModel(context.Background(), run, models, "p", allow)
	if err != nil || string(out) != "ok" || model != "anthropic/b" || strings.Join(calls, ",") != "a,b" {
		t.Fatalf("backup: %q %q %v %v", out, model, err, calls)
	}
	if _, _, err := callModel(context.Background(), run, models, "p", allow); !errors.Is(err, errBudget) {
		t.Fatalf("no budget: %v", err)
	}
}

func TestSummarizeStoresAndKeepsOldOnInvalid(t *testing.T) {
	d, _ := newTestDeck(t)
	in := d.cfg.Threads.Instances[0]
	refreshAndProbe(d, in)

	var prompts []string
	answer := validSummary
	d.runModel = func(ctx context.Context, m ModelConfig, prompt string) ([]byte, error) {
		prompts = append(prompts, prompt)
		return []byte(answer), nil
	}
	d.mu.Lock()
	_, thread, st, _ := d.findThreadLocked("main/thread-a1")
	d.mu.Unlock()
	if err := d.summarizeThread(context.Background(), in, thread, st, ""); err != nil {
		t.Fatal(err)
	}
	e, ok := d.cache.get("main/thread-a1")
	if !ok || e.Summary == nil || e.Basis.Revision != "40:2026-10-20T09:00:00.000Z" || e.Basis.Bucket != "unsettled" || e.Model != "anthropic/light-model" || e.Error != "" {
		t.Fatalf("entry %+v", e)
	}
	if !strings.Contains(prompts[0], "Pick a rollout window") || !strings.Contains(prompts[0], "Roll out the widget service") {
		t.Fatal("the prompt lacks the thread's facts")
	}

	// Invalid output keeps the summary and records the error against the
	// revision, so the thread is not retried until it changes. The same
	// revision is summarised again only by hand.
	answer = `{"progressPct": 10}`
	if err := d.summarizeThread(context.Background(), in, thread, st, summaryRole); err == nil {
		t.Fatal("invalid output was accepted")
	}
	e2, _ := d.cache.get("main/thread-a1")
	if e2.Summary == nil || e2.Summary.Goal != e.Summary.Goal || e2.Error == "" || e2.AttemptBasis != e.Basis {
		t.Fatalf("after invalid output: %+v", e2)
	}

	// The summary reaches the next report.
	report, _, err := d.buildReport(in, d.facts["main"].inventory)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(report), `"goal":"Build the widget"`) {
		t.Fatal("the cached summary is missing from the report")
	}
}

func TestSummarizeSkipsEmptyThread(t *testing.T) {
	d, _ := newTestDeck(t)
	abs, err := filepath.Abs(filepath.Join("testdata", "messages-empty.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DECK_MESSAGES", abs)
	refreshAndProbe(d, d.cfg.Threads.Instances[0])
	if keys := candidateKeys(d); len(keys) != 0 {
		t.Fatalf("threads without messages were queued: %v", keys)
	}
}

func TestRequestSummaryQueuesFirst(t *testing.T) {
	d, _ := newTestDeck(t)
	refreshAndProbe(d, d.cfg.Threads.Instances[0])
	d.mu.Lock()
	d.queue = []string{"main/thread-b2", "main/thread-c3"}
	d.mu.Unlock()
	if err := d.requestSummary("main", "thread-c3", summaryRole); err != nil {
		t.Fatal(err)
	}
	if err := d.requestSummary("main", "nope", summaryRole); err == nil {
		t.Fatal("an unknown thread was queued")
	}
	d.schedule()
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.queue[0] != "main/thread-c3" || d.manual["main/thread-c3"] == "" {
		t.Fatalf("queue %v", d.queue)
	}
}

func TestFactsErrorKeepsLastGood(t *testing.T) {
	d, _ := newTestDeck(t)
	in := d.cfg.Threads.Instances[0]
	refreshAndProbe(d, in)
	t.Setenv("DECK_FAIL", "1")
	d.now = func() time.Time { return testClock.Add(time.Minute) }
	refreshAndProbe(d, in)
	v := d.view()
	if v.Instances[0].Err == "" || !v.Instances[0].AsOf.Equal(testClock) || len(v.Sections) == 0 {
		t.Fatalf("view after a failed refresh: %+v", v.Instances[0])
	}
}

func TestNoRoleQueuesNothing(t *testing.T) {
	d, _ := newTestDeck(t)
	d.roleErr = errors.New("no shared llm config")
	d.models = nil
	refreshAndProbe(d, d.cfg.Threads.Instances[0])
	d.mu.Lock()
	n := len(d.queue)
	d.mu.Unlock()
	if n != 0 {
		t.Fatalf("queued %d threads without a model role", n)
	}
	if err := d.requestSummary("main", "thread-a1", summaryRole); err == nil {
		t.Fatal("a refresh was accepted without a model role")
	}
}

// thread returns a thread of the published facts.
func thread(t *testing.T, d *Deck, id string) InventoryThread {
	t.Helper()
	got, ok := d.thread("main", id)
	if !ok {
		t.Fatalf("no thread %s", id)
	}
	return got
}

func TestSettlingInvalidatesSummary(t *testing.T) {
	d, _ := newTestDeck(t)
	zero := 0
	d.cfg.Threads.SummaryIntervalMinutes = &zero
	in := d.cfg.Threads.Instances[0]
	refreshAndProbe(d, in)
	var prompts []string
	d.runModel = func(ctx context.Context, m ModelConfig, prompt string) ([]byte, error) {
		prompts = append(prompts, prompt)
		if strings.Contains(prompt, `"settled": true`) {
			return []byte(strings.Replace(validSummary, `"verdict": null`, `"verdict": "follow-up"`, 1)), nil
		}
		return []byte(validSummary), nil
	}
	d.mu.Lock()
	_, a1, st, _ := d.findThreadLocked("main/thread-a1")
	d.mu.Unlock()
	if err := d.summarizeThread(context.Background(), in, a1, st, ""); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(candidateKeys(d), ","), "thread-a1") {
		t.Fatal("a freshly summarised thread was queued")
	}

	// The thread settles without a new message: same revision, new basis.
	d.mu.Lock()
	f := d.facts["main"]
	f.inventory.Threads[0].Bucket = "recently-settled"
	f.inventory.Threads[0].SettledAt = "2026-10-20T11:00:00Z"
	d.mu.Unlock()
	if !strings.Contains(strings.Join(candidateKeys(d), ","), "thread-a1") {
		t.Fatal("settling did not invalidate the summary")
	}
	settled := thread(t, d, "thread-a1")
	if err := d.summarizeThread(context.Background(), in, settled, st, ""); err != nil {
		t.Fatal(err)
	}
	e, _ := d.cache.get("main/thread-a1")
	if e.Summary == nil || e.Summary.Verdict == nil || *e.Summary.Verdict != "follow-up" || e.Basis.SettledAt != "2026-10-20T11:00:00Z" {
		t.Fatalf("settled summary %+v", e)
	}
	if strings.Contains(strings.Join(candidateKeys(d), ","), "thread-a1") {
		t.Fatal("the settled summary is not current")
	}
}

// writeScript writes an executable shell script into dir.
func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestGroupingsPublishInOrder(t *testing.T) {
	d, _ := newTestDeck(t)
	in := d.cfg.Threads.Instances[0]
	refreshAndProbe(d, in)

	// The first grouping is slow and the second fast; each reports its call
	// number as the unsettled count.
	dir := t.TempDir()
	fixture, _ := filepath.Abs(filepath.Join("testdata", "group.json"))
	counter := filepath.Join(dir, "count")
	d.cmds.cfg.Group = writeScript(t, dir, "group", `n=$(cat "`+counter+`" 2>/dev/null || echo 0)
n=$((n + 1))
echo "$n" > "`+counter+`"
[ "$n" = 1 ] && sleep 1
exec sed "s/\"unsettled\": 4/\"unsettled\": $n/" "`+fixture+`"
`)
	d.now = func() time.Time { return testClock.Add(time.Minute) }
	done := make(chan error, 1)
	go func() { done <- d.regroup(context.Background(), in) }()
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(counter); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := d.refreshFacts(context.Background(), in, 0); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	f := d.facts["main"]
	if f.grouped.Summary.Unsettled != 2 || !f.asOf.Equal(testClock.Add(time.Minute)) {
		t.Fatalf("published grouping %d at %v; the later refresh must win", f.grouped.Summary.Unsettled, f.asOf)
	}
}

func TestFailedGroupingKeepsFactsTogether(t *testing.T) {
	d, _ := newTestDeck(t)
	in := d.cfg.Threads.Instances[0]
	refreshAndProbe(d, in)
	d.cmds.cfg.Group = writeScript(t, t.TempDir(), "group", "echo broken >&2\nexit 65\n")
	d.now = func() time.Time { return testClock.Add(time.Minute) }
	refreshAndProbe(d, in)
	d.mu.Lock()
	defer d.mu.Unlock()
	f := d.facts["main"]
	if !f.asOf.Equal(testClock) || f.grouped == nil || f.err == "" || !strings.Contains(f.err, "broken") {
		t.Fatalf("after a failed grouping: asOf %v err %q", f.asOf, f.err)
	}
}

func TestQuietMinutesZero(t *testing.T) {
	d, _ := newTestDeck(t)
	zero := 0
	d.cfg.Threads.QuietMinutes = &zero
	refreshAndProbe(d, d.cfg.Threads.Instances[0])
	// One second after the last activity, with no quiet period, every
	// thread with messages is eligible.
	d.now = func() time.Time { return time.Date(2026, 10, 20, 9, 0, 1, 0, time.UTC) }
	if got := len(candidateKeys(d)); got != 4 {
		t.Fatalf("%d candidates", got)
	}
}

func TestSummaryInterval(t *testing.T) {
	d, _ := newTestDeck(t)
	in := d.cfg.Threads.Instances[0]
	refreshAndProbe(d, in)
	d.runModel = func(ctx context.Context, m ModelConfig, prompt string) ([]byte, error) {
		return []byte(validSummary), nil
	}
	a1 := thread(t, d, "thread-a1")
	if err := d.summarizeThread(context.Background(), in, a1, StatusThread{}, ""); err != nil {
		t.Fatal(err)
	}

	// The thread changes; it waits for the interval, settled or not.
	d.mu.Lock()
	f := d.facts["main"]
	f.inventory.Threads[0].Bucket = "recently-settled"
	f.inventory.Threads[0].SettledAt = "2026-10-20T11:00:00Z"
	d.mu.Unlock()
	d.now = func() time.Time { return testClock.Add(359 * time.Minute) }
	if strings.Contains(strings.Join(candidateKeys(d), ","), "thread-a1") {
		t.Fatal("a thread was queued within the summary interval")
	}
	d.now = func() time.Time { return testClock.Add(360 * time.Minute) }
	if !strings.Contains(strings.Join(candidateKeys(d), ","), "thread-a1") {
		t.Fatal("a changed thread was not queued after the summary interval")
	}
}

func TestSummaryIntervalCountsFailures(t *testing.T) {
	d, _ := newTestDeck(t)
	in := d.cfg.Threads.Instances[0]
	refreshAndProbe(d, in)
	d.runModel = func(ctx context.Context, m ModelConfig, prompt string) ([]byte, error) {
		return nil, errors.New("model down")
	}
	a1 := thread(t, d, "thread-a1")
	if err := d.summarizeThread(context.Background(), in, a1, StatusThread{}, ""); err == nil {
		t.Fatal("a failing model call succeeded")
	}

	// The same basis is retried after its backoff.
	d.now = func() time.Time { return testClock.Add(15 * time.Minute) }
	if !strings.Contains(strings.Join(candidateKeys(d), ","), "thread-a1") {
		t.Fatal("a failed attempt was not retried after its backoff")
	}

	// A changed thread waits for the interval after the failed attempt.
	d.mu.Lock()
	f := d.facts["main"]
	f.inventory.Threads[0].Bucket = "recently-settled"
	f.inventory.Threads[0].SettledAt = "2026-10-20T11:00:00Z"
	d.mu.Unlock()
	if strings.Contains(strings.Join(candidateKeys(d), ","), "thread-a1") {
		t.Fatal("a changed thread was queued within the interval after a failure")
	}
	// A job queued before the change checks the interval when it runs.
	calls := 0
	d.runModel = func(ctx context.Context, m ModelConfig, prompt string) ([]byte, error) {
		calls++
		return []byte(strings.Replace(validSummary, `"verdict": null`, `"verdict": "follow-up"`, 1)), nil
	}
	settled := thread(t, d, "thread-a1")
	if err := d.summarizeThread(context.Background(), in, settled, StatusThread{}, ""); !errors.Is(err, errInterval) || calls != 0 {
		t.Fatalf("a queued job within the interval: %v, %d calls", err, calls)
	}
	if err := d.summarizeThread(context.Background(), in, settled, StatusThread{}, summaryRole); err != nil || calls != 1 {
		t.Fatalf("a refresh by hand runs at once: %v, %d calls", err, calls)
	}
	// The refresh by hand was an attempt too; the next change waits again.
	d.mu.Lock()
	f.inventory.Threads[0].SettledAt = "2026-10-20T11:30:00Z"
	d.mu.Unlock()
	d.now = func() time.Time { return testClock.Add(374 * time.Minute) }
	if strings.Contains(strings.Join(candidateKeys(d), ","), "thread-a1") {
		t.Fatal("a refresh by hand did not restart the interval")
	}
	d.now = func() time.Time { return testClock.Add(375 * time.Minute) }
	if !strings.Contains(strings.Join(candidateKeys(d), ","), "thread-a1") {
		t.Fatal("a changed thread was not queued after the summary interval")
	}
}

func TestQueuedJobWaitsWhenActiveAgain(t *testing.T) {
	d, _ := newTestDeck(t)
	in := d.cfg.Threads.Instances[0]
	refreshAndProbe(d, in)
	calls := 0
	d.runModel = func(ctx context.Context, m ModelConfig, prompt string) ([]byte, error) {
		calls++
		return []byte(validSummary), nil
	}
	// The fixture's last activity is 09:00; at 09:05 the thread is not quiet.
	d.now = func() time.Time { return time.Date(2026, 10, 20, 9, 5, 0, 0, time.UTC) }
	a1 := thread(t, d, "thread-a1")
	if err := d.summarizeThread(context.Background(), in, a1, StatusThread{}, ""); !errors.Is(err, errNotQuiet) {
		t.Fatalf("queued job: %v", err)
	}
	if calls != 0 {
		t.Fatal("a queued job spent a model call on an active thread")
	}
	if _, ok := d.cache.get("main/thread-a1"); ok {
		t.Fatal("waiting for quiet must not record an attempt")
	}
	if err := d.summarizeThread(context.Background(), in, a1, StatusThread{}, summaryRole); err != nil || calls != 1 {
		t.Fatalf("a refresh by hand runs at once: %v, %d calls", err, calls)
	}
}

func TestShownSummaryDatesTheShownText(t *testing.T) {
	d, _ := newTestDeck(t)
	in := d.cfg.Threads.Instances[0]
	refreshAndProbe(d, in)
	d.runModel = func(ctx context.Context, m ModelConfig, prompt string) ([]byte, error) {
		return []byte(validSummary), nil
	}
	// The summary succeeds but the regroup after it fails, so the page still
	// shows the grouping without it.
	d.cmds.cfg.Group = writeScript(t, t.TempDir(), "group", "exit 65\n")
	d.mu.Lock()
	d.inFlight["main/thread-a1"] = true
	d.mu.Unlock()
	d.summarize(context.Background(), "main/thread-a1", summaryRole)
	if e, ok := d.cache.get("main/thread-a1"); !ok || e.Summary == nil {
		t.Fatal("the summary was not cached")
	}
	for _, sec := range d.view().Sections {
		for _, sub := range sec.Subs {
			for _, r := range sub.Rows {
				if r.ThreadID == "thread-a1" && (r.HasSummary || !r.SummaryAt.IsZero()) {
					t.Fatalf("the row dates a summary it does not show: %+v", r)
				}
			}
		}
	}
}

func TestTransientFailuresRetryWithBackoff(t *testing.T) {
	d, _ := newTestDeck(t)
	in := d.cfg.Threads.Instances[0]
	refreshAndProbe(d, in)
	now := testClock
	d.now = func() time.Time { return now }
	d.runModel = func(ctx context.Context, m ModelConfig, prompt string) ([]byte, error) {
		return nil, errors.New("rate limited")
	}
	d4 := thread(t, d, "thread-d4")
	if err := d.summarizeThread(context.Background(), in, d4, StatusThread{State: "settled", Verdict: "check"}, ""); err == nil {
		t.Fatal("expected a failure")
	}
	e, _ := d.cache.get("main/thread-d4")
	if e.Failures != 1 || !e.RetryAt.Equal(testClock.Add(15*time.Minute)) {
		t.Fatalf("after one failure: %+v", e)
	}
	if strings.Contains(strings.Join(candidateKeys(d), ","), "thread-d4") {
		t.Fatal("retried before the backoff")
	}
	now = testClock.Add(16 * time.Minute)
	if !strings.Contains(strings.Join(candidateKeys(d), ","), "thread-d4") {
		t.Fatal("not retried after the backoff")
	}
	if err := d.summarizeThread(context.Background(), in, d4, StatusThread{State: "settled", Verdict: "check"}, ""); err == nil {
		t.Fatal("expected a failure")
	}
	e, _ = d.cache.get("main/thread-d4")
	if e.Failures != 2 || !e.RetryAt.Equal(now.Add(30*time.Minute)) {
		t.Fatalf("after two failures: %+v", e)
	}
	if retryBackoff(20) != 6*time.Hour {
		t.Fatal("backoff is not capped")
	}

	// Invalid output is not retried until the thread changes.
	d.runModel = func(ctx context.Context, m ModelConfig, prompt string) ([]byte, error) {
		return []byte("not json"), nil
	}
	if err := d.summarizeThread(context.Background(), in, d4, StatusThread{State: "settled", Verdict: "check"}, summaryRole); err == nil {
		t.Fatal("expected a failure")
	}
	e, _ = d.cache.get("main/thread-d4")
	if !e.RetryAt.IsZero() {
		t.Fatalf("invalid output scheduled a retry: %+v", e)
	}
}

func TestPruneForgetsOldThreads(t *testing.T) {
	d, _ := newTestDeck(t)
	old := testClock.Add(-10 * 24 * time.Hour)
	for key, at := range map[string]time.Time{
		"main/gone-old":   old,
		"main/gone-new":   testClock.Add(-time.Hour),
		"main/thread-a1":  old,
		"other/gone-old2": old,
	} {
		if err := d.cache.put(key, CacheEntry{AttemptedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	d.probes["main/gone-old"] = probe{revision: "x"}
	refreshAndProbe(d, d.cfg.Threads.Instances[0])
	for key, want := range map[string]bool{"main/gone-old": false, "main/gone-new": true, "main/thread-a1": true, "other/gone-old2": true} {
		if _, ok := d.cache.get(key); ok != want {
			t.Errorf("%s kept = %v", key, ok)
		}
	}
	d.mu.Lock()
	_, ok := d.probes["main/gone-old"]
	d.mu.Unlock()
	if ok {
		t.Error("the probe of a gone thread was kept")
	}
	reopened, err := openSummaryCache(d.cache.path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reopened.get("main/gone-old"); ok {
		t.Error("the pruned entry is still on disk")
	}
}

func TestPruneKeepsEntriesForTheInterval(t *testing.T) {
	d, _ := newTestDeck(t)
	interval := 15 * 24 * 60 // longer than the window
	d.cfg.Threads.SummaryIntervalMinutes = &interval
	if err := d.cache.put("main/gone-old", CacheEntry{AttemptedAt: testClock.Add(-10 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	refreshAndProbe(d, d.cfg.Threads.Instances[0])
	if _, ok := d.cache.get("main/gone-old"); !ok {
		t.Error("an entry within the summary interval was pruned")
	}
}

func TestBudgetRequeueIsAtomic(t *testing.T) {
	d, _ := newTestDeck(t)
	refreshAndProbe(d, d.cfg.Threads.Instances[0])
	d.cfg.Threads.CallsPerHour = 1
	d.calls = []time.Time{testClock} // the budget is spent
	key := "main/thread-a1"
	d.mu.Lock()
	d.queue = nil
	d.inFlight[key] = true
	d.mu.Unlock()
	d.summarize(context.Background(), key, summaryRole)
	d.mu.Lock()
	queued := len(d.queue) > 0 && d.queue[0] == key
	running := d.inFlight[key]
	d.mu.Unlock()
	if !queued || running || d.manual[key] == "" {
		t.Fatalf("after the budget stop: queued %v running %v", queued, running)
	}
	d.schedule()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.queue) == 0 || d.queue[0] != key {
		t.Fatalf("schedule dropped the requeued job: %v", d.queue)
	}
}

func TestStaleSettlementVerdictIsDropped(t *testing.T) {
	d, _ := newTestDeck(t)
	in := d.cfg.Threads.Instances[0]
	refreshAndProbe(d, in)
	s, err := parseSummary([]byte(strings.Replace(validSummary, `"verdict": null`, `"verdict": "done"`, 1)), true)
	if err != nil {
		t.Fatal(err)
	}
	// Made for an earlier settlement of thread-d4.
	if err := d.cache.put("main/thread-d4", CacheEntry{Summary: &s, Basis: Basis{Revision: "r", Bucket: "recently-settled", SettledAt: "2026-10-18T10:00:00Z"}}); err != nil {
		t.Fatal(err)
	}
	report, _, err := d.buildReport(in, d.facts["main"].inventory)
	if err != nil {
		t.Fatal(err)
	}
	var r struct {
		Threads []map[string]json.RawMessage `json:"threads"`
	}
	if err := json.Unmarshal(report, &r); err != nil {
		t.Fatal(err)
	}
	for _, row := range r.Threads {
		if string(row["threadId"]) == `"thread-d4"` {
			if string(row["verdict"]) != "null" || string(row["goal"]) == "" {
				t.Fatalf("stale verdict %s, goal %s", row["verdict"], row["goal"])
			}
			return
		}
	}
	t.Fatal("thread-d4 missing from the report")
}

func TestQueuedJobRechecksFacts(t *testing.T) {
	d, _ := newTestDeck(t)
	refreshAndProbe(d, d.cfg.Threads.Instances[0])
	key := "main/thread-e5" // acknowledged
	d.mu.Lock()
	d.inFlight[key] = true
	d.mu.Unlock()
	d.summarize(context.Background(), key, summaryRole) // runModel fails the test if called
	if _, ok := d.cache.get(key); ok {
		t.Fatal("an acknowledged thread was summarised")
	}
}

func TestProbesDoNotHoldFacts(t *testing.T) {
	d, _ := newTestDeck(t)
	in := d.cfg.Threads.Instances[0]
	release := filepath.Join(t.TempDir(), "release")
	d.cmds.cfg.Messages = writeScript(t, t.TempDir(), "messages", "while [ ! -e "+release+" ]; do sleep 0.05; done\nexit 1\n")
	start := time.Now()
	d.refreshInstance(context.Background(), in)
	d.refreshInstance(context.Background(), in)
	if time.Since(start) > 2*time.Second {
		t.Fatalf("facts refreshes waited for probes: %s", time.Since(start))
	}
	d.mu.Lock()
	probing := d.probing["main"]
	d.mu.Unlock()
	if !probing {
		t.Fatal("no probe pass running")
	}
	os.WriteFile(release, nil, 0o600)
	d.probeWG.Wait()
}

func TestAverageOverAllInstances(t *testing.T) {
	d, _ := newTestDeck(t)
	pct := func(n int) *int { return &n }
	row := func(p *int) GroupRow {
		return GroupRow{InventoryThread: InventoryThread{ThreadID: "x", Bucket: "unsettled"}, SummaryFields: SummaryFields{ProgressPct: p}}
	}
	second := d.cfg.Threads.Instances[0]
	second.Name = "second"
	d.cfg.Threads.Instances = append(d.cfg.Threads.Instances, second)
	d.facts["main"].grouped = &Grouped{Sections: []GroupSection{{Key: "agent", Threads: []GroupRow{row(pct(40)), row(pct(40)), row(pct(41))}}}}
	d.facts["second"] = &instanceFacts{grouped: &Grouped{Sections: []GroupSection{{Key: "agent", Threads: []GroupRow{row(pct(41)), row(nil)}}}}}
	v := d.view()
	// (40 + 40 + 41 + 41 + 0) / 5 = 32.4
	if !v.HasAvg || v.AvgPct != 32 {
		t.Fatalf("average %d", v.AvgPct)
	}
}

func TestSlowProbeCannotReplaceANewerRead(t *testing.T) {
	d, _ := newTestDeck(t)
	zero := 0 // the interval has its own tests
	d.cfg.Threads.SummaryIntervalMinutes = &zero
	in := d.cfg.Threads.Instances[0]
	dir := t.TempDir()
	release := filepath.Join(dir, "release")
	fixture, err := filepath.Abs(filepath.Join("testdata", "messages.json"))
	if err != nil {
		t.Fatal(err)
	}
	// A probe (--head 0) reads revision rev-A but only returns once
	// released; a summary read sees the newer rev-B.
	d.cmds.cfg.Messages = writeScript(t, dir, "messages", `for arg; do id=$arg; done
case " $* " in
*" --head 0 "*)
	touch "`+dir+`/started-$id"
	while [ ! -e "`+release+`" ]; do sleep 0.02; done
	rev=rev-A ;;
*) rev=rev-B ;;
esac
exec sed -e "s/__THREAD__/$id/" -e "s/40:2026-10-20T09:00:00.000Z/$rev/" "`+fixture+`"
`)
	calls := 0
	d.runModel = func(ctx context.Context, m ModelConfig, prompt string) ([]byte, error) {
		calls++
		return nil, errors.New("rate limited")
	}

	d.refreshInstance(context.Background(), in)
	deadline := time.Now().Add(hangGuard)
	for {
		if _, err := os.Stat(filepath.Join(dir, "started-thread-d4")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			os.WriteFile(release, nil, 0o600)
			t.Fatal("the probe of thread-d4 did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// A refresh by hand reads rev-B while the probe is still out, and fails.
	d.mu.Lock()
	_, d4, st, _ := d.findThreadLocked("main/thread-d4")
	d.mu.Unlock()
	if err := d.summarizeThread(context.Background(), in, d4, st, summaryRole); err == nil {
		t.Fatal("expected a failure")
	}
	os.WriteFile(release, nil, 0o600)
	d.probeWG.Wait()

	d.mu.Lock()
	p := d.probes["main/thread-d4"]
	d.mu.Unlock()
	if p.revision != "rev-B" {
		t.Fatalf("the slow probe replaced the newer read: %+v", p)
	}
	if strings.Contains(strings.Join(candidateKeys(d), ","), "thread-d4") {
		t.Fatal("queued again during the backoff")
	}

	// A job queued on an older revision checks the cache for the revision
	// it reads, so the backoff still holds.
	d.mu.Lock()
	p.revision = "rev-A"
	d.probes["main/thread-d4"] = p
	d.inFlight["main/thread-d4"] = true
	d.mu.Unlock()
	if !strings.Contains(strings.Join(candidateKeys(d), ","), "thread-d4") {
		t.Fatal("an older revision did not queue the thread")
	}
	d.summarize(context.Background(), "main/thread-d4", "")
	if calls != 1 {
		t.Fatalf("model calls %d, want 1", calls)
	}
	e, _ := d.cache.get("main/thread-d4")
	if e.Failures != 1 || !e.RetryAt.Equal(testClock.Add(15*time.Minute)) || e.AttemptBasis.Revision != "rev-B" {
		t.Fatalf("the backoff moved: %+v", e)
	}
}

func TestSummaryMaybeOutOfDateWhenChecksLag(t *testing.T) {
	d, _ := newTestDeck(t)
	in := d.cfg.Threads.Instances[0]
	refreshAndProbe(d, in)
	d.runModel = func(ctx context.Context, m ModelConfig, prompt string) ([]byte, error) {
		return []byte(validSummary), nil
	}
	d.mu.Lock()
	d.inFlight["main/thread-a1"] = true
	d.mu.Unlock()
	d.summarize(context.Background(), "main/thread-a1", summaryRole)

	row := func() RowView {
		for _, sec := range d.view().Sections {
			for _, sub := range sec.Subs {
				for _, r := range sub.Rows {
					if r.ThreadID == "thread-a1" {
						return r
					}
				}
			}
		}
		t.Fatal("no row for thread-a1")
		return RowView{}
	}
	if r := row(); !r.HasSummary || r.SummaryStale || r.SummaryUnchecked {
		t.Fatalf("a current summary is flagged: %+v", r)
	}

	// The last good check saw an earlier updatedAt than the inventory now
	// holds, and the checks since have failed.
	d.mu.Lock()
	p := d.probes["main/thread-a1"]
	p.updatedAt = "2026-10-20T08:00:00Z"
	d.probes["main/thread-a1"] = p
	d.mu.Unlock()
	if r := row(); !r.HasSummary || r.SummaryStale || !r.SummaryUnchecked {
		t.Fatalf("a lagging summary is not flagged: %+v", r)
	}
	var page strings.Builder
	tmpl, err := parseTemplates()
	if err != nil {
		t.Fatal(err)
	}
	if err := tmpl.ExecuteTemplate(&page, "content", d.view()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(page.String(), "maybe out of date") {
		t.Fatal("the page does not show the summary may be out of date")
	}
}

func TestShutdownRecordsNoFailure(t *testing.T) {
	d, _ := newTestDeck(t)
	in := d.cfg.Threads.Instances[0]
	refreshAndProbe(d, in)
	ctx, cancel := context.WithCancel(context.Background())
	d.runModel = func(callCtx context.Context, m ModelConfig, prompt string) ([]byte, error) {
		cancel() // the service stops while the model runs
		return nil, errors.New("signal: killed")
	}
	d4 := thread(t, d, "thread-d4")
	if err := d.summarizeThread(ctx, in, d4, StatusThread{State: "settled", Verdict: "check"}, ""); err == nil {
		t.Fatal("expected an error")
	}
	if e, ok := d.cache.get("main/thread-d4"); ok && (e.Failures != 0 || !e.RetryAt.IsZero() || e.Error != "") {
		t.Fatalf("a shutdown was recorded as a failure: %+v", e)
	}
}

func TestSeenUpToIsWhenTheFactsWereRead(t *testing.T) {
	d, _ := newTestDeck(t)
	in := d.cfg.Threads.Instances[0]
	// Every reading of the clock is a minute later than the one before.
	var mu sync.Mutex
	now := testClock
	d.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		now = now.Add(time.Minute)
		return now
	}
	if v := d.view(); !v.SeenUpTo.IsZero() {
		t.Fatalf("seen up to %s before any facts", v.SeenUpTo)
	}
	refreshAndProbe(d, in)
	d.mu.Lock()
	f := d.facts["main"]
	readAt, asOf := f.readAt, f.asOf
	d.mu.Unlock()
	if !readAt.Before(asOf) {
		t.Fatalf("read at %s, published at %s", readAt, asOf)
	}
	v := d.view()
	if !v.SeenUpTo.Equal(readAt) {
		t.Fatalf("seen up to %s, want %s", v.SeenUpTo, readAt)
	}
	var page strings.Builder
	tmpl, err := parseTemplates()
	if err != nil {
		t.Fatal(err)
	}
	if err := tmpl.ExecuteTemplate(&page, "content", v); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(page.String(), `data-seen="`+readAt.UTC().Format(time.RFC3339)+`"`) {
		t.Fatal("the page does not carry the time its facts cover")
	}
}

func TestHungPrimaryLeavesTimeForBackups(t *testing.T) {
	old := modelTimeout
	modelTimeout = 100 * time.Millisecond
	defer func() { modelTimeout = old }()
	models := []ModelConfig{{Provider: "anthropic", Model: "a"}, {Provider: "anthropic", Model: "b"}}
	run := func(ctx context.Context, m ModelConfig, prompt string) ([]byte, error) {
		if m.Model == "a" {
			<-ctx.Done() // hangs until its own deadline
			return nil, ctx.Err()
		}
		return []byte("ok"), nil
	}
	out, model, err := callModel(context.Background(), run, models, "p", func() bool { return true })
	if err != nil || string(out) != "ok" || model != "anthropic/b" {
		t.Fatalf("backup after a hung primary: %q %q %v", out, model, err)
	}
}

func TestFailedReadDoesNotSeedAProbe(t *testing.T) {
	d, _ := newTestDeck(t)
	// A refresh by hand failed before reading any messages.
	if err := d.cache.put("main/thread-a1", CacheEntry{
		AttemptedAt: testClock, Error: "messages failed", Failures: 1, RetryAt: testClock.Add(15 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	_, seeded := d.knownProbeLocked("main/thread-a1")
	d.mu.Unlock()
	if seeded {
		t.Fatal("a failed read seeded a probe, so the thread is not checked")
	}
}

func TestShutdownWaitsForWork(t *testing.T) {
	d, _ := newTestDeck(t)
	started := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	returned := false
	d.runModel = func(ctx context.Context, m ModelConfig, prompt string) ([]byte, error) {
		once.Do(func() { close(started) })
		<-ctx.Done()
		time.Sleep(50 * time.Millisecond)
		mu.Lock()
		returned = true
		mu.Unlock()
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	d.run(ctx)
	select {
	case <-started:
	case <-time.After(hangGuard):
		cancel()
		t.Fatal("no summary started")
	}
	cancel()
	if !d.wait(hangGuard) {
		t.Fatal("the background work did not stop")
	}
	mu.Lock()
	defer mu.Unlock()
	if !returned {
		t.Fatal("wait returned before the summary job ended")
	}
}

func TestVerdictOfOlderMessagesIsDropped(t *testing.T) {
	d, _ := newTestDeck(t)
	in := d.cfg.Threads.Instances[0]
	refreshAndProbe(d, in)
	s, err := parseSummary([]byte(strings.Replace(validSummary, `"verdict": null`, `"verdict": "done"`, 1)), true)
	if err != nil {
		t.Fatal(err)
	}
	verdict := func() string {
		report, _, err := d.buildReport(in, d.facts["main"].inventory)
		if err != nil {
			t.Fatal(err)
		}
		var r struct {
			Threads []map[string]json.RawMessage `json:"threads"`
		}
		if err := json.Unmarshal(report, &r); err != nil {
			t.Fatal(err)
		}
		for _, row := range r.Threads {
			if string(row["threadId"]) == `"thread-d4"` {
				return string(row["verdict"])
			}
		}
		t.Fatal("thread-d4 missing from the report")
		return ""
	}
	d4 := thread(t, d, "thread-d4")
	// Made for the current settlement and the revision the probe saw.
	d.mu.Lock()
	rev := d.probes["main/thread-d4"].revision
	d.mu.Unlock()
	if err := d.cache.put("main/thread-d4", CacheEntry{Summary: &s, Basis: basisOf(rev, d4)}); err != nil {
		t.Fatal(err)
	}
	if got := verdict(); got != `"done"` {
		t.Fatalf("a current verdict was dropped: %s", got)
	}
	// New messages arrive without a new settlement.
	d.mu.Lock()
	p := d.probes["main/thread-d4"]
	p.revision = "41:2026-10-20T11:00:00.000Z"
	d.probes["main/thread-d4"] = p
	d.mu.Unlock()
	if got := verdict(); got != "null" {
		t.Fatalf("a verdict of older messages decides the grouping: %s", got)
	}
}

func TestFailedReadRecordsTheCheckedUpdatedAt(t *testing.T) {
	d, _ := newTestDeck(t)
	in := d.cfg.Threads.Instances[0]
	refreshAndProbe(d, in)
	d.cmds.cfg.Messages = writeScript(t, t.TempDir(), "messages", "exit 1\n")
	d.mu.Lock()
	checked := d.probes["main/thread-d4"].updatedAt
	d.mu.Unlock()
	// The thread moved on after the last good check, then the read failed.
	d4 := thread(t, d, "thread-d4")
	d4.UpdatedAt = "2026-10-20T11:30:00Z"
	if err := d.summarizeThread(context.Background(), in, d4, StatusThread{State: "settled", Verdict: "check"}, summaryRole); err == nil {
		t.Fatal("expected a failure")
	}
	e, _ := d.cache.get("main/thread-d4")
	if e.ThreadUpdatedAt != checked {
		t.Fatalf("recorded updatedAt %q, want the checked %q", e.ThreadUpdatedAt, checked)
	}
	// After a restart the stale revision is probed again, not trusted.
	d.mu.Lock()
	delete(d.probes, "main/thread-d4")
	p, ok := d.knownProbeLocked("main/thread-d4")
	d.mu.Unlock()
	if ok && p.updatedAt == d4.UpdatedAt {
		t.Fatalf("seeded probe claims the old revision is current: %+v", p)
	}
}

func TestNewRevisionRegroupsAtOnce(t *testing.T) {
	d, _ := newTestDeck(t)
	in := d.cfg.Threads.Instances[0]
	copyPath := filepath.Join(t.TempDir(), "report.json")
	t.Setenv("DECK_REPORT_COPY", copyPath)
	s, err := parseSummary([]byte(strings.Replace(validSummary, `"verdict": null`, `"verdict": "done"`, 1)), true)
	if err != nil {
		t.Fatal(err)
	}
	// After a restart: a done verdict for older messages, for the current
	// settlement of thread-d4.
	if err := d.cache.put("main/thread-d4", CacheEntry{Summary: &s, Basis: Basis{
		Revision: "39:2026-10-20T08:00:00.000Z", Bucket: "recently-settled", SettledAt: "2026-10-20T10:00:00Z",
	}}); err != nil {
		t.Fatal(err)
	}
	refreshAndProbe(d, in)
	data, err := os.ReadFile(copyPath)
	if err != nil {
		t.Fatal(err)
	}
	var r struct {
		Threads []map[string]json.RawMessage `json:"threads"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	for _, row := range r.Threads {
		if string(row["threadId"]) == `"thread-d4"` {
			if string(row["verdict"]) != "null" {
				t.Fatalf("the last grouping still used the old verdict: %s", row["verdict"])
			}
			return
		}
	}
	t.Fatal("thread-d4 missing from the last report")
}

func TestSummaryOfAnotherSettlementIsStaleWithoutAProbe(t *testing.T) {
	d, _ := newTestDeck(t)
	in := d.cfg.Threads.Instances[0]
	refreshAndProbe(d, in)
	// thread-e5 is settled and done by the facts, so it is never probed.
	if err := d.cache.put("main/thread-e5", CacheEntry{Summary: &SummaryFields{Goal: "old goal"}, Basis: Basis{
		Revision: "r", Bucket: "unsettled",
	}, Model: "m", MadeAt: testClock}); err != nil {
		t.Fatal(err)
	}
	if err := d.regroup(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	_, probed := d.probes["main/thread-e5"]
	d.mu.Unlock()
	if probed {
		t.Fatal("thread-e5 was probed")
	}
	for _, sec := range d.view().Sections {
		for _, sub := range sec.Subs {
			for _, r := range sub.Rows {
				if r.ThreadID == "thread-e5" {
					if !r.HasSummary || !r.SummaryStale {
						t.Fatalf("a summary from before the settlement is not marked stale: %+v", r)
					}
					return
				}
			}
		}
	}
	t.Fatal("no row for thread-e5")
}

func TestRefreshDuringARunningJobRunsNext(t *testing.T) {
	d, _ := newTestDeck(t)
	in := d.cfg.Threads.Instances[0]
	d.models["deep"] = d.models[summaryRole]
	refreshAndProbe(d, in)
	d.runModel = func(ctx context.Context, m ModelConfig, prompt string) ([]byte, error) {
		return []byte(validSummary), nil
	}
	// An automatic job for thread-a1 is running when the button is pressed.
	d.mu.Lock()
	d.inFlight["main/thread-a1"] = true
	d.mu.Unlock()
	if err := d.requestSummary("main", "thread-a1", "deep"); err != nil {
		t.Fatal(err)
	}
	d.summarize(context.Background(), "main/thread-a1", "")
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.queue) == 0 || d.queue[0] != "main/thread-a1" || d.manual["main/thread-a1"] != "deep" {
		t.Fatalf("the refresh by hand was dropped: queue %v, manual %v", d.queue, d.manual)
	}
}

func TestRefreshAllQueuesFreshFactsAcrossInstances(t *testing.T) {
	d, _ := newTestDeck(t)
	cfg := d.cfg
	second := cfg.Threads.Instances[0]
	second.Name = "second"
	cfg.Threads.Instances = append(cfg.Threads.Instances, second)
	d = newDeck(cfg, d.cache, d.models, nil, quietLog())
	d.now = func() time.Time { return testClock }
	// Fresh facts must be requested even if the last read is recent. The
	// empty inventory here also checks that newly discovered threads queue.
	for _, f := range d.facts {
		f.asOf = testClock
	}
	d.requestRefreshAll()
	d.requestRefreshAll()
	for _, in := range cfg.Threads.Instances {
		if len(d.refreshCh[in.Name]) != 1 {
			t.Fatalf("refresh requests did not coalesce for %s", in.Name)
		}
		<-d.refreshCh[in.Name]
		refreshAndProbe(d, in)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.queue) != 8 || len(d.manual) != 8 {
		t.Fatalf("queue %v, manual %v", d.queue, d.manual)
	}
	for _, in := range cfg.Threads.Instances {
		for _, id := range []string{"thread-a1", "thread-b2", "thread-c3", "thread-d4"} {
			if d.manual[cacheKey(in.Name, id)] != cfg.Threads.SummaryRole {
				t.Errorf("missing manual refresh for %s/%s", in.Name, id)
			}
		}
		if d.manual[cacheKey(in.Name, "thread-e5")] != "" {
			t.Error("an accepted thread was queued")
		}
	}
}

func TestRefreshAllHonoursConcurrencyAndBudget(t *testing.T) {
	d, _ := newTestDeck(t)
	d.cfg.Threads.RefreshConcurrency = 2
	d.cfg.Threads.CallsPerHour = 2
	d.requestRefreshAll()
	refreshAndProbe(d, d.cfg.Threads.Instances[0])
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	d.runModel = func(ctx context.Context, m ModelConfig, prompt string) ([]byte, error) {
		started <- struct{}{}
		<-release
		return []byte(validSummary), nil
	}
	defer d.work.Wait()
	defer close(release)
	d.startJobs(context.Background())
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(hangGuard):
			t.Fatal("summary workers did not start")
		}
	}
	d.mu.Lock()
	if len(d.inFlight) != 2 || len(d.queue) != 2 {
		t.Errorf("running %v, queued %v", d.inFlight, d.queue)
	}
	d.mu.Unlock()
	d.startJobs(context.Background())
	select {
	case <-started:
		t.Fatal("a third model call exceeded concurrency or budget")
	default:
	}
}

func TestOneShotRoleAndAutomaticDefault(t *testing.T) {
	d, _ := newTestDeck(t)
	zero := 0 // the interval has its own tests
	d.cfg.Threads.SummaryIntervalMinutes = &zero
	in := d.cfg.Threads.Instances[0]
	refreshAndProbe(d, in)
	d.models["deep"] = []ModelConfig{{Provider: "anthropic", Model: "deep-model", Transport: "cli"}}
	key := "main/thread-a1"
	var called []string
	d.runModel = func(ctx context.Context, m ModelConfig, prompt string) ([]byte, error) {
		called = append(called, m.Model)
		return []byte(validSummary), nil
	}
	if err := d.requestSummary("main", "thread-a1", "deep"); err != nil {
		t.Fatal(err)
	}
	// Limit this dispatcher pass to the requested row.
	d.queue = d.queue[:1]
	d.startJobs(context.Background())
	d.work.Wait()
	reopened, err := openSummaryCache(d.cache.path)
	if err != nil {
		t.Fatal(err)
	}
	e, _ := reopened.get(key)
	if e.Role != "deep" || e.Model != "anthropic/deep-model" {
		t.Fatalf("cached role and model: %+v", e)
	}
	// A changed thread must return to the configured automatic role.
	messages, err := os.ReadFile("testdata/messages.json")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "messages.json")
	messages = []byte(strings.ReplaceAll(string(messages), "40:2026-10-20T09:00:00.000Z", "new-revision"))
	if err := os.WriteFile(path, messages, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DECK_MESSAGES", path)
	a1 := thread(t, d, "thread-a1")
	if err := d.summarizeThread(context.Background(), in, a1, StatusThread{}, ""); err != nil {
		t.Fatal(err)
	}
	e, _ = d.cache.get(key)
	if strings.Join(called, ",") != "deep-model,light-model" || e.Role != summaryRole {
		t.Fatalf("models %v, cached role %q", called, e.Role)
	}
}

func TestRefreshAllPreservesPendingRole(t *testing.T) {
	for _, rowFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "row during bulk refresh", true: "row before bulk refresh"}[rowFirst], func(t *testing.T) {
			d, _ := newTestDeck(t)
			in := d.cfg.Threads.Instances[0]
			refreshAndProbe(d, in)
			d.models["deep"] = d.models[summaryRole]
			d.cfg.Threads.CallsPerHour = 1
			d.calls = []time.Time{testClock}
			if !rowFirst {
				d.requestRefreshAll()
			}
			if err := d.requestSummary("main", "thread-a1", "deep"); err != nil {
				t.Fatal(err)
			}
			if rowFirst {
				d.requestRefreshAll()
			}
			refreshAndProbe(d, in)
			if d.manual["main/thread-a1"] != "deep" {
				t.Fatalf("bulk refresh replaced the pending role: %v", d.manual)
			}
			if len(d.queue) == 0 || d.queue[0] != "main/thread-a1" {
				t.Fatalf("bulk defaults displaced the row's priority: %v", d.queue)
			}
		})
	}
}
