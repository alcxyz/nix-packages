package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newPagesDeck builds a pages-only deck: no thread instances, the
// dashboard views' paths configured to link and reserve them.
func newPagesDeck(t *testing.T) *Deck {
	t.Helper()
	cfg := Config{
		Listen:       "127.0.0.1:0",
		ContentDir:   t.TempDir(),
		PublicOrigin: "https://deck.example.com",
		Backlog:      &BacklogConfig{},
	}
	if err := cfg.normalize(); err != nil {
		t.Fatal(err)
	}
	if !cfg.pagesOnly() {
		t.Fatal("not pages-only")
	}
	return newDeck(cfg, nil, nil, nil, quietLog())
}

func TestPagesOnlyRoutes(t *testing.T) {
	d := newPagesDeck(t)
	writeContentPage(t, d.cfg.ContentDir, "notes/example", "Example", "2026-10-01", "")
	// Directories named like the reserved paths are never collections.
	for _, name := range []string{"threads", "backlog", "static", "healthz"} {
		writeContentPage(t, d.cfg.ContentDir, name+"/example", "Reserved "+name, "2026-10-01", "")
	}
	h := contentTestHandler(t, d, nil)

	for _, p := range []string{"/", "/notes/", "/notes/example/", "/notes/example/style.css", "/static/theme.css", "/static/theme.js", "/static/base.css", "/static/library.css", "/healthz"} {
		if rec := get(t, h, p); rec.Code != http.StatusOK {
			t.Errorf("served %s: %d", p, rec.Code)
		}
	}
	body := get(t, h, "/").Body.String()
	for _, want := range []string{`href="/threads/"`, `href="/backlog/"`, `href="/notes/"`} {
		if !strings.Contains(body, want) {
			t.Errorf("root index lacks %s", want)
		}
	}
	if strings.Contains(body, "Reserved") {
		t.Errorf("root index lists a reserved path: %s", body)
	}

	// The dashboard views' paths belong to the dashboard process.
	for _, p := range []string{"/threads/", "/threads/fragment", "/threads/static/deck.css", "/threads/example/", "/backlog/", "/backlog/fragment", "/backlog/static/deck.css", "/static/example/", "/healthz/example/"} {
		if rec := get(t, h, p); rec.Code != http.StatusNotFound {
			t.Errorf("reserved %s: %d", p, rec.Code)
		}
	}
	for _, p := range []string{"/threads/actions/refresh", "/threads/actions/refresh-all", "/threads/actions/ack", "/threads/actions/forget", "/backlog/actions/refresh-all", "/backlog/actions/verdict"} {
		if rec := post(h, p, nil, d.cfg.PublicOrigin); rec.Code != http.StatusNotFound {
			t.Errorf("action %s: %d", p, rec.Code)
		}
	}
	// The bare paths redirect to the slash form the proxy routes.
	for _, p := range []string{"/threads", "/backlog"} {
		rec := get(t, h, p)
		if rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != p+"/" {
			t.Errorf("bare %s: %d %q", p, rec.Code, rec.Header().Get("Location"))
		}
	}
}

func TestPagesOnlyRunsNothing(t *testing.T) {
	d := newPagesDeck(t)
	if d.backlog != nil {
		t.Fatal("pages-only deck has a backlog view")
	}
	ctx, cancel := context.WithCancel(context.Background())
	d.run(ctx)
	cancel()
	if !d.wait(time.Second) {
		t.Fatal("background work did not stop")
	}
}

func TestPagesOnlyAuth(t *testing.T) {
	d := newPagesDeck(t)
	writeContentPage(t, d.cfg.ContentDir, "notes/example", "Example", "2026-10-01", "")
	ti := newTestIssuer(t)
	auth := newAuthenticator(AuthConfig{Issuer: ti.srv.URL, ClientID: "deck-client", Group: "deck-users"}, ti.srv.Client(), quietLog())
	h := contentTestHandler(t, d, auth)
	for _, p := range []string{"/", "/notes/example/", "/static/theme.css", "/threads/"} {
		if rec := get(t, h, p); rec.Code != http.StatusUnauthorized {
			t.Errorf("unauthenticated %s: %d", p, rec.Code)
		}
	}
	if rec := get(t, h, "/healthz"); rec.Code != http.StatusOK {
		t.Errorf("healthz: %d", rec.Code)
	}
	token := ti.sign(t, ti.key, ti.claims(nil))
	for _, p := range []string{"/", "/notes/example/"} {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("authenticated %s: %d", p, rec.Code)
		}
	}
}

