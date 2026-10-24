package main

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const validVerdict = `{"verdict":"close","reason":"The importer PR merged.","criteria":"The importer runs."}`

// newVerdictDeck is a backlog deck with verdicts on: the fake item command,
// a fresh cache and a model that answers answer and records its calls.
func newVerdictDeck(t *testing.T, answer string) (*Deck, http.Handler, *[]ModelConfig) {
	t.Helper()
	d, _ := newBacklogDeck(t)
	d.cfg.Backlog.Commands.Item = fakeBin(t, "backlog-item")
	d.backlog.cfg = *d.cfg.Backlog
	d.backlog.deck = d
	cache, err := openVerdictCache(filepath.Join(t.TempDir(), "verdicts.json"))
	if err != nil {
		t.Fatal(err)
	}
	d.backlog.cache = cache
	var mu sync.Mutex
	calls := &[]ModelConfig{}
	d.runModel = func(ctx context.Context, m ModelConfig, prompt string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		*calls = append(*calls, m)
		return []byte(answer), nil
	}
	return d, contentTestHandler(t, d, nil), calls
}

// judgeAll runs the worker until nothing is due and returns how many ran.
func judgeAll(d *Deck) int {
	n := 0
	for d.backlog.judgeNext(context.Background()) {
		n++
	}
	return n
}

func TestVerdictConfig(t *testing.T) {
	d, _ := newBacklogDeck(t)
	b := d.cfg.Backlog
	if b.VerdictRole != "light" || *b.VerdictEffort != "low" || *b.VerdictIntervalMinutes != 360 || b.Commands.Item != "" {
		t.Fatalf("defaults: %+v", b)
	}
	cfg := testConfig(t)
	cfg.Backlog = &BacklogConfig{VerdictRole: "special", Commands: BacklogCommandsConfig{Inventory: "x", Item: "y"}}
	if err := cfg.normalize(); err == nil || !strings.Contains(err.Error(), "verdictRole") {
		t.Errorf("a role outside summaryRoles: %v", err)
	}
	// Without an item command there are no verdicts, so the role is not checked.
	cfg = testConfig(t)
	cfg.Backlog = &BacklogConfig{VerdictRole: "special", Commands: BacklogCommandsConfig{Inventory: "x"}}
	if err := cfg.normalize(); err != nil {
		t.Errorf("facts only: %v", err)
	}
}

func TestItemArgs(t *testing.T) {
	issue := BacklogIssue{Ref: "o/r#1", MergedPRs: []BacklogMergedPR{
		{Ref: "o/r#2", MergedAt: "2026-10-01T00:00:00Z"}, {Ref: "o/r#5", MergedAt: "2026-10-05T00:00:00Z"},
		{Ref: "o/r#3", MergedAt: "2026-10-03T00:00:00Z"}, {Ref: "o/r#4", MergedAt: "2026-10-04T00:00:00Z"},
	}}
	args, err := itemArgs(issue)
	if err != nil || strings.Join(args, " ") != "--max-chars 2000 o/r#1 o/r#5 o/r#4 o/r#3" {
		t.Fatalf("args %q, %v", args, err)
	}
	issue.MergedPRs = append(issue.MergedPRs, BacklogMergedPR{Ref: "-x/y#9", MergedAt: "2026-10-09T00:00:00Z"})
	if _, err := itemArgs(issue); err == nil {
		t.Error("a pull request ref starting with a dash was passed on")
	}
	if a, b := verdictBasis(BacklogIssue{UpdatedAt: "t", MergedPRs: []BacklogMergedPR{{Ref: "b"}, {Ref: "a"}}}), verdictBasis(BacklogIssue{UpdatedAt: "t", MergedPRs: []BacklogMergedPR{{Ref: "a"}, {Ref: "b"}}}); a != b {
		t.Errorf("basis depends on the order of merged PRs: %q, %q", a, b)
	}
}

