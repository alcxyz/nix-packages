package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testPEM           = "-----BEGIN RSA PRIVATE KEY-----\nMIIEfakeKeyMaterialLineOne\nfakeKeyMaterialLineTwo==\n-----END RSA PRIVATE KEY-----\n"
	testCode          = "code-secret-value-123"
	testClientSecret  = "client-secret-value-456"
	testWebhookSecret = "webhook-secret-value-789"
	testClientID      = "Iv1.clientidvalue"
)

var secretStrings = []string{"fakeKeyMaterial", "PRIVATE KEY", testCode, testClientSecret, testWebhookSecret}

func assertNoSecrets(t *testing.T, label, text string) {
	t.Helper()
	for _, secret := range secretStrings {
		if strings.Contains(text, secret) {
			t.Errorf("%s contains secret %q:\n%s", label, secret, text)
		}
	}
}

// fakeConversion is a fake of GitHub's manifest conversion endpoint.
type fakeConversion struct {
	mu     sync.Mutex
	calls  int
	status int
	body   string
	t      *testing.T
}

func newFakeConversion(t *testing.T) (*fakeConversion, *httptest.Server) {
	f := &fakeConversion{t: t, status: http.StatusCreated}
	app := map[string]any{
		"id":             4242,
		"slug":           "my-test-app",
		"name":           "My Test App",
		"html_url":       "https://github.com/apps/my-test-app",
		"client_id":      testClientID,
		"client_secret":  testClientSecret,
		"webhook_secret": testWebhookSecret,
		"pem":            testPEM,
	}
	body, err := json.Marshal(app)
	if err != nil {
		t.Fatal(err)
	}
	f.body = string(body)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls++
		if r.Method != http.MethodPost {
			t.Errorf("conversion method = %s", r.Method)
		}
		if r.URL.Path != "/app-manifests/"+testCode+"/conversions" {
			t.Errorf("conversion path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Accept"); got != "application/vnd.github+json" {
			t.Errorf("Accept = %q", got)
		}
		if got := r.Header.Get("X-GitHub-Api-Version"); got != "2022-11-28" {
			t.Errorf("X-GitHub-Api-Version = %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization = %q, want none", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.status)
		io.WriteString(w, f.body)
	}))
	t.Cleanup(srv.Close)
	setConversionBaseURL(t, srv.URL)
	return f, srv
}

func (f *fakeConversion) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func setConversionBaseURL(t *testing.T, value string) {
	old := conversionBaseURL
	conversionBaseURL = value
	t.Cleanup(func() { conversionBaseURL = old })
}

