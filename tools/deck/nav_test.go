package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeTilePage writes a page bundle that declares a tile, with an ok
// tile.json.
func writeTilePage(t *testing.T, root, name, title, date string) {
	t.Helper()
	writeContentPage(t, root, name, title, date, "Summary of "+title)
	writeContentFile(t, root, name+"/metadata.json", `{"title": "`+title+`", "date": "`+date+`", "summary": "Summary of `+title+`", "tile": true}`)
	writeTileStatus(t, root, name, "ok")
}

// writeTileStatus writes the page's tile.json with the status and a
// headline naming it.
func writeTileStatus(t *testing.T, root, name, status string) {
	t.Helper()
	writeContentFile(t, root, name+"/tile.json", `{"status": "`+status+`", "headline": "Status `+status+`", "updated": null, "staleAfter": 15}`)
}

func TestNavJSON(t *testing.T) {
	d, _ := newBacklogDeck(t)
	d.cfg.Consent = &ConsentConfig{SpoolDir: t.TempDir()}
	writeConsentRequest(t, d.cfg.Consent.SpoolDir, "0123456789abcdef0123456789abcdef", "Read a token", time.Now().Add(time.Hour))
	refreshAndProbe(d, d.cfg.Threads.Instances[0])
	writeContentPage(t, d.cfg.ContentDir, "notes/a", "A", "2026-10-01", "")
	writeTilePage(t, d.cfg.ContentDir, "live/runners", "Runners", "2026-10-02")
	// A directory with no valid page is no collection, here or on the index.
	if err := os.MkdirAll(filepath.Join(d.cfg.ContentDir, "empty"), 0o700); err != nil {
		t.Fatal(err)
	}
	h := contentTestHandler(t, d, nil)

	rec := get(t, h, "/nav.json")
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("nav.json: %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	var nav NavView
	if err := json.Unmarshal(rec.Body.Bytes(), &nav); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, item := range nav.Items {
		got = append(got, item.Label+" "+item.URL)
	}
	want := []string{"Overview /", "Threads /threads/", "Backlog /backlog/", "Approvals /consent/", "Live /live/", "Notes /notes/"}
	if strings.Join(got, ", ") != strings.Join(want, ", ") {
		t.Errorf("nav items:\n got %v\nwant %v", got, want)
	}
	if threads := nav.Items[1]; threads.Count == 0 || threads.Tone != "attn" {
		t.Errorf("threads should count what waits on the owner: %+v", threads)
	}
	if approvals := nav.Items[3]; approvals.Count != 1 || approvals.Tone != "attn" {
		t.Errorf("approvals should count the pending request: %+v", approvals)
	}
	if !nav.Items[4].Collection || nav.Items[2].Collection {
		t.Errorf("only collections are marked as such: %+v", nav.Items)
	}

	// Deck's own pages render the same shell, with the current page marked.
	for path, current := range map[string]string{"/": "Overview", "/threads/": "Threads", "/backlog/": "Backlog", "/consent/": "Approvals", "/notes/": "Notes"} {
		body := get(t, h, path).Body.String()
		if !strings.Contains(body, `class="deck-shell deck-noprint"`) {
			t.Errorf("%s has no shell", path)
			continue
		}
		nav := body[strings.Index(body, `<nav class="deck-shell-nav"`):]
		nav = nav[:strings.Index(nav, "</nav>")]
		if strings.Count(nav, `aria-current="page"`) != 1 || !strings.Contains(nav, `aria-current="page">`+current) {
			t.Errorf("%s marks the wrong item: %s", path, nav)
		}
	}
}

func TestNavPagesOnly(t *testing.T) {
	d := newPagesDeck(t)
	writeContentPage(t, d.cfg.ContentDir, "notes/a", "A", "2026-10-01", "")
	h := contentTestHandler(t, d, nil)
	var nav NavView
	if err := json.Unmarshal(get(t, h, "/nav.json").Body.Bytes(), &nav); err != nil {
		t.Fatal(err)
	}
	for _, item := range nav.Items {
		if item.Count != 0 {
			t.Errorf("the pages-only mode has no counts: %+v", item)
		}
	}
	if len(nav.Items) != 4 || nav.Items[1].URL != "/threads/" || nav.Items[2].URL != "/backlog/" {
		t.Errorf("the pages-only mode links its views: %+v", nav.Items)
	}
	body := get(t, h, "/").Body.String()
	if strings.Contains(body, "Needs you") {
		t.Errorf("the pages-only overview has no counts to show: %s", body)
	}
}

func TestNavWithDashboardAtRoot(t *testing.T) {
	d, _ := newTestDeck(t)
	d.cfg.BasePath = "/"
	s := &Server{deck: d, log: quietLog()}
	items := s.nav("/").Items
	if items[0].Label != "Threads" || !items[0].Current {
		t.Errorf("with the dashboard at the root there is no overview, and the root is Threads: %+v", items)
	}
}

func TestOverviewNeedsAndTiles(t *testing.T) {
	d, _ := newBacklogDeck(t)
	refreshAndProbe(d, d.cfg.Threads.Instances[0])
	writeContentPage(t, d.cfg.ContentDir, "notes/a", "Plain page", "2026-10-01", "")
	writeTilePage(t, d.cfg.ContentDir, "live/runners", "Runners", "2026-10-02")
	writeContentPage(t, d.cfg.ContentDir, "live/extra", "Extra live page", "2026-10-03", "")
	// A tile.js no longer makes a tile: metadata declares it.
	writeContentPage(t, d.cfg.ContentDir, "live/script", "Script", "2026-10-04", "")
	writeContentFile(t, d.cfg.ContentDir, "live/script/tile.js", `export default async () => ({});`)
	h := contentTestHandler(t, d, nil)

	body := get(t, h, "/").Body.String()
	for _, want := range []string{
		"Needs you", `href="/threads/#card=you"`, `href="/threads/#card=stalled"`, `href="/backlog/#card=ready"`,
		`<a class="deck-tile" href="/live/runners/" data-status="ok">`, "Status ok", `src="/static/live.js"`,
		`href="/live/">All 3 live pages`, `data-deck-live="tiles"`, `data-deck-live="needs"`, `data-deck-live="nav"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("overview lacks %s", want)
		}
	}
	for _, unwanted := range []string{"data-tile", "tiles.js", `href="/live/script/" data-status`} {
		if strings.Contains(body, unwanted) {
			t.Errorf("overview has %s", unwanted)
		}
	}
	// The library leaves the collection with tiles to its link.
	library := body[strings.Index(body, `id="ov-library"`):]
	if !strings.Contains(library, `href="/notes/"`) || strings.Contains(library, `href="/live/"`) {
		t.Errorf("library should list notes and not live: %s", library)
	}

	collection := get(t, h, "/live/").Body.String()
	tile := strings.Index(collection, `<a class="deck-tile" href="/live/runners/"`)
	extra := strings.Index(collection, `href="/live/extra/"`)
	if tile < 0 || extra < 0 || tile > extra || !strings.Contains(collection, `src="/static/live.js"`) {
		t.Errorf("collection index should show the tile, then the other pages: %s", collection)
	}
	if strings.Contains(get(t, h, "/notes/").Body.String(), "live.js") {
		t.Error("a collection without tiles loads live.js")
	}
	if strings.Contains(get(t, h, "/threads/").Body.String(), "live.js") {
		t.Error("the thread dashboard loads live.js")
	}
	for _, p := range []string{"/static/live.js", "/static/shell.js"} {
		if rec := get(t, h, p); rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), "javascript") {
			t.Errorf("%s: %d %s", p, rec.Code, rec.Header().Get("Content-Type"))
		}
	}
	if rec := get(t, h, "/static/tiles.js"); rec.Code != http.StatusNotFound {
		t.Errorf("tiles.js is gone: %d", rec.Code)
	}
}

func TestOverviewNeedsBeforeFacts(t *testing.T) {
	d, _ := newTestDeck(t)
	writeContentPage(t, d.cfg.ContentDir, "notes/a", "A", "2026-10-01", "")
	h := contentTestHandler(t, d, nil)
	body := get(t, h, "/").Body.String()
	if !strings.Contains(body, "<b>…</b><span>threads wait on you</span>") {
		t.Errorf("counts not read yet should show as unknown, not zero: %s", body)
	}
}

func TestNavPathIsNoBasePath(t *testing.T) {
	for _, field := range []string{"basePath", "backlog.basePath"} {
		if _, err := normalizeBasePath(field, "/nav.json", ""); err == nil {
			t.Errorf("%s /nav.json/ should be refused: it would shadow the shell's links", field)
		}
	}
}

func TestThreadCountsNeedEveryInstance(t *testing.T) {
	d, _ := newTestDeck(t)
	second := d.cfg.Threads.Instances[0]
	second.Name = "second"
	d.cfg.Threads.Instances = append(d.cfg.Threads.Instances, second)
	d.facts["main"].grouped = &Grouped{Summary: GroupSummary{User: 2}}
	if _, ok := d.threadCounts(); ok {
		t.Error("counts read from one of two instances passed for the whole")
	}
	d.facts["second"] = &instanceFacts{grouped: &Grouped{Summary: GroupSummary{User: 1}}}
	if sum, ok := d.threadCounts(); !ok || sum.User != 3 {
		t.Errorf("counts from every instance: %+v %v", sum, ok)
	}
}