func TestParseVerdict(t *testing.T) {
	v, err := parseVerdict([]byte("```json\n" + validVerdict + "\n```"))
	if err != nil || v.Verdict != "close" || v.Reason != "The importer PR merged." || v.Criteria == nil {
		t.Fatalf("%+v, %v", v, err)
	}
	if v, err := parseVerdict([]byte(`{"verdict":"keep","reason":"Waits on upstream.","criteria":null}`)); err != nil || v.Criteria != nil {
		t.Errorf("null criteria: %+v, %v", v, err)
	}
	// Models often answer criteria as a list.
	if v, err := parseVerdict([]byte(`{"verdict":"keep","reason":"x","criteria":["The importer runs."," ","Docs updated."]}`)); err != nil || v.Criteria == nil || *v.Criteria != "The importer runs.; Docs updated." {
		t.Errorf("list criteria: %+v, %v", v, err)
	}
	if v, err := parseVerdict([]byte(`{"verdict":"keep","reason":"x","criteria":[]}`)); err != nil || v.Criteria != nil {
		t.Errorf("empty list criteria: %+v, %v", v, err)
	}
	for _, bad := range []string{
		`{"verdict":"maybe","reason":"x","criteria":null}`,
		`{"verdict":"close","reason":" ","criteria":null}`,
		`{"verdict":"close","reason":"x","criteria":null,"extra":1}`,
		`{"verdict":"close","reason":"x","criteria":{"a":1}}`,
		`{"verdict":"close","reason":"x","criteria":[1,2]}`,
		`{"verdict":"close","reason":"x","criteria":[null]}`,
		`{"verdict":"close","reason":"` + strings.Repeat("x", 301) + `","criteria":null}`,
		validVerdict + ` trailing`,
		`not json`,
	} {
		if _, err := parseVerdict([]byte(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	for _, bad := range []string{
		`{"verdict":"close","reason":"SECRET-REASON","x":1}`,
		`{"verdict":"close","reason":"x","SECRET-REASON":1}`,
	} {
		if _, err := parseVerdict([]byte(bad)); err == nil || strings.Contains(err.Error(), "SECRET-REASON") {
			t.Errorf("%q: the error quotes the answer: %v", bad, err)
		}
	}
}

func TestAutomaticVerdicts(t *testing.T) {
	d, _, calls := newVerdictDeck(t, validVerdict)
	var prompts []string
	run := d.runModel
	d.runModel = func(ctx context.Context, m ModelConfig, prompt string) ([]byte, error) {
		prompts = append(prompts, prompt)
		return run(ctx, m, prompt)
	}
	// The fixture shortlists two maybe-done and two stale issues.
	if n := judgeAll(d); n != 4 || len(*calls) != 4 {
		t.Fatalf("judged %d, %d calls", n, len(*calls))
	}
	for _, m := range *calls {
		if m.Effort != "low" {
			t.Errorf("verdict made at effort %q", m.Effort)
		}
	}
	p := prompts[0]
	facts, data := strings.Index(p, "\nFACTS\n"), strings.Index(p, "\nBEGIN ISSUE DATA (")
	body := strings.Index(p, "ISSUE-BODY-TEXT")
	if facts < 0 || data < facts || body < data || strings.LastIndex(p, "\nEND ISSUE DATA\n") < body {
		t.Error("the issue's text is not inside the untrusted block after the facts")
	}
	for _, ref := range []string{"example/app#1", "example/app#2", "example/lib#4", "example/lib#6"} {
		if e, ok := d.backlog.cache.get(ref); !ok || e.Verdict == nil || e.Role != "light" || e.Model != "anthropic/light-model" {
			t.Errorf("%s: %+v", ref, e)
		}
	}
	if judgeAll(d) != 0 {
		t.Fatal("an unchanged issue was judged again")
	}

	// A change waits for the interval, then is judged once more.
	d.backlog.mu.Lock()
	d.backlog.inv.Issues[0].UpdatedAt = "2026-10-20T11:30:00Z"
	d.backlog.mu.Unlock()
	clock := testClock.Add(time.Hour)
	d.backlog.now = func() time.Time { return clock }
	if judgeAll(d) != 0 {
		t.Fatal("a changed issue skipped the verdict interval")
	}
	if v := d.backlog.view("/backlog/"); !v.Sections[0].Subs[0].Rows[0].VerdictOutdated {
		t.Error("the outdated verdict is not marked")
	}
	clock = testClock.Add(7 * time.Hour)
	if judgeAll(d) != 1 {
		t.Fatal("a changed issue was not judged after the interval")
	}
}

func TestVerdictReserveAndBudget(t *testing.T) {
	d, _, calls := newVerdictDeck(t, validVerdict)
	d.cfg.Threads.CallsPerHour = 4
	d.calls = []time.Time{testClock, testClock, testClock} // one call left, a quarter of the budget
	if judgeAll(d) != 0 {
		t.Fatal("an automatic verdict ate into the summaries' reserve")
	}
	if err := d.backlog.requestVerdict("example/app#1", "light"); err != nil {
		t.Fatal(err)
	}
	if judgeAll(d) != 1 || len(*calls) != 1 {
		t.Fatal("a verdict asked for by hand did not use the last call")
	}
	// With no budget left, a request by hand waits in the queue without
	// reading its item.
	log := filepath.Join(t.TempDir(), "args.log")
	t.Setenv("DECK_ARGS_LOG", log)
	if err := d.backlog.requestVerdict("example/app#2", "light"); err != nil {
		t.Fatal(err)
	}
	if judgeAll(d) != 0 || len(d.backlog.queue) != 1 || d.backlog.manual["example/app#2"] != "light" {
		t.Fatalf("queue %v after the budget ran out", d.backlog.queue)
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Error("the item was read with no budget left")
	}
}

func TestAutomaticBackupKeepsReserve(t *testing.T) {
	d, _, calls := newVerdictDeck(t, validVerdict)
	primary := d.models["light"][0]
	backup := primary
	backup.Model = "backup-model"
	d.models["light"] = []ModelConfig{primary, backup}
	run := d.runModel
	d.runModel = func(ctx context.Context, m ModelConfig, prompt string) ([]byte, error) {
		if m.Model == primary.Model {
			run(ctx, m, prompt)
			return nil, errors.New("primary failed on purpose")
		}
		return run(ctx, m, prompt)
	}
	d.cfg.Threads.CallsPerHour = 4
	d.calls = []time.Time{testClock, testClock} // two calls left, one above the reserve
	judgeAll(d)
	if len(*calls) != 1 || d.budgetLeft() != 1 {
		t.Fatalf("%d calls, %d left: the backup ate into the summaries' reserve", len(*calls), d.budgetLeft())
	}
}

func TestStatusTextIsCut(t *testing.T) {
	long := strings.Repeat("n", 3*itemChars)
	issue := BacklogIssue{Ref: "example/app#1", Status: &BacklogStatus{Next: &long, Owner: &long}}
	p, err := buildVerdictPrompt(issue, ItemText{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p, strings.Repeat("n", itemChars+1)) {
		t.Error("the status line is longer than the item limit")
	}
}

func TestVerdictFailures(t *testing.T) {
	d, _, _ := newVerdictDeck(t, `{"verdict":"perhaps"}`)
	judgeAll(d)
	e, _ := d.backlog.cache.get("example/app#1")
	if e.Verdict != nil || e.Error == "" || !e.RetryAt.IsZero() {
		t.Fatalf("invalid answer: %+v", e)
	}
	if judgeAll(d) != 0 {
		t.Error("an invalid answer was retried without a change")
	}

	d, _, calls := newVerdictDeck(t, validVerdict)
	t.Setenv("DECK_ITEM_FAIL", "1")
	judgeAll(d)
	e, _ = d.backlog.cache.get("example/app#1")
	if len(*calls) != 0 || !strings.Contains(e.Error, "item failed on purpose") || !e.RetryAt.Equal(testClock.Add(15*time.Minute)) {
		t.Fatalf("item failure: %d calls, %+v", len(*calls), e)
	}
}

func TestRefreshNowQueuesMissingVerdicts(t *testing.T) {
	d, _, _ := newVerdictDeck(t, validVerdict)
	if err := d.backlog.cache.put("example/app#1", VerdictEntry{
		Verdict: &Verdict{Verdict: "keep", Reason: "x"}, Basis: verdictBasis(d.backlog.inv.Issues[0]),
		AttemptBasis: verdictBasis(d.backlog.inv.Issues[0]), AttemptedAt: testClock, MadeAt: testClock,
	}); err != nil {
		t.Fatal(err)
	}
	// An automatic verdict still runs for lib#6 against its current basis.
	d.backlog.judging = "example/lib#6"
	for _, issue := range d.backlog.inv.Issues {
		if issue.Ref == "example/lib#6" {
			d.backlog.judgingBasis = verdictBasis(issue)
		}
	}
	d.backlog.requestRefreshAll()
	d.backlog.refresh(context.Background())
	if got := strings.Join(d.backlog.queue, " "); got != "example/app#2 example/lib#4" {
		t.Fatalf("queued %q", got)
	}
	// Once the issue has changed, the running verdict no longer covers it.
	d.backlog.queue, d.backlog.manual = nil, map[string]string{}
	d.backlog.judgingBasis = "an older basis"
	d.backlog.requestRefreshAll()
	d.backlog.refresh(context.Background())
	if got := strings.Join(d.backlog.queue, " "); got != "example/app#2 example/lib#6 example/lib#4" {
		t.Fatalf("queued %q after a change", got)
	}
}

func TestVerdictPageAndAction(t *testing.T) {
	d, h, _ := newVerdictDeck(t, validVerdict)
	judgeAll(d)
	body := get(t, h, "/backlog/").Body.String()
	for _, want := range []string{
		"<b>Verdict:</b> close: The importer PR merged.", "<b>Done when:</b> The importer runs.",
		`action="actions/verdict"`, "Refresh verdict", `<option value="light" selected>light</option>`,
		"verdict by anthropic/light-model (light)", "calls left this hour",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(body, "ISSUE-BODY-TEXT") {
		t.Error("the page shows issue text")
	}
	origin := "https://deck.example.com"
	if rec := post(h, "/backlog/actions/verdict", url.Values{"ref": {"example/app#1"}, "role": {"light"}}, origin); rec.Code != http.StatusSeeOther {
		t.Errorf("request: %d", rec.Code)
	}
	if len(d.backlog.queue) != 1 || d.backlog.queue[0] != "example/app#1" {
		t.Errorf("queue %v", d.backlog.queue)
	}
	for _, tt := range []struct {
		form url.Values
		code int
	}{
		{url.Values{"ref": {"gh:example/site#7"}}, http.StatusConflict},
		{url.Values{"ref": {"-x/y#1"}}, http.StatusBadRequest},
		{url.Values{"ref": {"example/app#1"}, "role": {"nosuch"}}, http.StatusBadRequest},
	} {
		if rec := post(h, "/backlog/actions/verdict", tt.form, origin); rec.Code != tt.code {
			t.Errorf("%v: %d, want %d", tt.form, rec.Code, tt.code)
		}
	}
	if rec := post(h, "/backlog/actions/verdict", url.Values{"ref": {"example/app#1"}}, "https://other.example.com"); rec.Code != http.StatusForbidden {
		t.Errorf("cross-origin: %d", rec.Code)
	}
}

func TestVerdictsOffWithoutItemCommand(t *testing.T) {
	d, h := newBacklogDeck(t)
	body := get(t, h, "/backlog/").Body.String()
	if !strings.Contains(body, "verdicts off: no item command is configured") || strings.Contains(body, "Refresh verdict") {
		t.Error("the page offers verdicts without an item command")
	}
	if rec := post(h, "/backlog/actions/verdict", url.Values{"ref": {"example/app#1"}}, "https://deck.example.com"); rec.Code != http.StatusConflict {
		t.Errorf("request: %d", rec.Code)
	}
	if d.backlog.judgeNext(context.Background()) {
		t.Error("a verdict ran without an item command")
	}
}

func TestPruneVerdicts(t *testing.T) {
	d, _, _ := newVerdictDeck(t, validVerdict)
	old := testClock.Add(-7 * time.Hour)
	d.backlog.cache.put("gone/repo#1", VerdictEntry{AttemptedAt: old, MadeAt: old})
	d.backlog.cache.put("gone/repo#2", VerdictEntry{AttemptedAt: testClock.Add(-time.Hour)})
	d.backlog.cache.put("example/app#1", VerdictEntry{AttemptedAt: old})
	// The fixture's GitHub source failed: nothing is pruned.
	d.backlog.refresh(context.Background())
	if _, ok := d.backlog.cache.get("gone/repo#1"); !ok {
		t.Fatal("an incomplete inventory pruned a verdict")
	}
	inv := *d.backlog.inv
	inv.Sources = map[string]string{"forgejo": "https://git.example.com", "github": "ok"}
	inv.Scope.Missing = nil
	if !inventoryComplete(inv) {
		t.Fatal("a complete inventory counts as incomplete")
	}
	d.backlog.pruneVerdicts(inv)
	for ref, want := range map[string]bool{"gone/repo#1": false, "gone/repo#2": true, "example/app#1": true} {
		if _, ok := d.backlog.cache.get(ref); ok != want {
			t.Errorf("%s kept = %v, want %v", ref, ok, want)
		}
	}
	reopened, err := openVerdictCache(d.backlog.cache.path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reopened.get("gone/repo#1"); ok {
		t.Error("the pruned entry is still on disk")
	}
}

// A verdict is made for the issue as picked: an inventory read that changes
// it during the call queues it again for the new basis, once.
func TestVerdictJudgesThePickedSnapshot(t *testing.T) {
	d, _, _ := newVerdictDeck(t, validVerdict)
	picked := verdictBasis(d.backlog.inv.Issues[0])
	run := d.runModel
	changed := false
	d.runModel = func(ctx context.Context, m ModelConfig, prompt string) ([]byte, error) {
		if !changed {
			changed = true
			d.backlog.mu.Lock()
			inv := *d.backlog.inv
			inv.Issues = append([]BacklogIssue(nil), inv.Issues...)
			inv.Issues[0].UpdatedAt = "2026-10-20T11:59:00Z"
			d.backlog.inv = &inv
			d.backlog.refreshVerdicts = true
			d.backlog.queueOutdatedLocked()
			d.backlog.mu.Unlock()
		}
		return run(ctx, m, prompt)
	}
	if !d.backlog.judgeNext(context.Background()) {
		t.Fatal("nothing was judged")
	}
	if e, _ := d.backlog.cache.get("example/app#1"); e.Basis != picked {
		t.Errorf("judged basis %q, want the picked %q", e.Basis, picked)
	}
	n := 0
	for _, ref := range d.backlog.queue {
		if ref == "example/app#1" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("the changed issue is queued %d times, want once", n)
	}
}

// A Refresh now asked for while a read runs waits for the next read, which
// sees what changed since the running one started.
func TestRefreshNowDuringARead(t *testing.T) {
	d, _, _ := newVerdictDeck(t, validVerdict)
	gate := filepath.Join(t.TempDir(), "go")
	t.Setenv("DECK_BACKLOG_WAIT", gate)
	done := make(chan struct{})
	go func() {
		d.backlog.refresh(context.Background())
		close(done)
	}()
	for {
		d.backlog.mu.Lock()
		running := d.backlog.running
		d.backlog.mu.Unlock()
		if running {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	d.backlog.requestRefreshAll()
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	<-done
	d.backlog.mu.Lock()
	pending, queued := d.backlog.refreshVerdicts, len(d.backlog.queue)
	d.backlog.mu.Unlock()
	if !pending || queued != 0 {
		t.Fatalf("after the running read: pending %v, %d queued", pending, queued)
	}
	d.backlog.refresh(context.Background())
	if len(d.backlog.queue) != 4 {
		t.Fatalf("the next read queued %d", len(d.backlog.queue))
	}
}