func writeManifest(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const validManifest = `{
  "name": "My Test App",
  "url": "https://example.com",
  "description": "test",
  "hook_attributes": {"url": "https://example.com", "active": false},
  "redirect_url": "https://evil.example/steal",
  "public": false,
  "default_permissions": {"contents": "read"},
  "default_events": []
}`

// commandStore stores the key with sh into a file in a temp dir.
func commandStore(t *testing.T) (keyStore, string) {
	out := filepath.Join(t.TempDir(), "stored-key")
	return keyStore{command: []string{"sh", "-c", `cat > "$1"`, "sh", out}}, out
}

func testRegistrar(t *testing.T, store keyStore) *registrar {
	t.Helper()
	manifest, err := loadManifest(writeManifest(t, validManifest))
	if err != nil {
		t.Fatal(err)
	}
	r, err := newRegistrar(context.Background(), manifest, "", "127.0.0.1:3999", store)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func serve(r *registrar, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Host = r.host
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func TestRegisterEndToEnd(t *testing.T) {
	fake, _ := newFakeConversion(t)
	manifestPath := writeManifest(t, validManifest)
	out := filepath.Join(t.TempDir(), "stored-key")

	pr, pw := io.Pipe()
	var stderr bytes.Buffer
	exit := make(chan int, 1)
	go func() {
		code := run([]string{"register", "--manifest", manifestPath, "--port", "0",
			"--", "sh", "-c", `cat > "$1"`, "sh", out}, nil, pw, &stderr)
		pw.Close()
		exit <- code
	}()

	reader := bufio.NewReader(pr)
	first, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("reading the first line: %v (stderr %s)", err, stderr.String())
	}
	match := regexp.MustCompile(`^open (http://127\.0\.0\.1:[0-9]+)(/[A-Za-z0-9_-]{32}/) in a browser signed in to GitHub\n$`).FindStringSubmatch(first)
	if match == nil {
		t.Fatalf("first line = %q", first)
	}
	base := match[1]

	// Without the printed path the state stays hidden.
	resp, err := http.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	hidden, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound || strings.Contains(string(hidden), "state=") {
		t.Fatalf("GET / status %d, body %q", resp.StatusCode, hidden)
	}

	resp, err = http.Get(base + match[2])
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / status %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("Referrer-Policy = %q", got)
	}
	action := regexp.MustCompile(`action="([^"]*)"`).FindSubmatch(page)
	value := regexp.MustCompile(`name="manifest" value="([^"]*)"`).FindSubmatch(page)
	if action == nil || value == nil {
		t.Fatalf("form not found in page:\n%s", page)
	}
	postURL, err := url.Parse(html.UnescapeString(string(action[1])))
	if err != nil {
		t.Fatal(err)
	}
	if postURL.Scheme+"://"+postURL.Host+postURL.Path != "https://github.com/settings/apps/new" {
		t.Errorf("form action = %s", postURL)
	}
	state := postURL.Query().Get("state")
	if len(state) < 32 {
		t.Fatalf("state %q is too short", state)
	}
	var manifest map[string]any
	if err := json.Unmarshal([]byte(html.UnescapeString(string(value[1]))), &manifest); err != nil {
		t.Fatalf("manifest in form: %v", err)
	}
	if manifest["redirect_url"] != base+"/callback" {
		t.Errorf("redirect_url = %v, want %s/callback", manifest["redirect_url"], base)
	}
	if manifest["name"] != "My Test App" || manifest["url"] != "https://example.com" {
		t.Errorf("manifest fields changed: %v", manifest)
	}

	get := func(target string) (int, string) {
		resp, err := http.Get(base + target)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	if status, _ := get("/favicon.ico"); status != http.StatusNotFound {
		t.Errorf("unknown path status %d", status)
	}
	if status, _ := get("/callback?state=wrong&code=" + testCode); status != http.StatusBadRequest {
		t.Errorf("wrong state status %d", status)
	}
	if status, _ := get("/callback?state=" + url.QueryEscape(state)); status != http.StatusBadRequest {
		t.Errorf("missing code status %d", status)
	}
	if n := fake.callCount(); n != 0 {
		t.Fatalf("conversion called %d times before a valid callback", n)
	}

	status, body := get("/callback?state=" + url.QueryEscape(state) + "&code=" + testCode)
	if status != http.StatusOK {
		t.Fatalf("callback status %d: %s", status, body)
	}
	if !strings.Contains(body, "https://github.com/apps/my-test-app/installations/new") || !strings.Contains(body, "4242") {
		t.Errorf("callback page lacks the App's facts:\n%s", body)
	}
	assertNoSecrets(t, "callback page", body)

	rest, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if code := <-exit; code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	if fake.callCount() != 1 {
		t.Errorf("conversion called %d times", fake.callCount())
	}
	stored, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(stored) != testPEM {
		t.Errorf("stored key = %q, want the exact PEM", stored)
	}
	output := first + string(rest)
	for _, want := range []string{"app id:   4242", "slug:     my-test-app", "https://github.com/apps/my-test-app",
		"install:  https://github.com/apps/my-test-app/installations/new", "stored in the standard input of", "discarded"} {
		if !strings.Contains(output, want) {
			t.Errorf("output lacks %q:\n%s", want, output)
		}
	}
	assertNoSecrets(t, "stdout", output)
	assertNoSecrets(t, "stderr", stderr.String())
	if strings.Contains(output, testClientID) {
		t.Errorf("output contains the client ID:\n%s", output)
	}
}

// Fake sops bodies. Each records its arguments and input next to itself.
const (
	// fakeSopsEncrypts replaces the key's value with an ENC[...] value.
	fakeSopsEncrypts = `key=$(head -n 1 "$dir/stdin" | sed 's/: |$//')
printf '%s: ENC[AES256_GCM,data:Zm9vYmFy,iv:aXY=,tag:dGFn,type:str]\nsops:\n    version: 3.9.0\n' "$key"
`
	// fakeSopsPlaintext echoes its input, as an encrypted_regex rule that
	// does not match the key would.
	fakeSopsPlaintext = `cat "$dir/stdin"
printf 'sops:\n    version: 3.9.0\n'
`
	// fakeSopsNoENC drops the key's value without encrypting it.
	fakeSopsNoENC = `key=$(head -n 1 "$dir/stdin" | sed 's/: |$//')
printf '%s: redacted\n' "$key"
`
	// fakeSopsLeaksRealKey encrypts the preflight's synthetic key but
	// echoes any other input.
	fakeSopsLeaksRealKey = `if grep -q PREFLIGHT "$dir/stdin"; then
` + fakeSopsEncrypts + `else
cat "$dir/stdin"
fi
`
)

// useFakeSops installs a fake sops running body and returns its directory.
func useFakeSops(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"dir=$(dirname \"$0\")\n" +
		"printf '%s\\n' \"$@\" > \"$dir/args\"\n" +
		"cat > \"$dir/stdin\"\n" + body
	if err := os.WriteFile(filepath.Join(dir, "sops"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	old := sopsCommand
	sopsCommand = filepath.Join(dir, "sops")
	t.Cleanup(func() { sopsCommand = old })
	return dir
}

func TestRegisterSopsStore(t *testing.T) {
	newFakeConversion(t)
	dir := useFakeSops(t, fakeSopsEncrypts)
	target := filepath.Join(t.TempDir(), "app.yaml")
	store := keyStore{sopsFile: target, sopsKey: "github_app_private_key"}
	if err := store.check(context.Background()); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	r := testRegistrar(t, store)
	rec := serve(r, "/callback?state="+url.QueryEscape(r.state)+"&code="+testCode)
	if rec.Code != http.StatusOK {
		t.Fatalf("callback status %d: %s", rec.Code, rec.Body.String())
	}
	result := <-r.done
	if result.err != nil {
		t.Fatal(result.err)
	}

	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	if want := "encrypt\n--filename-override\n" + target + "\n/dev/stdin\n"; string(args) != want {
		t.Errorf("sops args = %q, want %q", args, want)
	}
	stdin, _ := os.ReadFile(filepath.Join(dir, "stdin"))
	want := `"github_app_private_key": |` + "\n" +
		"    -----BEGIN RSA PRIVATE KEY-----\n" +
		"    MIIEfakeKeyMaterialLineOne\n" +
		"    fakeKeyMaterialLineTwo==\n" +
		"    -----END RSA PRIVATE KEY-----\n"
	if string(stdin) != want {
		t.Errorf("sops input = %q, want %q", stdin, want)
	}
	written, _ := os.ReadFile(target)
	if !strings.HasPrefix(string(written), `"github_app_private_key": ENC[AES256_GCM,`) {
		t.Errorf("target = %q", written)
	}
	assertNoSecrets(t, "target", string(written))
}

func TestSopsDocumentQuotesKeyName(t *testing.T) {
	for _, name := range []string{"123", "true"} {
		doc := keyStore{sopsKey: name}.sopsDocument(testPEM)
		if !strings.HasPrefix(doc, `"`+name+`": |`+"\n") {
			t.Errorf("document for %s starts %q", name, doc)
		}
	}
}

func TestSopsPreflightRefuses(t *testing.T) {
	for _, tt := range []struct {
		name string
		body string
	}{
		{"plaintext", fakeSopsPlaintext},
		{"no ENC", fakeSopsNoENC},
		{"failing sops", "exit 1\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			useFakeSops(t, tt.body)
			target := filepath.Join(t.TempDir(), "app.yaml")
			err := keyStore{sopsFile: target, sopsKey: "k"}.check(context.Background())
			if err == nil || !strings.Contains(err.Error(), "sops would not encrypt k in "+target+"; check .sops.yaml creation rules") {
				t.Errorf("error = %v", err)
			}
		})
	}
}

func TestRegisterRefusesPlaintextSopsOutput(t *testing.T) {
	newFakeConversion(t)
	useFakeSops(t, fakeSopsLeaksRealKey)
	target := filepath.Join(t.TempDir(), "app.yaml")
	store := keyStore{sopsFile: target, sopsKey: "k"}
	if err := store.check(context.Background()); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	r := testRegistrar(t, store)
	rec := serve(r, "/callback?state="+url.QueryEscape(r.state)+"&code="+testCode)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d", rec.Code)
	}
	result := <-r.done
	if result.err == nil {
		t.Fatal("plaintext output was accepted")
	}
	msg := result.err.Error()
	for _, want := range []string{"App 4242", "nothing was written", "generate a new private key"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q lacks %q", msg, want)
		}
	}
	assertNoSecrets(t, "error", msg)
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("target was written: %v", err)
	}
}

