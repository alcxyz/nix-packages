package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pagesConfig is a pages-only configuration with the backlog's path, before
// normalize.
func pagesConfig(t *testing.T, tools ...ToolConfig) Config {
	t.Helper()
	return Config{
		ContentDir:   t.TempDir(),
		PublicOrigin: "https://deck.example.com",
		Backlog:      &BacklogConfig{},
		Tools:        tools,
	}
}

func TestToolConfigValidation(t *testing.T) {
	for _, p := range []string{"/devstats/", "/ops/stats/", "/threadsx/", "/static-site/"} {
		cfg := pagesConfig(t, ToolConfig{Label: "Stats", Path: p, Description: "Work stats"})
		if err := cfg.normalize(); err != nil {
			t.Errorf("tool path %q refused: %v", p, err)
		}
	}
	for _, p := range []string{
		"", "devstats/", "/devstats", "/", "//", "/a b/", "/{x}/", "/a/../b/",
		"/threads/", "/threads/sub/", "/backlog/", "/backlog/x/", "/consent/", "/consent/x/",
		"/static/", "/static/x/", "/nav.json/", "/healthz/",
	} {
		cfg := pagesConfig(t, ToolConfig{Label: "Stats", Path: p})
		if err := cfg.normalize(); err == nil {
			t.Errorf("tool path %q accepted", p)
		}
	}
	for name, tools := range map[string][]ToolConfig{
		"same path":       {{Label: "A", Path: "/a/"}, {Label: "B", Path: "/a/"}},
		"nested path":     {{Label: "A", Path: "/a/"}, {Label: "B", Path: "/a/b/"}},
		"nesting path":    {{Label: "B", Path: "/a/b/"}, {Label: "A", Path: "/a/"}},
		"empty label":     {{Label: "", Path: "/a/"}},
		"padded label":    {{Label: " A", Path: "/a/"}},
		"two-line label":  {{Label: "A\nB", Path: "/a/"}},
		"two-line detail": {{Label: "A", Path: "/a/", Description: "one\ntwo"}},
	} {
		cfg := pagesConfig(t, tools...)
		if err := cfg.normalize(); err == nil {
			t.Errorf("%s accepted: %+v", name, tools)
		}
	}

	// With the dashboard at the root, its route names stay free.
	cfg := testConfig(t)
	cfg.BasePath = "/"
	for p, ok := range map[string]bool{"/devstats/": true, "/actions/": false, "/fragment/": false} {
		cfg.Tools = []ToolConfig{{Label: "T", Path: p}}
		if err := cfg.validateTools(); (err == nil) != ok {
			t.Errorf("basePath / with tool %s: %v", p, err)
		}
	}
}

func TestToolConfigFromTOML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	config := `publicOrigin = "https://deck.example.com"
contentDir = "/srv/pages"

[[tool]]
label = "Stats"
path = "/devstats/"
description = "Development activity"

[[tool]]
label = "Logs"
path = "/logs/"
`
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []ToolConfig{{Label: "Stats", Path: "/devstats/", Description: "Development activity"}, {Label: "Logs", Path: "/logs/"}}
	if len(cfg.Tools) != 2 || cfg.Tools[0] != want[0] || cfg.Tools[1] != want[1] {
		t.Errorf("tools: %+v", cfg.Tools)
	}
}

// navSection cuts the shell's nav element out of a page.
func navSection(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, `<nav class="deck-shell-nav"`)
	if start < 0 {
		t.Fatalf("no shell nav in %s", body)
	}
	nav := body[start:]
	return nav[:strings.Index(nav, "</nav>")]
}

