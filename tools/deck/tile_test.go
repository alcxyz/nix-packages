package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func readTestTile(t *testing.T, dir string, now time.Time) (Tile, error) {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	return readTile(root, now)
}

func TestReadTile(t *testing.T) {
	now := time.Date(2026, 11, 9, 23, 0, 0, 0, time.UTC)
	valid := `{
		"status": "watch",
		"headline": "Wait 24 s at p90 in 24 h.",
		"metrics": [
			{"label": "Jobs (24h)", "value": "289"},
			{"label": "Wait p90 (24h)", "value": "24 s"},
			{"label": "Failed (24h)", "value": "11", "tone": "act"}
		],
		"series": {"label": "Queue wait p90 per day, seconds", "values": [3, 0, null, 12.5]},
		"updated": "2026-11-09T22:51:02Z",
		"staleAfter": 15,
		"extra": {"ignored": true}
	}`
	tile, err := readTestTile(t, writeTileDir(t, valid), now)
	if err != nil {
		t.Fatal(err)
	}
	if tile.Status != tileWatch || tile.Headline != "Wait 24 s at p90 in 24 h." || len(tile.Metrics) != 3 || tile.Metrics[2].Tone != "act" {
		t.Errorf("valid tile: %+v", tile)
	}
	if tile.Fresh != "9 min ago" || tile.Stale || tile.Spark == nil || tile.Spark.Label != "Queue wait p90 per day, seconds" {
		t.Errorf("valid tile freshness or series: %+v %+v", tile, tile.Spark)
	}

	// Missing: no data yet, worth a look.
	tile, err = readTestTile(t, t.TempDir(), now)
	if err != nil || tile.Status != tileWatch || tile.Headline != "No data yet." {
		t.Errorf("missing tile.json: %+v %v", tile, err)
	}

	for name, body := range map[string]string{
		"not json":            `{`,
		"no status":           `{"headline": "x", "staleAfter": 15}`,
		"unknown status":      `{"status": "fine", "headline": "x", "staleAfter": 15}`,
		"status not string":   `{"status": 1, "headline": "x", "staleAfter": 15}`,
		"blank headline":      `{"status": "ok", "headline": " ", "staleAfter": 15}`,
		"long headline":       `{"status": "ok", "headline": "` + strings.Repeat("é", 201) + `", "staleAfter": 15}`,
		"four metrics":        `{"status": "ok", "headline": "x", "staleAfter": 15, "metrics": [{"label": "a", "value": "1"}, {"label": "b", "value": "2"}, {"label": "c", "value": "3"}, {"label": "d", "value": "4"}]}`,
		"long metric value":   `{"status": "ok", "headline": "x", "staleAfter": 15, "metrics": [{"label": "a", "value": "1234567890123"}]}`,
		"long metric label":   `{"status": "ok", "headline": "x", "staleAfter": 15, "metrics": [{"label": "` + strings.Repeat("a", 41) + `", "value": "1"}]}`,
		"unknown metric tone": `{"status": "ok", "headline": "x", "staleAfter": 15, "metrics": [{"label": "a", "value": "1", "tone": "red"}]}`,
		"one point":           `{"status": "ok", "headline": "x", "staleAfter": 15, "series": {"label": "s", "values": [1]}}`,
		"61 points":           `{"status": "ok", "headline": "x", "staleAfter": 15, "series": {"label": "s", "values": [` + strings.Repeat("1,", 60) + `1]}}`,
		"text point":          `{"status": "ok", "headline": "x", "staleAfter": 15, "series": {"label": "s", "values": [1, "2"]}}`,
		"bad updated":         `{"status": "ok", "headline": "x", "staleAfter": 15, "updated": "yesterday"}`,
		"no staleAfter":       `{"status": "ok", "headline": "x"}`,
		"zero staleAfter":     `{"status": "ok", "headline": "x", "staleAfter": 0}`,
		"oversized":           `{"status": "ok", "headline": "x", "staleAfter": 15, "pad": "` + strings.Repeat("x", tileLimit) + `"}`,
	} {
		tile, err := readTestTile(t, writeTileDir(t, body), now)
		if err == nil || tile.Status != tileError || tile.Headline != "The tile's data could not be read." || tile.StateText() != "Unavailable." {
			t.Errorf("%s: %+v %v", name, tile, err)
		}
	}

	// A symlink is refused, like any linked asset.
	dir := writeTileDir(t, valid)
	linked := t.TempDir()
	if err := os.Symlink(filepath.Join(dir, "tile.json"), filepath.Join(linked, "tile.json")); err != nil {
		t.Fatal(err)
	}
	if tile, err := readTestTile(t, linked, now); err == nil || tile.Status != tileError {
		t.Errorf("symlinked tile.json: %+v %v", tile, err)
	}

	// Stale data is at least worth a look and says so; act stays act.
	stale := `{"status": "ok", "headline": "x", "updated": "2026-11-09T21:00:00Z", "staleAfter": 15}`
	tile, err = readTestTile(t, writeTileDir(t, stale), now)
	if err != nil || tile.Status != tileWatch || !tile.Stale || tile.Fresh != "stale, 2 h ago" || !strings.HasPrefix(tile.UpdatedText(), "Data from ") {
		t.Errorf("stale tile: %+v %v", tile, err)
	}
	tile, err = readTestTile(t, writeTileDir(t, strings.Replace(stale, `"ok"`, `"act"`, 1)), now)
	if err != nil || tile.Status != tileAct || !tile.Stale {
		t.Errorf("stale act tile: %+v %v", tile, err)
	}
	// Without a time the data is never stale.
	tile, err = readTestTile(t, writeTileDir(t, `{"status": "ok", "headline": "x", "updated": null, "staleAfter": 15}`), now)
	if err != nil || tile.Status != tileOK || tile.Fresh != "" || tile.UpdatedText() != "" {
		t.Errorf("tile without a time: %+v %v", tile, err)
	}
}