func TestStoreCheckRefuses(t *testing.T) {
	useFakeSops(t, fakeSopsEncrypts)
	missingDir := filepath.Join(t.TempDir(), "missing", "app.yaml")
	if err := (keyStore{sopsFile: missingDir, sopsKey: "k"}).check(context.Background()); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("missing directory: error = %v", err)
	}
	err := keyStore{command: []string{"gh-app-test-no-such-command"}}.check(context.Background())
	if err == nil || !strings.Contains(err.Error(), "store command") {
		t.Errorf("missing command: error = %v", err)
	}
	sopsCommand = filepath.Join(t.TempDir(), "no-sops")
	err = keyStore{sopsFile: filepath.Join(t.TempDir(), "app.yaml"), sopsKey: "k"}.check(context.Background())
	if err == nil || !strings.Contains(err.Error(), "sops:") {
		t.Errorf("missing sops: error = %v", err)
	}
}

func TestRegisterRefusesExistingSopsFile(t *testing.T) {
	fake, _ := newFakeConversion(t)
	target := filepath.Join(t.TempDir(), "app.yaml")
	if err := os.WriteFile(target, []byte("existing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"register", "--manifest", writeManifest(t, validManifest), "--port", "0",
		"--sops-file", target, "--sops-key", "key"}, nil, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "already exists") {
		t.Errorf("stderr = %q", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing (no server started)", stdout.String())
	}
	if fake.callCount() != 0 {
		t.Error("conversion was called")
	}
}

func TestRegisterStoreFailureWithholdsOutput(t *testing.T) {
	newFakeConversion(t)
	r := testRegistrar(t, keyStore{command: []string{"sh", "-c", "cat >&2; echo leaked-output >&2; exit 3"}})
	rec := serve(r, "/callback?state="+url.QueryEscape(r.state)+"&code="+testCode)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d", rec.Code)
	}
	result := <-r.done
	if result.err == nil {
		t.Fatal("store failure was not reported")
	}
	msg := result.err.Error()
	if !strings.Contains(msg, "exit status 3") || !strings.Contains(msg, "4242") {
		t.Errorf("error = %q", msg)
	}
	if strings.Contains(msg, "leaked-output") || strings.Contains(rec.Body.String(), "leaked-output") {
		t.Errorf("store output leaked: %q", msg)
	}
	assertNoSecrets(t, "error", msg)
	assertNoSecrets(t, "page", rec.Body.String())

	// The registration is over: a replayed callback is refused.
	rec = serve(r, "/callback?state="+url.QueryEscape(r.state)+"&code="+testCode)
	if rec.Code != http.StatusConflict {
		t.Errorf("replayed callback status %d", rec.Code)
	}
}

