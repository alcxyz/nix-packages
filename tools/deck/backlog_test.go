package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newBacklogDeck is a test deck with the backlog view on the fake
// inventory command, read once.
func newBacklogDeck(t *testing.T) (*Deck, http.Handler) {
	t.Helper()
	d, _ := newTestDeck(t)
	d.cfg.Backlog = &BacklogConfig{Commands: BacklogCommandsConfig{Inventory: fakeBin(t, "backlog-inventory")}}
	if err := d.cfg.Backlog.normalize(d.cfg.BasePath); err != nil {
		t.Fatal(err)
	}
	d.backlog = newBacklog(*d.cfg.Backlog, quietLog())
	d.backlog.deck = d
	d.backlog.now = func() time.Time { return testClock }
	d.backlog.refresh(context.Background())
	return d, contentTestHandler(t, d, nil)
}

func TestBacklogConfig(t *testing.T) {
	dir := t.TempDir()
	write := func(extra string) string {
		p := filepath.Join(dir, "deck.toml")
		top := ""
		if rest, ok := strings.CutPrefix(extra, "basePath = \"/\"\n"); ok {
			top, extra = "basePath = \"/\"\n", rest
		}
		body := top + "publicOrigin = \"https://deck.example.com\"\n" +
			"[[threads.instance]]\nname = \"main\"\ndb = \"/x.sqlite\"\nwebURL = \"https://t3.example.com\"\n" + extra
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cfg, err := loadConfig(write(""))
	if err != nil || cfg.Backlog != nil {
		t.Fatalf("without [backlog]: %v, %+v", err, cfg.Backlog)
	}
	cfg, err = loadConfig(write("[backlog.commands]\ninventory = \"deck-backlog-inventory\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if b := cfg.Backlog; b.BasePath != "/backlog/" || b.IntervalMinutes != 15 || b.TimeoutMinutes != 5 || b.Commands.Inventory != "deck-backlog-inventory" {
		t.Fatalf("defaults: %+v", b)
	}
	for _, tt := range []struct{ extra, want string }{
		{"[backlog]\n", "backlog.commands.inventory is required"},
		{"[backlog]\nbasePath = \"/threads/backlog/\"\n[backlog.commands]\ninventory = \"x\"\n", "must not overlap"},
		{"basePath = \"/\"\n[backlog.commands]\ninventory = \"x\"\n", "must not overlap"},
		{"[backlog]\nbasePath = \"/static/backlog/\"\n[backlog.commands]\ninventory = \"x\"\n", "shared asset path"},
		{"[backlog]\nbasePath = \"/{x}/\"\n[backlog.commands]\ninventory = \"x\"\n", "backlog.basePath"},
		{"[backlog]\nintervalMinutes = -1\n[backlog.commands]\ninventory = \"x\"\n", "at least 1"},
		{"[backlog]\nshortlist = 3\n[backlog.commands]\ninventory = \"x\"\n", "unknown keys"},
	} {
		if _, err := loadConfig(write(tt.extra)); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%q: error %v, want %q", tt.extra, err, tt.want)
		}
	}
}

func TestShortlistLimits(t *testing.T) {
	var issues []BacklogIssue
	for i := 1; i <= 30; i++ {
		issues = append(issues, BacklogIssue{Ref: fmt.Sprintf("o/r#%d", i), Lane: "close"})
	}
	issues = append(issues, BacklogIssue{Ref: "o/r#100", Lane: "inflight", Flags: []string{"untouched-since-merge"}})
	for i := 1; i <= 15; i++ {
		issues = append(issues, BacklogIssue{Ref: fmt.Sprintf("o/s#%d", i), Lane: "stale", IdleDays: 50 + i})
	}
	issues = append(issues, BacklogIssue{Ref: "o/s#200", Lane: "stale", IdleDays: 999, Flags: []string{"related-closed"}})
	maybeDone, stale := shortlist(issues)
	if len(maybeDone) != shortlistMaybeDone || maybeDone[0].Ref != "o/r#1" || maybeDone[23].Ref != "o/r#24" {
		t.Fatalf("maybe done: %d rows, first %s", len(maybeDone), maybeDone[0].Ref)
	}
	for _, issue := range maybeDone {
		if issue.Ref == "o/r#100" || issue.Ref == "o/s#200" {
			t.Fatalf("%s should not be in the first 24", issue.Ref)
		}
	}
	if len(stale) != shortlistStale || stale[0].Ref != "o/s#15" || stale[11].Ref != "o/s#4" {
		t.Fatalf("stale: %d rows, first %s, last %s", len(stale), stale[0].Ref, stale[len(stale)-1].Ref)
	}
	// A flagged stale issue counts as maybe done, not stale, once there is room.
	maybeDone, stale = shortlist([]BacklogIssue{{Ref: "o/s#200", Lane: "stale", Flags: []string{"related-closed"}}})
	if len(maybeDone) != 1 || len(stale) != 0 {
		t.Fatalf("flagged stale issue: %d maybe done, %d stale", len(maybeDone), len(stale))
	}
}

func TestBacklogPage(t *testing.T) {
	_, h := newBacklogDeck(t)
	rec := get(t, h, "/backlog/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	for _, raw := range []string{"<script>alert(1)", "javascript:", "<b>done</b>"} {
		if strings.Contains(body, raw) {
			t.Errorf("page holds unescaped or unsafe %q", raw)
		}
	}
	for _, want := range []string{
		"<title>Backlog</title>",
		`data-base="/backlog/"`,
		`data-visit-key="deck.lastVisit.backlog"`,
		`src="/backlog/static/deck.js"`,
		`action="actions/refresh-all"`,
		"Fix &lt;script&gt;alert(1)&lt;/script&gt; in titles",
		`href="https://git.example.com/example/app/issues/1"`,
		"Maybe done", "Close?", "Check whether it is done",
		"Stale", "Drop, refresh or dispatch?",
		"Next: decide whether to keep it",
		"other/tool#5 (closed): Tool support &lt;b&gt;done&lt;/b&gt;",
		"Ready to merge", "Merge", "Renovate",
		"Other issues",
		"github: error: gh api: HTTP 403",
		"1 scoped repositories not found",
		"read as example-agent",
		`data-key="sub:prs:Renovate"`, `data-key="sub:issues:`,
		"<b>7</b> open issues", "<b>2</b> maybe done", "<b>2</b> stale", "<b>1</b> ready to merge",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	// The dashboard issue is left out, the thread's issue is not shortlisted.
	if strings.Contains(body, "Dependency dashboard") {
		t.Error("the dashboard issue is shown")
	}
	if i, j := strings.Index(body, "Held by a thread"), strings.Index(body, "Other issues"); i < j {
		t.Error("an issue a thread holds was shortlisted")
	}
	// The older stale issue comes first.
	if strings.Index(body, "Older idea") > strings.Index(body, "Old idea") {
		t.Error("stale issues are not idlest first")
	}
	frag := get(t, h, "/backlog/fragment")
	if frag.Code != http.StatusOK || strings.Contains(frag.Body.String(), "<html") || !strings.Contains(frag.Body.String(), "Maybe done") {
		t.Errorf("fragment %d", frag.Code)
	}
	if rec := get(t, h, "/backlog"); rec.Code != http.StatusMovedPermanently {
		t.Errorf("redirect %d", rec.Code)
	}
	if rec := get(t, h, "/backlog/static/deck.css"); rec.Code != http.StatusOK {
		t.Errorf("static %d", rec.Code)
	}
	if rec := get(t, h, "/backlog/actions/refresh-all"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET action %d", rec.Code)
	}
	if rec := post(h, "/backlog/actions/refresh-all", nil, "https://other.example.com"); rec.Code != http.StatusForbidden {
		t.Errorf("cross-origin action %d", rec.Code)
	}
	if rec := get(t, h, "/backlog/elsewhere"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown backlog path %d", rec.Code)
	}
}

func TestBacklogRefreshAction(t *testing.T) {
	d, h := newBacklogDeck(t)
	rec := post(h, "/backlog/actions/refresh-all", nil, "https://deck.example.com")
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/backlog/" {
		t.Fatalf("plain form: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	select {
	case <-d.backlog.refreshCh:
	default:
		t.Fatal("no refresh was requested")
	}
}

func TestBacklogKeepsLastGoodFacts(t *testing.T) {
	d, h := newBacklogDeck(t)
	t.Setenv("DECK_BACKLOG_FAIL", "1")
	d.backlog.refresh(context.Background())
	body := get(t, h, "/backlog/").Body.String()
	if !strings.Contains(body, "Ship the importer") || !strings.Contains(body, "backlog inventory failed on purpose") {
		t.Error("a failed refresh lost the facts or its error")
	}

	fresh := newBacklog(*d.cfg.Backlog, quietLog())
	fresh.refresh(context.Background())
	if v := fresh.view("/backlog/"); v.HasRun || !v.Empty || v.Err == "" {
		t.Errorf("first refresh failing: %+v", v)
	}
}

func TestBacklogFactsDatedFromReadStart(t *testing.T) {
	d, _ := newBacklogDeck(t)
	b := newBacklog(*d.cfg.Backlog, quietLog())
	clock := testClock
	b.now = func() time.Time {
		clock = clock.Add(time.Minute)
		return clock
	}
	b.refresh(context.Background())
	v := b.view("/backlog/")
	if want := testClock.Add(time.Minute); !v.AsOf.Equal(want) || !v.SeenUpTo.Equal(want) {
		t.Errorf("as of %s, seen up to %s, want the read's start %s", v.AsOf, v.SeenUpTo, want)
	}
}

func TestBacklogRejectsBadRefs(t *testing.T) {
	for _, ref := range []string{"-o/r#1", "o/r#0", "o/r", "o/r#1 x", "gh:o#1", "o/r/s#1"} {
		data := fmt.Sprintf(`{"issues":[{"ref":%q}]}`, ref)
		if _, err := parseBacklogInventory([]byte(data)); err == nil {
			t.Errorf("ref %q accepted", ref)
		}
	}
	if _, err := parseBacklogInventory([]byte(`{"issues":[{"ref":"gh:o.x/r-y#12"}],"prs":[{"ref":"o/r#3"}]}`)); err != nil {
		t.Error(err)
	}
}

// Without [backlog], its path is the content library's, as in a process
// that does not serve the view; with it, a collection of that name is
// hidden.
func TestBacklogPathOnlyWhenConfigured(t *testing.T) {
	d, _ := newTestDeck(t)
	writeContentPage(t, d.cfg.ContentDir, "backlog/example", "Example", "2026-10-01", "")
	h := contentTestHandler(t, d, nil)
	if rec := get(t, h, "/backlog/"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Example") {
		t.Errorf("collection without the view: %d", rec.Code)
	}
	if body := get(t, h, "/").Body.String(); strings.Contains(body, "Dashboard</span><h2>Backlog") {
		t.Error("root index links a backlog view that is off")
	}

	d, h = newBacklogDeck(t)
	writeContentPage(t, d.cfg.ContentDir, "backlog/example", "Example", "2026-10-01", "")
	if rec := get(t, h, "/backlog/example/"); rec.Code != http.StatusNotFound {
		t.Errorf("collection under the view's path: %d", rec.Code)
	}
	body := get(t, h, "/").Body.String()
	if !strings.Contains(body, `href="/backlog/"`) || strings.Contains(body, "lib-recent") {
		t.Error("root index lacks the view's card, or lists the hidden collection")
	}
}
