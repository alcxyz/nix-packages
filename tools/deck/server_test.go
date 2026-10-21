package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func newTestServer(t *testing.T) (*Deck, http.Handler, string) {
	t.Helper()
	d, argsLog := newTestDeck(t)
	refreshAndProbe(d, d.cfg.Threads.Instances[0])
	h, err := newHandler(d, nil, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	return d, h, argsLog
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	return rec
}

func post(h http.Handler, path string, form url.Values, origin string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestPageEscapesThreadText(t *testing.T) {
	_, h, _ := newTestServer(t)
	rec := get(t, h, "/threads/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	for _, raw := range []string{"<script>alert(1)", "<img src=x", "<i>fails</i>", "<b>bold</b>"} {
		if strings.Contains(body, raw) {
			t.Errorf("unescaped thread text %q in the page", raw)
		}
	}
	for _, want := range []string{
		"Fix &lt;script&gt;alert(1)&lt;/script&gt; parser",
		"Waiting on you", "Decide or answer", "Maybe not done", "Confirmed done",
		"Accept as done", "Undo accept",
		"One merge unblocks several",
		`href="https://t3.example.com/env-example/thread-a1"`,
		"Settle: check, importer left a TODO",
		"Run the importer once",
		"facts as of",
		`action="actions/refresh-all"`,
		"Refresh now",
		// The fixture's avgPct follows the group command's rule: progress,
		// or 0 without one, over unsettled threads; the page must agree.
		"<b>27%</b> average progress",
		`src="/threads/static/deck.js"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Count(body, `action="actions/ack"`) != 1 {
		t.Error("exactly the one settled-open thread should offer Accept as done")
	}
}

func TestSecurityHeadersAndRoutes(t *testing.T) {
	_, h, _ := newTestServer(t)
	rec := get(t, h, "/threads/")
	csp := rec.Header().Get("Content-Security-Policy")
	for _, want := range []string{"script-src 'self'", "frame-ancestors 'none'", "default-src 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q lacks %q", csp, want)
		}
	}
	if strings.Contains(csp, "unsafe-inline") {
		t.Error("CSP allows inline code")
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" || rec.Header().Get("Referrer-Policy") != "same-origin" {
		t.Error("missing headers")
	}
	if rec := get(t, h, "/healthz"); rec.Code != http.StatusOK {
		t.Errorf("healthz %d", rec.Code)
	}
	if rec := get(t, h, "/threads/static/deck.css"); rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), "css") {
		t.Errorf("static css %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	if rec := get(t, h, "/threads/fragment"); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "<html") {
		t.Errorf("fragment %d", rec.Code)
	}
	if rec := get(t, h, "/threads"); rec.Code != http.StatusMovedPermanently {
		t.Errorf("redirect %d", rec.Code)
	}
	if rec := get(t, h, "/elsewhere"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown path %d", rec.Code)
	}
	for _, action := range []string{"refresh", "refresh-all", "ack", "forget"} {
		path := "/threads/actions/" + action
		for _, method := range []string{"GET", "HEAD", "PUT", "DELETE"} {
			req := httptest.NewRequest(method, path, nil)
			req.Header.Set("Origin", "https://deck.example.com")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s: got %d, want 405", method, path, rec.Code)
			}
		}
	}
}

func TestAuthAppliesToEveryRouteButHealthz(t *testing.T) {
	d, _ := newTestDeck(t)
	ti := newTestIssuer(t)
	auth := newAuthenticator(AuthConfig{Issuer: ti.srv.URL, ClientID: "deck-client", Group: "deck-users"}, ti.srv.Client(), quietLog())
	h, err := newHandler(d, auth, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/threads/", "/threads/fragment", "/threads/static/deck.js", "/elsewhere"} {
		if rec := get(t, h, p); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without a token: %d", p, rec.Code)
		}
	}
	for _, action := range []string{"refresh", "refresh-all", "ack", "forget"} {
		path := "/threads/actions/" + action
		if rec := post(h, path, nil, "https://deck.example.com"); rec.Code != http.StatusUnauthorized {
			t.Errorf("POST %s without a token: %d", path, rec.Code)
		}
	}
	if rec := get(t, h, "/healthz"); rec.Code != http.StatusOK {
		t.Errorf("healthz %d", rec.Code)
	}
	req := httptest.NewRequest("GET", "/threads/", nil)
	req.Header.Set("Authorization", "Bearer "+ti.sign(t, ti.key, ti.claims(nil)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("with a valid token: %d", rec.Code)
	}
}

func TestAckAction(t *testing.T) {
	_, h, argsLog := newTestServer(t)
	form := url.Values{"instance": {"main"}, "thread": {"thread-d4"}, "settledAt": {"2026-10-20T10:00:00Z"}}

	if rec := post(h, "/threads/actions/ack", form, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("without Origin: %d", rec.Code)
	}
	if rec := post(h, "/threads/actions/ack", form, "https://evil.example.com"); rec.Code != http.StatusForbidden {
		t.Fatalf("cross origin: %d", rec.Code)
	}
	rec := post(h, "/threads/actions/ack", form, "https://deck.example.com")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("ack: %d %s", rec.Code, rec.Body.String())
	}
	var acks []string
	for _, line := range readArgs(t, argsLog) {
		if strings.HasPrefix(line, "ack ") {
			acks = append(acks, line)
		}
	}
	if len(acks) != 1 || acks[0] != "ack thread-d4@2026-10-20T10:00:00Z" {
		t.Fatalf("ack calls %v", acks)
	}

	forget := post(h, "/threads/actions/forget", url.Values{"instance": {"main"}, "thread": {"thread-e5"}}, "https://deck.example.com")
	if forget.Code != http.StatusSeeOther {
		t.Fatalf("forget: %d", forget.Code)
	}

	for name, f := range map[string]url.Values{
		"unsettled thread": {"instance": {"main"}, "thread": {"thread-a1"}, "settledAt": {"2026-10-20T10:00:00Z"}},
		"unknown thread":   {"instance": {"main"}, "thread": {"thread-zz"}, "settledAt": {"2026-10-20T10:00:00Z"}},
		"unknown instance": {"instance": {"other"}, "thread": {"thread-d4"}, "settledAt": {"2026-10-20T10:00:00Z"}},
		"bad id":           {"instance": {"main"}, "thread": {"--list"}, "settledAt": {"2026-10-20T10:00:00Z"}},
		"no settledAt":     {"instance": {"main"}, "thread": {"thread-d4"}},
		"bad settledAt":    {"instance": {"main"}, "thread": {"thread-d4"}, "settledAt": {"2026;x"}},
	} {
		if rec := post(h, "/threads/actions/ack", f, "https://deck.example.com"); rec.Code < 400 {
			t.Errorf("%s: %d", name, rec.Code)
		}
	}
}

func TestAckFailureIsReported(t *testing.T) {
	d, h, _ := newTestServer(t)
	failing := filepath.Join(t.TempDir(), "ack")
	if err := os.WriteFile(failing, []byte("#!/bin/sh\necho 'ack file is broken' >&2\nexit 65\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	d.cmds.cfg.Ack = failing
	req := httptest.NewRequest("POST", "/threads/actions/ack", strings.NewReader("instance=main&thread=thread-d4&settledAt=2026-10-20T10:00:00Z"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://deck.example.com")
	req.Header.Set("X-Deck-Fetch", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Body)
	if rec.Code != http.StatusBadGateway || !strings.Contains(string(body), "ack file is broken") {
		t.Fatalf("got %d %s", rec.Code, body)
	}
}

func TestRefreshAction(t *testing.T) {
	d, h, _ := newTestServer(t)
	req := httptest.NewRequest("POST", "/threads/actions/refresh", strings.NewReader("instance=main&thread=thread-b2"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://deck.example.com")
	req.Header.Set("X-Deck-Fetch", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("refresh %d", rec.Code)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.queue) == 0 || d.queue[0] != "main/thread-b2" {
		t.Fatalf("queue %v", d.queue)
	}
	if got := d.manual["main/thread-b2"]; got != d.cfg.Threads.SummaryRole {
		t.Fatalf("queued role %q, want default %q", got, d.cfg.Threads.SummaryRole)
	}
}

func TestThreadLinkEscapesSegments(t *testing.T) {
	in := InstanceConfig{WebURL: "https://t3.example.com"}
	if got := threadLink(in, "env/1", "a:b%c"); got != "https://t3.example.com/env%2F1/a:b%25c" {
		t.Fatalf("link %s", got)
	}
	if got := threadLink(in, "", "x"); got != "https://t3.example.com/" {
		t.Fatalf("link without environment %s", got)
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	good := `publicOrigin = "https://deck.example.com/"
[auth]
issuer = "https://id.example.com"
clientID = "deck"
group = "deck-users"
[[threads.instance]]
name = "main"
db = "/var/lib/example/state.sqlite"
webURL = "https://t3.example.com"
`
	if err := os.WriteFile(path, []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:3775" || cfg.BasePath != "/threads/" || cfg.PublicOrigin != "https://deck.example.com" ||
		cfg.Threads.WindowDays != 3 || *cfg.Threads.QuietMinutes != 10 || cfg.Threads.RefreshConcurrency != 2 ||
		cfg.Threads.CallsPerHour != 60 || cfg.Threads.SummaryRole != "light" ||
		!reflect.DeepEqual(cfg.Threads.SummaryRoles, []string{"light", "standard", "deep"}) || cfg.Commands.Messages != "t3-thread-messages" || cfg.Threads.Instances[0].Label != "main" {
		t.Fatalf("defaults: %+v", cfg)
	}
	if err := cfg.validateAuth(false); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`basePath = "ops/deck-1.0"`+"\n"+good), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg, err := loadConfig(path); err != nil || cfg.BasePath != "/ops/deck-1.0/" {
		t.Fatalf("nested base path: %v %q", err, cfg.BasePath)
	}

	bad := map[string]string{
		"unknown key":     good + "\nbogus = 1\n",
		"no instance":     strings.Split(good, "[[threads.instance]]")[0],
		"bad origin":      strings.Replace(good, "https://deck.example.com/", "https://deck.example.com/threads", 1),
		"bad web url":     strings.Replace(good, "https://t3.example.com", "javascript:alert(1)", 1),
		"duplicate":       good + "[[threads.instance]]\nname = \"main\"\ndb = \"x\"\nwebURL = \"https://t3.example.com\"\n",
		"bad name":        strings.Replace(good, `name = "main"`, `name = "Main/1"`, 1),
		"negative budget": strings.Replace(good, "[[threads.instance]]", "[threads]\ncallsPerHour = -1\n[[threads.instance]]", 1),
		"no db":           strings.Replace(good, `db = "/var/lib/example/state.sqlite"`, ``, 1),
		"wildcard base":   `basePath = "/t{3}/"` + "\n" + good,
		"malformed base":  `basePath = "/a{b/"` + "\n" + good,
		"dot-dot base":    `basePath = "/a/../b/"` + "\n" + good,
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

func TestAckRefusesAnUnseenSettlement(t *testing.T) {
	_, h, argsLog := newTestServer(t)
	// The page showed an earlier settlement than the facts now hold.
	form := url.Values{"instance": {"main"}, "thread": {"thread-d4"}, "settledAt": {"2026-10-19T08:00:00Z"}}
	rec := post(h, "/threads/actions/ack", form, "https://deck.example.com")
	if rec.Code != http.StatusConflict {
		t.Fatalf("got %d", rec.Code)
	}
	for _, line := range readArgs(t, argsLog) {
		if strings.HasPrefix(line, "ack ") {
			t.Fatalf("the ack command ran: %s", line)
		}
	}
	page := get(t, h, "/threads/").Body.String()
	if !strings.Contains(page, `name="settledAt" value="2026-10-20T10:00:00Z"`) {
		t.Fatal("the Accept form does not carry the settlement it shows")
	}
}

func TestOriginNormalization(t *testing.T) {
	for in, want := range map[string]string{
		"https://deck.example.com":      "https://deck.example.com",
		"HTTPS://Deck.Example.COM/":     "https://deck.example.com",
		"https://deck.example.com:443":  "https://deck.example.com",
		"http://127.0.0.1:80":           "http://127.0.0.1",
		"http://127.0.0.1:3775":         "http://127.0.0.1:3775",
		"https://deck.example.com:8443": "https://deck.example.com:8443",
		"http://[::1]:3775":             "http://[::1]:3775",
	} {
		got, ok := normalizeOrigin(in)
		if !ok || got != want {
			t.Errorf("%s: got %q %v, want %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "null", "deck.example.com", "ftp://deck.example.com", "https://deck.example.com/x", "https://u@deck.example.com", "https://deck.example.com?q"} {
		if _, ok := normalizeOrigin(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}

	cfg := testConfig(t)
	cfg.PublicOrigin = "HTTPS://Deck.Example.com:443/"
	if err := cfg.normalize(); err != nil || cfg.PublicOrigin != "https://deck.example.com" {
		t.Fatalf("config origin %q, %v", cfg.PublicOrigin, err)
	}
	h := sameOrigin(cfg.PublicOrigin, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) }))
	req := httptest.NewRequest("POST", "/threads/actions/ack", nil)
	req.Header.Set("Origin", "https://DECK.example.com:443")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTeapot {
		t.Fatalf("an equivalent Origin was refused: %d", rec.Code)
	}
}

func TestQuietMinutesConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	cfg := `publicOrigin = "https://deck.example.com"
[threads]
quietMinutes = 0
[[threads.instance]]
name = "main"
db = "/var/lib/example/state.sqlite"
webURL = "https://t3.example.com"
`
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if *c.Threads.QuietMinutes != 0 {
		t.Fatalf("quietMinutes = 0 became %d", *c.Threads.QuietMinutes)
	}
}

func TestAckRechecksTheSettlement(t *testing.T) {
	d, h, argsLog := newTestServer(t)
	fixture, err := filepath.Abs(filepath.Join("testdata", "inventory.json"))
	if err != nil {
		t.Fatal(err)
	}
	// The thread settled again after the last facts refresh.
	d.cmds.cfg.Inventory = writeScript(t, t.TempDir(), "inventory",
		`exec sed "s/2026-10-20T10:00:00Z/2026-10-20T11:30:00Z/" "`+fixture+`"`+"\n")
	form := url.Values{"instance": {"main"}, "thread": {"thread-d4"}, "settledAt": {"2026-10-20T10:00:00Z"}}
	if rec := post(h, "/threads/actions/ack", form, "https://deck.example.com"); rec.Code != http.StatusConflict {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
	for _, line := range readArgs(t, argsLog) {
		if strings.HasPrefix(line, "ack ") {
			t.Fatalf("the ack command ran: %s", line)
		}
	}
}

func TestReferrerPolicyKeepsFormPostsWorking(t *testing.T) {
	_, h, _ := newTestServer(t)
	// no-referrer would make browsers send "Origin: null" on the page's
	// plain form posts.
	if got := get(t, h, "/threads/").Header().Get("Referrer-Policy"); got != "same-origin" {
		t.Fatalf("Referrer-Policy %q", got)
	}
	form := url.Values{"instance": {"main"}, "thread": {"thread-a1"}}
	if rec := post(h, "/threads/actions/refresh", form, "null"); rec.Code != http.StatusForbidden {
		t.Fatalf("Origin null: %d", rec.Code)
	}
}

func TestAckSharesOneDeadline(t *testing.T) {
	d, h, argsLog := newTestServer(t)
	old := actionTimeout
	actionTimeout = 1500 * time.Millisecond
	defer func() { actionTimeout = old }()
	// Each command fits the action's deadline alone, but not together.
	dir := t.TempDir()
	d.cmds.cfg.Inventory = writeScript(t, dir, "inventory", "sleep 1\nexec "+fakeBin(t, "t3-thread-inventory")+` "$@"`+"\n")
	d.cmds.cfg.Ack = writeScript(t, dir, "ack", "sleep 1\nexec "+fakeBin(t, "t3-thread-overview-ack")+` "$@"`+"\n")
	form := url.Values{"instance": {"main"}, "thread": {"thread-d4"}, "settledAt": {"2026-10-20T10:00:00Z"}}
	start := time.Now()
	rec := post(h, "/threads/actions/ack", form, "https://deck.example.com")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
	// Bounded by the deadline and the kill grace, with room for a loaded
	// build machine.
	if limit := actionTimeout + killGrace; time.Since(start) > limit {
		t.Fatalf("the action took %s, more than %s", time.Since(start), limit)
	}
	for _, line := range readArgs(t, argsLog) {
		if strings.HasPrefix(line, "ack ") {
			t.Fatalf("the ack command outlived the deadline: %s", line)
		}
	}
}

func TestActionsStopAtShutdown(t *testing.T) {
	d, h, argsLog := newTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	d.base = ctx
	cancel()
	form := url.Values{"instance": {"main"}, "thread": {"thread-d4"}, "settledAt": {"2026-10-20T10:00:00Z"}}
	if rec := post(h, "/threads/actions/ack", form, "https://deck.example.com"); rec.Code != http.StatusBadGateway {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
	for _, line := range readArgs(t, argsLog) {
		if strings.HasPrefix(line, "ack ") {
			t.Fatalf("the ack command ran after shutdown: %s", line)
		}
	}
}

func TestServeBindsBeforeStartingWork(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	dir := t.TempDir()
	argsLog := filepath.Join(dir, "args.log")
	t.Setenv("DECK_ARGS_LOG", argsLog)
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	path := filepath.Join(dir, "config.toml")
	config := `listen = "` + busy.Addr().String() + `"
publicOrigin = "https://deck.example.com"
[commands]
inventory = "` + fakeBin(t, "t3-thread-inventory") + `"
status = "` + fakeBin(t, "t3-thread-status") + `"
[[threads.instance]]
name = "main"
db = "/var/lib/example/state.sqlite"
webURL = "https://t3.example.com"
`
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := serve([]string{"-config", path, "-dev-no-auth"}); err == nil || !strings.Contains(err.Error(), "address already in use") {
		t.Fatalf("serve on a busy address: %v", err)
	}
	if args := readArgs(t, argsLog); len(args) != 0 {
		t.Fatalf("commands ran although deck could not listen: %v", args)
	}
}

func TestActionOrigins(t *testing.T) {
	d, h, argsLog := newTestServer(t)
	before := readArgs(t, argsLog)
	form := url.Values{"instance": {"main"}, "thread": {"thread-d4"}, "settledAt": {"2026-10-20T10:00:00Z"}, "role": {"light"}}
	for _, action := range []string{"refresh", "refresh-all", "ack", "forget"} {
		for _, origin := range []string{"", "null", "https://evil.example.com"} {
			if rec := post(h, "/threads/actions/"+action, form, origin); rec.Code != http.StatusForbidden {
				t.Errorf("%s with Origin %q: got %d, want 403", action, origin, rec.Code)
			}
		}
	}
	if got := readArgs(t, argsLog); !reflect.DeepEqual(got, before) {
		t.Fatalf("rejected actions ran commands: before %v, after %v", before, got)
	}
	if len(d.manual) != 0 || len(d.refreshCh["main"]) != 0 {
		t.Fatalf("rejected actions queued work: manual %v, refreshes %d", d.manual, len(d.refreshCh["main"]))
	}
}

func TestRefreshActionRoles(t *testing.T) {
	cases := []struct {
		name, role, wantRole string
		wantStatus           int
	}{
		{"configured default", "", "deep", http.StatusSeeOther},
		{"explicit role", "light", "light", http.StatusSeeOther},
		{"explicit default", "deep", "deep", http.StatusSeeOther},
		{"unknown role", "missing", "", http.StatusBadRequest},
		{"unusable role", "standard", "", http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, h, _ := newTestServer(t)
			d.cfg.Threads.SummaryRole = "deep"
			d.models["deep"] = []ModelConfig{{Provider: "anthropic", Model: "deep-model", Transport: "cli"}}
			// A configured role with no runnable models must not be queued.
			d.models["standard"] = nil
			before := append([]string(nil), d.queue...)
			form := url.Values{"instance": {"main"}, "thread": {"thread-b2"}}
			if tc.role != "" {
				form.Set("role", tc.role)
			}
			rec := post(h, "/threads/actions/refresh", form, "https://deck.example.com")
			if rec.Code != tc.wantStatus {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantRole == "" {
				if !reflect.DeepEqual(d.queue, before) || len(d.manual) != 0 {
					t.Fatalf("invalid role changed work: queue %v, manual %v", d.queue, d.manual)
				}
				return
			}
			if len(d.queue) == 0 || d.queue[0] != "main/thread-b2" || d.manual["main/thread-b2"] != tc.wantRole {
				t.Fatalf("queue %v, manual %v; want %q", d.queue, d.manual, tc.wantRole)
			}
			if rec.Header().Get("Location") != d.cfg.BasePath {
				t.Fatalf("redirect %q", rec.Header().Get("Location"))
			}
		})
	}
}

func TestRefreshAllActionWithoutSummaries(t *testing.T) {
	d, _, argsLog := newTestServer(t)
	d.roleErr = errors.New("default role unavailable")
	d.models = nil
	d.cfg.BasePath = "/ops/deck/"
	h, err := newHandler(d, nil, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	page := get(t, h, d.cfg.BasePath)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `action="actions/refresh-all"`) {
		t.Fatalf("refresh-all missing while summaries are disabled: %d %s", page.Code, page.Body.String())
	}
	before := readArgs(t, argsLog)
	for _, fetch := range []bool{false, true} {
		req := httptest.NewRequest("POST", d.cfg.BasePath+"actions/refresh-all", nil)
		req.Header.Set("Origin", d.cfg.PublicOrigin)
		want := http.StatusSeeOther
		if fetch {
			req.Header.Set("X-Deck-Fetch", "1")
			want = http.StatusNoContent
		}
		rec := httptest.NewRecorder()
		done := make(chan struct{})
		go func() {
			h.ServeHTTP(rec, req)
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("refresh-all blocked instead of queuing a facts refresh")
		}
		if rec.Code != want {
			t.Fatalf("refresh-all fetch=%v: %d %s", fetch, rec.Code, rec.Body.String())
		}
		if !fetch && rec.Header().Get("Location") != d.cfg.BasePath {
			t.Fatalf("redirect %q", rec.Header().Get("Location"))
		}
		// Facts are fresh and no worker is running. Both requests must
		// return, including the one whose refresh channel is already full.
		if len(d.refreshCh["main"]) != 1 {
			t.Fatalf("fresh instance was not queued: %d refreshes", len(d.refreshCh["main"]))
		}
	}
	if got := readArgs(t, argsLog); !reflect.DeepEqual(got, before) {
		t.Fatalf("refresh-all ran commands in the handler: before %v, after %v", before, got)
	}
}

func TestSummaryRoleConfig(t *testing.T) {
	cases := []struct {
		name, settings, wantDefault string
		wantRoles                   []string
	}{
		{"custom default", `summaryRole = "standard"`, "standard", []string{"light", "standard", "deep"}},
		{"custom list", `summaryRoles = ["deep", "light"]`, "light", []string{"deep", "light"}},
		{"append default", "summaryRole = \"review\"\nsummaryRoles = [\"deep\"]", "review", []string{"deep", "review"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			content := `publicOrigin = "https://deck.example.com"
[threads]
` + tc.settings + `
[[threads.instance]]
name = "main"
db = "/var/lib/example/state.sqlite"
webURL = "https://t3.example.com"
`
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := loadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Threads.SummaryRole != tc.wantDefault || !reflect.DeepEqual(cfg.Threads.SummaryRoles, tc.wantRoles) {
				t.Fatalf("default %q, roles %v; want %q, %v", cfg.Threads.SummaryRole, cfg.Threads.SummaryRoles, tc.wantDefault, tc.wantRoles)
			}
			if err := cfg.normalize(); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cfg.Threads.SummaryRoles, tc.wantRoles) {
				t.Fatalf("normalizing twice changed roles to %v", cfg.Threads.SummaryRoles)
			}
		})
	}
}

func TestSummaryRolePickerAndAttribution(t *testing.T) {
	for _, cachedRole := range []string{"", "deep"} {
		t.Run("cached-role-"+cachedRole, func(t *testing.T) {
			d, _ := newTestDeck(t)
			d.cfg.Threads.SummaryRole = "standard"
			d.cfg.Threads.SummaryRoles = []string{"deep", "light", "missing", "standard"}
			d.models["standard"] = []ModelConfig{{Provider: "anthropic", Model: "standard-model", Transport: "cli"}}
			d.models["deep"] = nil
			s, err := parseSummary([]byte(validSummary), false)
			if err != nil {
				t.Fatal(err)
			}
			entry := CacheEntry{Summary: &s, Model: "anthropic/cached-model", Role: cachedRole, MadeAt: testClock}
			row := d.rowLocked(d.cfg.Threads.Instances[0], "env-example", GroupRow{
				InventoryThread: InventoryThread{ThreadID: "thread-a1", Bucket: "unsettled"},
			}, StatusThread{}, map[string]CacheEntry{"main/thread-a1": entry}, nil)
			wantRole := cachedRole
			if wantRole == "" {
				wantRole = "standard"
			}
			if row.SummaryRole != wantRole || row.DefaultSummaryRole != "standard" || !reflect.DeepEqual(row.SummaryRoles, []string{"light", "standard"}) {
				t.Fatalf("summary role %q, default %q, picker %v", row.SummaryRole, row.DefaultSummaryRole, row.SummaryRoles)
			}
			tmpl, err := parseTemplates()
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			if err := tmpl.ExecuteTemplate(&out, "row", row); err != nil {
				t.Fatal(err)
			}
			body := out.String()
			for _, want := range []string{`name="role"`, `value="light"`, `value="standard" selected`, "summary by anthropic/cached-model (" + wantRole + ")"} {
				if !strings.Contains(body, want) {
					t.Errorf("row lacks %q: %s", want, body)
				}
			}
			for _, unavailable := range []string{`value="deep"`, `value="missing"`} {
				if strings.Contains(body, unavailable) {
					t.Errorf("row offers unavailable role %s", unavailable)
				}
			}
		})
	}
}