func writeTileDir(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tile.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestTileAgo(t *testing.T) {
	for _, tc := range []struct {
		age  time.Duration
		want string
	}{
		{-time.Minute, "just now"},
		{29 * time.Second, "just now"},
		{30 * time.Second, "1 min ago"},
		{59 * time.Minute, "59 min ago"},
		{89 * time.Minute, "1 h ago"},
		{90 * time.Minute, "2 h ago"},
		{47 * time.Hour, "47 h ago"},
		{48 * time.Hour, "2 d ago"},
	} {
		if got := ago(tc.age); got != tc.want {
			t.Errorf("ago(%s) = %q, want %q", tc.age, got, tc.want)
		}
	}
}

func TestSparkline(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	for _, tc := range []struct {
		values []*float64
		want   string
	}{
		// Missing points are skipped; the floor is 0.
		{[]*float64{f(0), nil, f(10), f(5)}, "0.00,28.00 50.00,2.00 100.00,15.00"},
		// Below 0 the floor is the smallest value.
		{[]*float64{f(-5), f(5)}, "0.00,28.00 100.00,2.00"},
		// A flat line at 0 spans 1.
		{[]*float64{f(0), f(0)}, "0.00,28.00 100.00,28.00"},
	} {
		s := sparkline("label", tc.values)
		if s == nil || s.Line != tc.want || s.Area != "0,30 "+tc.want+" 100,30" {
			t.Errorf("sparkline(%v) = %+v, want %s", tc.values, s, tc.want)
		}
	}
	if s := sparkline("label", []*float64{f(1), nil}); s != nil {
		t.Errorf("one number makes no line: %+v", s)
	}
	// JavaScript's toFixed rounds an exact tie up; other values round to
	// the nearest.
	for v, want := range map[float64]string{3.125: "3.13", 0.015: "0.01", 1.005: "1.00", 2.5: "2.50", 0.375: "0.38", 0.625: "0.63"} {
		if got := fixed2(v); got != want {
			t.Errorf("fixed2(%v) = %s, want %s", v, got, want)
		}
	}
}

// liveTestPages writes a collection of tile pages with the given statuses,
// newest first, so the index order is the order given.
func liveTestPages(t *testing.T, root, collection string, statuses ...string) {
	t.Helper()
	for i, status := range statuses {
		name := collection + "/" + status + "-" + string(rune('a'+i))
		writeTilePage(t, root, name, "Page "+string(rune('A'+i)), time.Date(2026, 10, 28-i, 0, 0, 0, 0, time.UTC).Format(time.DateOnly))
		if status == "error" {
			writeContentFile(t, root, name+"/tile.json", `{`)
		} else {
			writeTileStatus(t, root, name, status)
		}
	}
}

func TestTileRenderingAndOrder(t *testing.T) {
	d, _ := newBacklogDeck(t)
	refreshAndProbe(d, d.cfg.Threads.Instances[0])
	root := d.cfg.ContentDir
	liveTestPages(t, root, "live", "ok", "error", "watch", "act", "ok")
	// A full tile: metrics, tones, a series and a stale time.
	writeContentFile(t, root, "live/act-d/tile.json", `{
		"status": "act",
		"headline": "Runners <down>",
		"metrics": [{"label": "Wait p90", "value": "24 s"}, {"label": "Failed", "value": "11", "tone": "watch"}],
		"series": {"label": "Wait per day", "values": [0, null, 10, 5]},
		"updated": "`+testClock.Add(-2*time.Hour).Format(time.RFC3339)+`",
		"staleAfter": 15
	}`)
	h := contentTestHandler(t, d, nil)

	for _, path := range []string{"/", "/live/"} {
		body := get(t, h, path).Body.String()
		start := strings.Index(body, `<ul class="deck-tiles" data-deck-live="tiles">`)
		if start < 0 {
			t.Fatalf("%s has no tiles: %s", path, body)
		}
		tiles := body[start:]
		tiles = tiles[:strings.Index(tiles, "</ul>")]
		// act, watch, error, ok, then index order.
		var at []int
		for _, page := range []string{"act-d", "watch-c", "error-b", "ok-a", "ok-e"} {
			at = append(at, strings.Index(tiles, `href="/live/`+page+`/"`))
		}
		for i := 1; i < len(at); i++ {
			if at[i-1] < 0 || at[i] < at[i-1] {
				t.Errorf("%s tile order %v: %s", path, at, tiles)
				break
			}
		}
		for _, want := range []string{
			`<a class="deck-tile" href="/live/act-d/" data-status="act">`,
			`<span class="deck-tile-state deck-sr">Needs action.</span>`,
			`<span class="deck-tile-fresh stale" title="Data from `,
			`>stale, 2 h ago</span>`,
			`<span class="deck-tile-headline">Runners &lt;down&gt;</span>`,
			`<span class="deck-metric"><b title="24 s">24 s</b><span>Wait p90</span></span>`,
			`<span class="deck-metric watch"><b title="11">11</b><span>Failed</span></span>`,
			`<polygon class="deck-spark-area" points="0,30 0.00,28.00 50.00,2.00 100.00,15.00 100,30"></polygon>`,
			`<polyline class="deck-spark-line" points="0.00,28.00 50.00,2.00 100.00,15.00"></polyline>`,
			`<small>Wait per day</small>`,
			`data-status="error"`, "The tile&#39;s data could not be read.", "Unavailable.",
			`data-status="watch"`, "Worth a look.", `data-status="ok"`, "OK.",
		} {
			if !strings.Contains(tiles, want) {
				t.Errorf("%s tiles lack %s", path, want)
			}
		}
		// A tile without metrics or a series leaves them empty, which the
		// stylesheet hides.
		if !strings.Contains(tiles, `<span class="deck-tile-metrics"></span>`) || !strings.Contains(tiles, `<span class="deck-tile-spark" aria-hidden="true"></span>`) {
			t.Errorf("%s: empty metrics or series are not empty: %s", path, tiles)
		}
	}
}

func TestLiveCounts(t *testing.T) {
	d, _ := newBacklogDeck(t)
	refreshAndProbe(d, d.cfg.Threads.Instances[0])
	root := d.cfg.ContentDir
	liveTestPages(t, root, "live", "ok", "error", "watch", "act")
	liveTestPages(t, root, "reports", "watch", "ok")
	liveTestPages(t, root, "calm", "ok")
	writeContentPage(t, root, "notes/a", "A", "2026-10-01", "")
	h := contentTestHandler(t, d, nil)

	var nav NavView
	if err := json.Unmarshal(get(t, h, "/nav.json").Body.Bytes(), &nav); err != nil {
		t.Fatal(err)
	}
	counts := map[string]string{}
	for _, item := range nav.Items {
		if item.Collection {
			counts[item.URL] = item.Tone + " " + string(rune('0'+item.Count))
		}
	}
	want := map[string]string{"/live/": "danger 3", "/reports/": "attn 1", "/calm/": " 0", "/notes/": " 0"}
	for url, w := range want {
		if counts[url] != w {
			t.Errorf("nav.json %s: %q, want %q", url, counts[url], w)
		}
	}

	body := get(t, h, "/").Body.String()
	for _, want := range []string{
		`<a href="/live/">Live<span class="deck-shell-count danger" aria-label="3 waiting">3</span></a>`,
		`<a href="/reports/">Reports<span class="deck-shell-count attn" aria-label="1 waiting">1</span></a>`,
		`<a href="/calm/">Calm</a>`,
		`<a class="ov-need danger" href="/live/"><b>3</b><span>live pages need a look</span></a>`,
		`<a class="ov-need attn" href="/reports/"><b>1</b><span>reports page needs a look</span></a>`,
		`<a class="ov-need quiet" href="/calm/"><b>0</b><span>calm pages need a look</span></a>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("overview lacks %s", want)
		}
	}
	// The live entries follow the views' own.
	if strings.Index(body, `href="/live/"><b>`) < strings.Index(body, "issues maybe done") {
		t.Error("the live entries should follow the views' counts")
	}
	if strings.Contains(body, `href="/notes/"><b>`) {
		t.Error("a collection without tiles has no needs entry")
	}
}

func TestLiveCountsPagesOnly(t *testing.T) {
	d := newPagesDeck(t)
	liveTestPages(t, d.cfg.ContentDir, "live", "watch", "ok")
	h := contentTestHandler(t, d, nil)
	body := get(t, h, "/").Body.String()
	needs := body[strings.Index(body, `<ul class="ov-needs"`):]
	needs = needs[:strings.Index(needs, "</ul>")]
	if strings.Count(needs, "<li>") != 1 || !strings.Contains(needs, `<a class="ov-need attn" href="/live/"><b>1</b><span>live page needs a look</span></a>`) {
		t.Errorf("the pages-only overview counts only the live pages: %s", needs)
	}
	var nav NavView
	if err := json.Unmarshal(get(t, h, "/nav.json").Body.Bytes(), &nav); err != nil {
		t.Fatal(err)
	}
	last := nav.Items[len(nav.Items)-1]
	if last.URL != "/live/" || last.Count != 1 || last.Tone != "attn" {
		t.Errorf("the pages-only nav counts the live pages: %+v", nav.Items)
	}
}
