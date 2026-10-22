package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func writeContentFile(t *testing.T, root, name, body string) {
	t.Helper()
	file := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeContentPage(t *testing.T, root, name, title, date, summary string) {
	t.Helper()
	data, err := json.Marshal(ContentPage{Title: title, Date: date, Summary: summary})
	if err != nil {
		t.Fatal(err)
	}
	writeContentFile(t, root, name+"/metadata.json", string(data))
	writeContentFile(t, root, name+"/index.html", `<!doctype html><title>Example</title><link rel="stylesheet" href="style.css"><script src="app.js" defer></script><h1>Example page</h1>`)
	writeContentFile(t, root, name+"/style.css", "h1 { color: navy; }")
	writeContentFile(t, root, name+"/app.js", `document.title = "Example";`)
}

func contentTestHandler(t *testing.T, d *Deck, auth *Authenticator) http.Handler {
	t.Helper()
	h, err := newHandler(d, auth, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestContentIndexes(t *testing.T) {
	d, _ := newTestDeck(t)
	d.cfg.ContentDir = t.TempDir()
	writeContentPage(t, d.cfg.ContentDir, "notes/old", "Older", "2026-01-01", "First")
	writeContentPage(t, d.cfg.ContentDir, "notes/latest", `<script>alert("title")</script>`, "2026-10-01", `<img src=x onerror="alert(1)">`)
	writeContentPage(t, d.cfg.ContentDir, "notes/alpha", "Same date", "2026-10-01", "Tie")
	writeContentPage(t, d.cfg.ContentDir, "examples/start", "Start", "2026-02-01", "Example")
	writeContentPage(t, d.cfg.ContentDir, "Invalid/start", "Invalid collection", "2026-02-01", "")
	writeContentPage(t, d.cfg.ContentDir, "threads/hidden", "Reserved collection", "2026-02-01", "")
	h := contentTestHandler(t, d, nil)
	library := get(t, h, "/")
	if library.Code != http.StatusOK {
		t.Fatalf("library: %d %s", library.Code, library.Body.String())
	}
	body := library.Body.String()
	for _, want := range []string{`href="/threads/"`, `href="/examples/"`, `href="/notes/"`} {
		if !strings.Contains(body, want) {
			t.Errorf("library missing %s: %s", want, body)
		}
	}
	if strings.Index(body, `href="/examples/"`) > strings.Index(body, `href="/notes/"`) || strings.Contains(body, "Invalid") || strings.Contains(body, "hidden") {
		t.Errorf("invalid library order or entries: %s", body)
	}
	collection := get(t, h, "/notes/")
	body = collection.Body.String()
	if collection.Code != http.StatusOK {
		t.Fatalf("collection: %d %s", collection.Code, body)
	}
	if strings.Index(body, `href="/notes/alpha/"`) > strings.Index(body, `href="/notes/latest/"`) || strings.Index(body, `href="/notes/latest/"`) > strings.Index(body, `href="/notes/old/"`) {
		t.Errorf("pages not sorted by date and slug: %s", body)
	}
	for _, escaped := range []string{"&lt;script&gt;", "&lt;img src=x", `href="/notes/latest/"`, `datetime="2026-10-01"`} {
		if !strings.Contains(body, escaped) {
			t.Errorf("collection missing %q: %s", escaped, body)
		}
	}
	if strings.Contains(body, `<script>`) || strings.Contains(body, `<img src=x`) {
		t.Fatalf("metadata was not escaped: %s", body)
	}
}

func TestContentPageAssetsAndRedirects(t *testing.T) {
	d, _ := newTestDeck(t)
	d.cfg.ContentDir = t.TempDir()
	writeContentPage(t, d.cfg.ContentDir, "notes/example", "Example", "2026-10-01", "")
	writeContentFile(t, d.cfg.ContentDir, "notes/example/figure.png", "\x89PNG\r\n\x1a\n")
	h := contentTestHandler(t, d, nil)
	for _, tc := range []struct{ path, kind, body string }{
		{"/notes/example/", "text/html", "Example page"},
		{"/notes/example/index.html", "text/html", "Example page"},
		{"/notes/example/style.css", "text/css", "color: navy"},
		{"/notes/example/app.js", "javascript", "document.title"},
		{"/notes/example/figure.png", "image/png", "PNG"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			rec := get(t, h, tc.path)
			if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), tc.kind) || !strings.Contains(rec.Body.String(), tc.body) {
				t.Fatalf("%s: %d %s %s", tc.path, rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
			}
			if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe-inline") {
				t.Errorf("unexpected CSP %q", csp)
			}
			if rec.Header().Get("X-Content-Type-Options") != "nosniff" || rec.Header().Get("Cache-Control") != "no-store" {
				t.Error("missing security headers")
			}
		})
	}
	for _, p := range []string{"/notes", "/notes/example"} {
		rec := get(t, h, p+"?view=full")
		if rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != p+"/?view=full" {
			t.Errorf("redirect %s: %d %q", p, rec.Code, rec.Header().Get("Location"))
		}
	}
	for _, p := range []string{"/missing", "/missing/", "/notes/missing", "/notes/example/missing.css", "/notes/example/style.css/", "/notes/example/assets/"} {
		if rec := get(t, h, p); rec.Code != http.StatusNotFound {
			t.Errorf("unknown %s: %d", p, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/notes/example/", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("HEAD page: %d", rec.Code)
	}
}

func TestContentMalformedPages(t *testing.T) {
	d, _ := newTestDeck(t)
	d.cfg.ContentDir = t.TempDir()
	for _, tc := range []struct{ slug, metadata string }{
		{"invalid-json", "{"},
		{"missing-title", `{"date":"2026-10-01"}`},
		{"blank-title", `{"title":"  ","date":"2026-10-01"}`},
		{"invalid-date", `{"title":"Invalid","date":"2026-02-30"}`},
		{"short-date", `{"title":"Invalid","date":"2026-1-01"}`},
		{"missing-date", `{"title":"Invalid"}`},
		{"oversized", `{"title":"` + strings.Repeat("x", metadataLimit) + `","date":"2026-10-01"}`},
		{"trailing-json", `{"title":"Invalid","date":"2026-10-01"} {}`},
	} {
		writeContentFile(t, d.cfg.ContentDir, "notes/"+tc.slug+"/metadata.json", tc.metadata)
		writeContentFile(t, d.cfg.ContentDir, "notes/"+tc.slug+"/index.html", "Invalid")
		writeContentFile(t, d.cfg.ContentDir, "notes/"+tc.slug+"/style.css", "Invalid")
	}
	writeContentFile(t, d.cfg.ContentDir, "notes/missing-metadata/index.html", "Invalid")
	writeContentFile(t, d.cfg.ContentDir, "notes/missing-index/metadata.json", `{"title":"Invalid","date":"2026-10-01"}`)
	writeContentPage(t, d.cfg.ContentDir, "notes/Invalid", "Invalid identifier", "2026-10-01", "")
	var logs bytes.Buffer
	h, err := newHandler(d, nil, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	rec := get(t, h, "/notes/")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "No pages yet.") {
		t.Fatalf("invalid pages listed: %d %s", rec.Code, rec.Body.String())
	}
	entries, err := os.ReadDir(filepath.Join(d.cfg.ContentDir, "notes"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		for _, suffix := range []string{"", "/", "/style.css"} {
			p := "/notes/" + entry.Name() + suffix
			if rec := get(t, h, p); rec.Code != http.StatusNotFound {
				t.Errorf("malformed %s: %d", p, rec.Code)
			}
		}
	}
	if !strings.Contains(logs.String(), "content page skipped") || !strings.Contains(logs.String(), "invalid-json") || !strings.Contains(logs.String(), "metadata exceeds") {
		t.Errorf("missing useful diagnostics: %s", logs.String())
	}
}

func TestContentRefusesPrivatePathsAndSpecialFiles(t *testing.T) {
	d, _ := newTestDeck(t)
	d.cfg.ContentDir = t.TempDir()
	writeContentPage(t, d.cfg.ContentDir, "notes/example", "Example", "2026-10-01", "")
	for _, name := range []string{".hidden", ".hidden.css", "private.json", "archive.zip", "UPPER.css", "nested/file.css"} {
		writeContentFile(t, d.cfg.ContentDir, "notes/example/"+name, "not public")
	}
	outside := t.TempDir()
	writeContentPage(t, outside, "outside/example", "Outside", "2026-10-01", "")
	writeContentFile(t, outside, "outside.css", "not public")
	for _, link := range []struct{ target, name string }{
		{filepath.Join(outside, "outside.css"), "notes/example/escape.css"},
		{"metadata.json", "notes/example/metadata.css"},
		{".hidden.css", "notes/example/hidden.css"},
		{filepath.Join(outside, "outside"), "escape"},
		{filepath.Join(outside, "outside/example"), "notes/escape"},
	} {
		if err := os.Symlink(link.target, filepath.Join(d.cfg.ContentDir, link.name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := syscall.Mkfifo(filepath.Join(d.cfg.ContentDir, "notes/example/pipe.css"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(d.cfg.ContentDir, "notes/example/directory.css"), 0o700); err != nil {
		t.Fatal(err)
	}
	h := contentTestHandler(t, d, nil)
	for _, p := range []string{
		"/notes/example/metadata.json", "/notes/example/.hidden", "/notes/example/.hidden.css", "/notes/example/private.json", "/notes/example/archive.zip", "/notes/example/UPPER.css", "/notes/example/nested/file.css", "/notes/example/escape.css", "/notes/example/metadata.css", "/notes/example/hidden.css", "/escape/", "/escape/example/", "/notes/escape/", "/notes/example/pipe.css", "/notes/example/directory.css", "/notes/example/directory.css/", "/notes/example/../example/style.css", "/notes/example/%2e%2e/example/style.css", "/notes/example/%2fstyle.css", "/notes/example/%5cstyle.css", "/notes/example/%00.css", "/notes//example/style.css", "/notes/example/style.css/extra", "/notes/example/%252e%252e", "/notes/example/../../outside.css", "/notes/../threads/", "/notes/../threads/static/deck.css",
	} {
		if rec := get(t, h, p); rec.Code != http.StatusNotFound {
			t.Errorf("refused path %s: %d (%s)", p, rec.Code, rec.Header().Get("Location"))
		}
	}
	if rec := get(t, h, "/"); strings.Contains(rec.Body.String(), `href="/escape/"`) {
		t.Error("symlink collection listed")
	}
}

func TestContentAuthAndOrigin(t *testing.T) {
	d, _ := newTestDeck(t)
	d.cfg.ContentDir = t.TempDir()
	writeContentPage(t, d.cfg.ContentDir, "notes/example", "Example", "2026-10-01", "")
	ti := newTestIssuer(t)
	auth := newAuthenticator(AuthConfig{Issuer: ti.srv.URL, ClientID: "deck-client", Group: "deck-users"}, ti.srv.Client(), quietLog())
	h := contentTestHandler(t, d, auth)
	token := ti.sign(t, ti.key, ti.claims(nil))
	for _, p := range []string{"/", "/notes", "/notes/", "/notes/example", "/notes/example/", "/notes/example/index.html", "/notes/example/style.css", "/notes/example/app.js", "/notes/example/metadata.json", "/notes/example/../example/style.css", "/notes//example/"} {
		if rec := get(t, h, p); rec.Code != http.StatusUnauthorized {
			t.Errorf("unauthenticated %s: %d", p, rec.Code)
		}
	}
	for _, p := range []string{"/", "/notes/", "/notes/example/", "/notes/example/style.css", "/notes/example/app.js"} {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("authenticated %s: %d", p, rec.Code)
		}
		req = httptest.NewRequest(http.MethodPost, p, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Origin", "https://other.example.com")
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("cross-origin POST %s: %d", p, rec.Code)
		}
		req.Header.Set("Origin", d.cfg.PublicOrigin)
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("same-origin POST %s: %d", p, rec.Code)
		}
	}
}

func TestContentWithDashboardBases(t *testing.T) {
	for _, base := range []string{"/threads/", "/ops/deck/", "/"} {
		t.Run(base, func(t *testing.T) {
			d, _ := newTestDeck(t)
			d.cfg.BasePath = base
			d.cfg.ContentDir = t.TempDir()
			writeContentPage(t, d.cfg.ContentDir, "notes/example", "Example", "2026-10-01", "")
			h := contentTestHandler(t, d, nil)
			for _, p := range []string{base, base + "fragment", base + "static/deck.css", base + "static/deck.js", "/notes/", "/notes/example/", "/healthz"} {
				if rec := get(t, h, p); rec.Code != http.StatusOK {
					t.Errorf("existing or content route %s: %d", p, rec.Code)
				}
			}
			body := get(t, h, "/").Body.String()
			if base == "/" && (!strings.Contains(body, "<title>Threads</title>") || strings.Contains(body, "<h2>Collections</h2>")) {
				t.Error("root dashboard was replaced")
			}
			if base != "/" && !strings.Contains(body, `href="`+base+`"`) {
				t.Error("library lacks configured dashboard link")
			}
			for _, action := range []string{"refresh", "refresh-all", "ack", "forget"} {
				p := base + "actions/" + action
				if rec := get(t, h, p); rec.Code != http.StatusMethodNotAllowed {
					t.Errorf("action GET %s: %d", p, rec.Code)
				}
				if rec := post(h, p, nil, "https://other.example.com"); rec.Code != http.StatusForbidden {
					t.Errorf("action Origin %s: %d", p, rec.Code)
				}
			}
		})
	}
}

func TestContentMissingDirectoryAndConfig(t *testing.T) {
	d, _ := newTestDeck(t)
	d.cfg.ContentDir = filepath.Join(t.TempDir(), "not-created")
	h := contentTestHandler(t, d, nil)
	if rec := get(t, h, "/"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "No collections yet.") {
		t.Fatalf("empty library: %d %s", rec.Code, rec.Body.String())
	}
	if rec := get(t, h, "/notes/"); rec.Code != http.StatusNotFound {
		t.Errorf("missing collection: %d", rec.Code)
	}
	if _, err := os.Stat(d.cfg.ContentDir); !os.IsNotExist(err) {
		t.Errorf("HTTP created content directory: %v", err)
	}
	dataHome, home := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	for _, tc := range []struct{ xdg, explicit, want string }{
		{dataHome, "", filepath.Join(dataHome, "deck/pages")},
		{"", "", filepath.Join(home, ".local/share/deck/pages")},
		{"relative", "", filepath.Join(home, ".local/share/deck/pages")},
		{dataHome, filepath.Join(home, "custom"), filepath.Join(home, "custom")},
	} {
		t.Setenv("XDG_DATA_HOME", tc.xdg)
		cfg := d.cfg
		cfg.ContentDir = tc.explicit
		if err := cfg.normalize(); err != nil || cfg.ContentDir != tc.want {
			t.Errorf("normalize XDG %q explicit %q: %q, %v", tc.xdg, tc.explicit, cfg.ContentDir, err)
		}
	}
	cfg := d.cfg
	cfg.ContentDir = "relative/pages"
	if err := cfg.normalize(); err == nil {
		t.Error("relative contentDir accepted")
	}
	configPath := filepath.Join(t.TempDir(), "deck.toml")
	config := "contentDir = " + strconv.Quote(filepath.Join(home, "bundles")) + `
publicOrigin = "https://deck.example.com"
[[threads.instance]]
name = "main"
db = "/var/lib/example/state.sqlite"
webURL = "https://t3.example.com"
`
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadConfig(configPath)
	if err != nil || loaded.ContentDir != filepath.Join(home, "bundles") {
		t.Fatalf("load contentDir: %q, %v", loaded.ContentDir, err)
	}
}

func TestContentBundleRevalidation(t *testing.T) {
	d, _ := newTestDeck(t)
	d.cfg.ContentDir = t.TempDir()
	h := contentTestHandler(t, d, nil)
	writeContentPage(t, d.cfg.ContentDir, "notes/example", "Example", "2026-10-01", "")
	if rec := get(t, h, "/notes/example/"); rec.Code != http.StatusOK {
		t.Fatalf("new bundle without restart: %d", rec.Code)
	}
	for _, name := range []string{"metadata.json", "index.html"} {
		t.Run(name, func(t *testing.T) {
			file := filepath.Join(d.cfg.ContentDir, "notes/example", name)
			original, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(file); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("app.js", file); err != nil {
				t.Fatal(err)
			}
			for _, p := range []string{"/notes/example/", "/notes/example/app.js"} {
				if rec := get(t, h, p); rec.Code != http.StatusNotFound {
					t.Errorf("bundle with symlink %s served at %s: %d", name, p, rec.Code)
				}
			}
			if rec := get(t, h, "/notes/"); strings.Contains(rec.Body.String(), `href="/notes/example/"`) {
				t.Errorf("bundle with symlink %s listed", name)
			}
			if err := os.Remove(file); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file, original, 0o600); err != nil {
				t.Fatal(err)
			}
		})
	}
}