func TestRegisterStoreTimesOut(t *testing.T) {
	saved := storeTimeout
	storeTimeout = 200 * time.Millisecond
	t.Cleanup(func() { storeTimeout = saved })
	newFakeConversion(t)
	r := testRegistrar(t, keyStore{command: []string{"sleep", "30"}})
	start := time.Now()
	rec := serve(r, "/callback?state="+url.QueryEscape(r.state)+"&code="+testCode)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d", rec.Code)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("store ran for %v", elapsed)
	}
	if result := <-r.done; result.err == nil {
		t.Fatal("a stalled store was not reported")
	}
}

func TestStoreTimeoutKillsChildren(t *testing.T) {
	saved := storeTimeout
	storeTimeout = 200 * time.Millisecond
	t.Cleanup(func() { storeTimeout = saved })
	marker := filepath.Join(t.TempDir(), "late")
	// The child keeps stdout open and would write the marker after the timeout.
	store := keyStore{command: []string{"sh", "-c", `(sleep 2; touch "$1") & wait`, "sh", marker}}
	start := time.Now()
	if err := store.save(context.Background(), "-----BEGIN TEST KEY-----\nAAAA\n-----END TEST KEY-----\n"); err == nil || !strings.Contains(err.Error(), "stopped after") {
		t.Fatalf("a stalled store was reported as %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("store ran for %v", elapsed)
	}
	time.Sleep(3 * time.Second)
	if _, err := os.Stat(marker); err == nil {
		t.Error("the store's child kept running after the timeout")
	}
}

func TestStoreInterruptKillsChildren(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "late")
	store := keyStore{command: []string{"sh", "-c", `(sleep 2; touch "$1") & wait`, "sh", marker}}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	start := time.Now()
	if err := store.save(ctx, "-----BEGIN TEST KEY-----\nAAAA\n-----END TEST KEY-----\n"); err == nil || !strings.Contains(err.Error(), "stopped by an interrupt") {
		t.Fatalf("an interrupted store was reported as %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("store ran for %v", elapsed)
	}
	time.Sleep(3 * time.Second)
	if _, err := os.Stat(marker); err == nil {
		t.Error("the store's child kept running after the interrupt")
	}
}

func TestRegisterConversionErrorShowsOnlyMessage(t *testing.T) {
	fake, _ := newFakeConversion(t)
	fake.status = http.StatusUnprocessableEntity
	fake.body = `{"message":"Validation Failed","documentation_url":"https://docs.example","detail":"body-detail-text"}`
	store, out := commandStore(t)
	r := testRegistrar(t, store)
	rec := serve(r, "/callback?state="+url.QueryEscape(r.state)+"&code="+testCode)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d", rec.Code)
	}
	result := <-r.done
	if result.err == nil {
		t.Fatal("conversion failure was not reported")
	}
	msg := result.err.Error()
	if !strings.HasPrefix(msg, "conversion failed: HTTP 422: Validation Failed; ") || !strings.Contains(msg, "probably created the App") {
		t.Errorf("error = %q", msg)
	}
	if strings.Contains(rec.Body.String(), "body-detail-text") {
		t.Error("page shows the response body")
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("store ran after a failed conversion")
	}
}