// A directory with no valid bundle, such as a checkout's docs/, is not
// listed as a collection.
func TestRootIndexLeavesOutCollectionsWithoutBundles(t *testing.T) {
	d := newPagesDeck(t)
	writeContentPage(t, d.cfg.ContentDir, "notes/example", "Example", "2026-10-01", "")
	writeContentFile(t, d.cfg.ContentDir, "docs/readme.md", "# Notes")
	writeContentFile(t, d.cfg.ContentDir, "drafts/unfinished/index.html", "Invalid")
	h := contentTestHandler(t, d, nil)
	body := get(t, h, "/").Body.String()
	if !strings.Contains(body, `href="/notes/"`) {
		t.Errorf("valid collection missing: %s", body)
	}
	for _, name := range []string{"docs", "drafts"} {
		if strings.Contains(body, `href="/`+name+`/"`) {
			t.Errorf("collection without bundles listed: %s", name)
		}
	}
}

// A sync process replaces the checkout by swapping a symlink; deck opens
// the content root per request, so it serves the new revision at once.
func TestContentRootResolvedPerRequest(t *testing.T) {
	d := newPagesDeck(t)
	dir := t.TempDir()
	writeContentPage(t, filepath.Join(dir, "rev1"), "notes/first", "First revision", "2026-10-01", "")
	writeContentPage(t, filepath.Join(dir, "rev2"), "notes/second", "Second revision", "2026-10-02", "")
	link := filepath.Join(dir, "current")
	if err := os.Symlink("rev1", link); err != nil {
		t.Fatal(err)
	}
	d.cfg.ContentDir = link
	h := contentTestHandler(t, d, nil)
	if body := get(t, h, "/").Body.String(); !strings.Contains(body, "First revision") {
		t.Fatalf("first revision not served: %s", body)
	}
	next := filepath.Join(dir, "next")
	if err := os.Symlink("rev2", next); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, link); err != nil {
		t.Fatal(err)
	}
	body := get(t, h, "/").Body.String()
	if !strings.Contains(body, "Second revision") || strings.Contains(body, "First revision") {
		t.Fatalf("second revision not served: %s", body)
	}
	if rec := get(t, h, "/notes/second/"); rec.Code != http.StatusOK {
		t.Errorf("new page: %d", rec.Code)
	}
	if rec := get(t, h, "/notes/first/"); rec.Code != http.StatusNotFound {
		t.Errorf("old page: %d", rec.Code)
	}
}

func TestDashboardWithCollectionsOff(t *testing.T) {
	d, _ := newTestDeck(t)
	off := false
	d.cfg.Collections = &off
	writeContentPage(t, d.cfg.ContentDir, "notes/example", "Example", "2026-10-01", "")
	refreshAndProbe(d, d.cfg.Threads.Instances[0])
	h := contentTestHandler(t, d, nil)
	for _, p := range []string{"/threads/", "/threads/static/deck.css", "/healthz"} {
		if rec := get(t, h, p); rec.Code != http.StatusOK {
			t.Errorf("dashboard %s: %d", p, rec.Code)
		}
	}
	for _, p := range []string{"/", "/notes/", "/notes/example/"} {
		if rec := get(t, h, p); rec.Code != http.StatusNotFound {
			t.Errorf("collections off %s: %d", p, rec.Code)
		}
	}
}

func TestLoadPagesOnlyConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	noBacklog := `publicOrigin = "https://deck.example.com"
contentDir = "/srv/deck/current"
[auth]
issuer = "https://id.example.com"
clientID = "deck"
group = "deck-users"
`
	good := noBacklog + `[backlog]
basePath = "/backlog/"
`
	if err := os.WriteFile(path, []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.pagesOnly() || !*cfg.Collections || cfg.BasePath != "/threads/" || cfg.Backlog.BasePath != "/backlog/" {
		t.Fatalf("pages-only config: %+v", cfg)
	}
	bad := map[string]string{
		"root base":        `basePath = "/"` + "\n" + noBacklog,
		"mixed case table": noBacklog + "[Backlog]\nbasePath = \"/backlog/\"\nintervalMinutes = 0\n",
		"nothing to serve": "collections = false\n" + good,
		"backlog commands": good + "[backlog.commands]\ninventory = \"backlog-inventory\"\n",
		"empty commands":   good + "[backlog.commands]\ninventory = \"\"\n",
		"zero interval":    good + "intervalMinutes = 0\n",
		"empty role":       good + "verdictRole = \"\"\n",
		"backlog interval": good + "intervalMinutes = 30\n",
		"backlog timeout":  good + "timeoutMinutes = 2\n",
		"verdict role":     good + "verdictRole = \"deep\"\n",
		"verdict effort":   good + "verdictEffort = \"\"\n",
		"verdict interval": good + "verdictIntervalMinutes = 0\n",
		"overlapping":      `basePath = "/backlog/x/"` + "\n" + good,
	}
	for name, content := range bad {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadConfig(path); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