func TestNavToolsAndPagesMenu(t *testing.T) {
	d, _ := newBacklogDeck(t)
	d.cfg.Tools = []ToolConfig{{Label: "Stats", Path: "/devstats/", Description: "Development activity"}}
	writeContentPage(t, d.cfg.ContentDir, "notes/a", "A", "2026-10-01", "")
	writeContentPage(t, d.cfg.ContentDir, "live/b", "B", "2026-10-02", "")
	h := contentTestHandler(t, d, nil)

	// /nav.json keeps every item in order, with the flags the shell needs.
	rec := get(t, h, "/nav.json")
	var raw struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, item := range raw.Items {
		flag := ""
		if item["tool"] == true {
			flag = " tool"
		}
		if item["collection"] == true {
			flag = " collection"
		}
		got = append(got, item["label"].(string)+" "+item["url"].(string)+flag)
	}
	want := []string{"Overview /", "Threads /threads/", "Backlog /backlog/", "Stats /devstats/ tool", "Live /live/ collection", "Notes /notes/ collection"}
	if strings.Join(got, ", ") != strings.Join(want, ", ") {
		t.Errorf("nav.json items:\n got %v\nwant %v", got, want)
	}

	s := &Server{deck: d, log: quietLog()}
	view := s.nav("/devstats/graphs/")
	if links := view.Links(); len(links) != 4 || !links[3].Tool || !links[3].Current {
		t.Errorf("the tool is a flat link, current below its path: %+v", links)
	}
	if pages := view.Pages(); len(pages) != 2 || view.PagesCurrent() {
		t.Errorf("the collections go to the menu, which is not current here: %+v", pages)
	}
	if view := s.nav("/notes/a/"); !view.PagesCurrent() || !view.Pages()[1].Current {
		t.Errorf("a page in a collection marks the menu and its collection: %+v", view)
	}

	// The server-rendered shell: flat links, then the Pages menu.
	nav := navSection(t, get(t, h, "/notes/").Body.String())
	links := nav[:strings.Index(nav, "<details")]
	menu := nav[strings.Index(nav, "<details"):]
	if !strings.Contains(links, `<a href="/devstats/" class="tool">Stats</a>`) || strings.Contains(links, `href="/notes/"`) {
		t.Errorf("the bar should link the tool and not the collections: %s", links)
	}
	if strings.Index(links, `href="/backlog/"`) > strings.Index(links, `href="/devstats/"`) {
		t.Errorf("the tool should follow the views: %s", links)
	}
	if !strings.HasPrefix(menu, `<details class="deck-shell-menu"><summary aria-current="true">Pages</summary><div class="deck-shell-menu-list">`) ||
		!strings.Contains(menu, `<a href="/notes/" aria-current="page">Notes</a>`) || !strings.Contains(menu, `<a href="/live/">Live</a>`) {
		t.Errorf("the menu should hold the collections, with the current one marked: %s", menu)
	}
	if strings.Count(nav, `aria-current="page"`) != 1 {
		t.Errorf("one current page: %s", nav)
	}
	nav = navSection(t, get(t, h, "/").Body.String())
	if !strings.Contains(nav, `<details class="deck-shell-menu"><summary>Pages</summary>`) {
		t.Errorf("the menu is not current on the overview: %s", nav)
	}

	// Every server-rendered page loads shell.js for the menu.
	for _, path := range []string{"/", "/notes/", "/threads/", "/backlog/"} {
		if !strings.Contains(get(t, h, path).Body.String(), `static/shell.js" defer></script>`) {
			t.Errorf("%s does not load shell.js", path)
		}
	}
}

func TestNavWithoutCollectionsHasNoMenu(t *testing.T) {
	d, _ := newTestDeck(t)
	h := contentTestHandler(t, d, nil)
	if nav := navSection(t, get(t, h, "/").Body.String()); strings.Contains(nav, "<details") || strings.Contains(nav, "Pages") {
		t.Errorf("no collections, no menu: %s", nav)
	}
}

func TestToolPathIsNoCollection(t *testing.T) {
	d := newPagesDeck(t)
	d.cfg.Tools = []ToolConfig{{Label: "Stats", Path: "/devstats/"}, {Label: "Logs", Path: "/ops/logs/"}}
	writeContentPage(t, d.cfg.ContentDir, "devstats/a", "Shadow", "2026-10-01", "")
	writeContentPage(t, d.cfg.ContentDir, "ops/b", "Nested shadow", "2026-10-01", "")
	writeContentPage(t, d.cfg.ContentDir, "notes/c", "C", "2026-10-01", "")
	h := contentTestHandler(t, d, nil)

	for _, path := range []string{"/devstats/", "/devstats/a/", "/devstats/a/index.html", "/ops/", "/ops/b/", "/ops/logs/"} {
		if rec := get(t, h, path); rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404: a tool's path is no collection", path, rec.Code)
		}
	}
	if rec := get(t, h, "/devstats"); rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != "/devstats/" {
		t.Errorf("/devstats: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if rec := get(t, h, "/notes/c/"); rec.Code != http.StatusOK {
		t.Errorf("/notes/c/: %d", rec.Code)
	}
	index := get(t, h, "/").Body.String()
	if strings.Contains(index, "Shadow") || strings.Contains(index, `href="/ops/"`) {
		t.Errorf("a tool's path is listed as a collection: %s", index)
	}
	var nav NavView
	if err := json.Unmarshal(get(t, h, "/nav.json").Body.Bytes(), &nav); err != nil {
		t.Fatal(err)
	}
	for _, item := range nav.Items {
		if item.Collection && item.URL != "/notes/" {
			t.Errorf("a tool's path is in the menu: %+v", item)
		}
	}
}

func TestOverviewListsTools(t *testing.T) {
	tools := []ToolConfig{{Label: "Stats", Path: "/devstats/", Description: "Development activity"}, {Label: "Logs", Path: "/logs/"}}
	pages := newPagesDeck(t)
	dashboard, _ := newTestDeck(t)
	for mode, d := range map[string]*Deck{"pages-only": pages, "dashboard": dashboard} {
		d.cfg.Tools = tools
		body := get(t, contentTestHandler(t, d, nil), "/").Body.String()
		section := body[strings.Index(body, `id="ov-tools"`):]
		for _, want := range []string{
			`<a class="ov-tool" href="/devstats/"><b>Stats</b><span>Development activity</span></a>`,
			`<a class="ov-tool" href="/logs/"><b>Logs</b></a>`,
		} {
			if !strings.Contains(section, want) {
				t.Errorf("%s overview lacks %s: %s", mode, want, section)
			}
		}
	}
	d := newPagesDeck(t)
	if strings.Contains(get(t, contentTestHandler(t, d, nil), "/").Body.String(), "ov-tools") {
		t.Error("an overview without tools has a Tools section")
	}
}