func TestRegisterConversionDoesNotFollowRedirects(t *testing.T) {
	fake, _ := newFakeConversion(t)
	fake.status = http.StatusFound
	fake.body = `{"pem":"x"}`
	store, _ := commandStore(t)
	r := testRegistrar(t, store)
	serve(r, "/callback?state="+url.QueryEscape(r.state)+"&code="+testCode)
	result := <-r.done
	if result.err == nil || !strings.HasPrefix(result.err.Error(), "conversion failed: HTTP 302; ") {
		t.Errorf("error = %v", result.err)
	}
}

func TestRegisterConversionTransportErrorHidesCode(t *testing.T) {
	setConversionBaseURL(t, "http://127.0.0.1:1")
	store, _ := commandStore(t)
	r := testRegistrar(t, store)
	serve(r, "/callback?state="+url.QueryEscape(r.state)+"&code="+testCode)
	result := <-r.done
	if result.err == nil {
		t.Fatal("no error")
	}
	assertNoSecrets(t, "error", result.err.Error())
}

func TestRegisterRefusesOtherHosts(t *testing.T) {
	fake, _ := newFakeConversion(t)
	store, _ := commandStore(t)
	r := testRegistrar(t, store)
	req := httptest.NewRequest(http.MethodGet, "/callback?state="+url.QueryEscape(r.state)+"&code="+testCode, nil)
	req.Host = "rebound.example:3999"
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status %d", rec.Code)
	}
	if fake.callCount() != 0 {
		t.Error("conversion was called")
	}
}

func TestRegisterOrgURL(t *testing.T) {
	manifest, err := loadManifest(writeManifest(t, validManifest))
	if err != nil {
		t.Fatal(err)
	}
	r, err := newRegistrar(context.Background(), manifest, "my-org", "127.0.0.1:3999", keyStore{command: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	want := "https://github.com/organizations/my-org/settings/apps/new?state=" + r.state
	if r.postURL != want {
		t.Errorf("postURL = %q, want %q", r.postURL, want)
	}
	if !strings.Contains(serve(r, r.pagePath).Body.String(), html.EscapeString(want)) {
		t.Error("form does not post to the organisation's URL")
	}
}

func TestLoadManifest(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{"valid", validManifest, ""},
		{"missing url", `{"name":"x"}`, `"url" is required`},
		{"empty url", `{"name":"x","url":""}`, `"url" must be a non-empty string`},
		{"missing name", `{"url":"https://example.com"}`, `"name" is required`},
		{"unknown key", `{"name":"x","url":"https://example.com","permissions":{}}`, `unknown field "permissions"`},
		{"not an object", `[1]`, "manifest:"},
		{"null", `null`, "not a JSON object"},
		{"trailing data", `{"name":"x","url":"https://example.com"} {}`, "unexpected data"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadManifest(writeManifest(t, tt.content))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestParseRegisterArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"sops", []string{"--manifest", "m", "--sops-file", "f.yaml", "--sops-key", "k_1"}, ""},
		{"sops yml", []string{"--manifest", "m", "--sops-file", "dir/f.yml", "--sops-key", "123"}, ""},
		{"sops json", []string{"--manifest", "m", "--sops-file", "f.json", "--sops-key", "k"}, ".yaml or .yml"},
		{"command", []string{"--manifest", "m", "--org", "my-org", "--", "pass", "insert", "-m", "x"}, ""},
		{"no manifest", []string{"--", "true"}, "--manifest is required"},
		{"no store", []string{"--manifest", "m"}, "give --sops-file"},
		{"both stores", []string{"--manifest", "m", "--sops-file", "f.yaml", "--sops-key", "k", "--", "true"}, "not both"},
		{"sops file only", []string{"--manifest", "m", "--sops-file", "f"}, "together"},
		{"bad sops key", []string{"--manifest", "m", "--sops-file", "f.yaml", "--sops-key", "a-b"}, "--sops-key"},
		{"bad org", []string{"--manifest", "m", "--org", "a/b", "--", "true"}, "--org"},
		{"bad port", []string{"--manifest", "m", "--port", "70000", "--", "true"}, "--port"},
		{"command without separator", []string{"--manifest", "m", "pass", "insert"}, "after --"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts, err := parseRegisterArgs(tt.args)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				if opts.port != defaultRegisterPort {
					t.Errorf("port = %d", opts.port)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want %q", err, tt.wantErr)
			}
		})
	}
	opts, err := parseRegisterArgs([]string{"--manifest", "m", "--", "pass", "insert", "-m", "github/x"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(opts.command, " ") != "pass insert -m github/x" {
		t.Errorf("command = %q", opts.command)
	}
}
