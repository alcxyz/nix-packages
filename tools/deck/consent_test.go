package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	consentTestID    = "0123456789abcdef0123456789abcdef"
	consentTestAgent = "https://deck.example.com"
)

func newConsentServer(t *testing.T) (http.Handler, string) {
	t.Helper()
	d, _ := newTestDeck(t)
	spool := t.TempDir()
	d.cfg.Consent = &ConsentConfig{SpoolDir: spool}
	h, err := newHandler(d, nil, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	return h, spool
}

func writeConsentRequest(t *testing.T, spool, id, title string, expires time.Time) string {
	t.Helper()
	data := fmt.Sprintf(`{"version":1,  "id":%q,"createdAt":%d,"expiresAt":%d,"title":%q,"requester":"No T3 thread"}`,
		id, time.Now().Unix(), expires.Unix(), title)
	if err := os.WriteFile(filepath.Join(spool, id+".request.json"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return data
}

func postJSON(h http.Handler, path, body, origin string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestConsentListShowsLiveRequestsEscaped(t *testing.T) {
	h, spool := newConsentServer(t)
	writeConsentRequest(t, spool, consentTestID, "<script>alert(1)</script>", time.Now().Add(time.Minute))
	writeConsentRequest(t, spool, "fedcba9876543210fedcba9876543210", "Expired one", time.Now().Add(-time.Minute))
	rec := get(t, h, "/consent/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "<script>alert(1)") {
		t.Fatal("the title was not escaped")
	}
	if !strings.Contains(body, "/consent/"+consentTestID) {
		t.Fatal("the live request is missing")
	}
	if strings.Contains(body, "Expired one") {
		t.Fatal("an expired request is listed")
	}
}

func TestConsentListWithoutASpool(t *testing.T) {
	d, _ := newTestDeck(t)
	d.cfg.Consent = &ConsentConfig{SpoolDir: filepath.Join(t.TempDir(), "missing")}
	h, err := newHandler(d, nil, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	rec := get(t, h, "/consent/")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Nothing is waiting") {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
}

// The page signs what this returns, so it must be the file's exact bytes.
func TestConsentRequestBytesAreVerbatim(t *testing.T) {
	h, spool := newConsentServer(t)
	data := writeConsentRequest(t, spool, consentTestID, "Title", time.Now().Add(time.Minute))
	rec := get(t, h, "/consent/"+consentTestID+"/request")
	if rec.Code != http.StatusOK || rec.Body.String() != data {
		t.Fatalf("status %d: %q", rec.Code, rec.Body)
	}
}

func TestConsentRefusesUnknownMalformedAndExpired(t *testing.T) {
	h, spool := newConsentServer(t)
	writeConsentRequest(t, spool, consentTestID, "Expired", time.Now().Add(-time.Second))
	for _, id := range []string{consentTestID, "fedcba9876543210fedcba9876543210", "not-an-id", "..%2f..%2fsecret"} {
		if rec := get(t, h, "/consent/"+id+"/request"); rec.Code != http.StatusNotFound {
			t.Errorf("%s request: status %d", id, rec.Code)
		}
		if rec := postJSON(h, "/consent/"+id+"/deny", "", consentTestAgent); rec.Code != http.StatusNotFound {
			t.Errorf("%s deny: status %d", id, rec.Code)
		}
	}
	if _, err := os.Stat(filepath.Join(spool, consentTestID+".response.json")); err == nil {
		t.Fatal("an expired request took an answer")
	}
}

func TestConsentRejectsARequestUnderAnotherName(t *testing.T) {
	h, spool := newConsentServer(t)
	data := writeConsentRequest(t, spool, consentTestID, "Title", time.Now().Add(time.Minute))
	other := "fedcba9876543210fedcba9876543210"
	if err := os.WriteFile(filepath.Join(spool, other+".request.json"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if rec := get(t, h, "/consent/"+other+"/request"); rec.Code != http.StatusNotFound {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestConsentApproveRelaysTheAssertion(t *testing.T) {
	h, spool := newConsentServer(t)
	writeConsentRequest(t, spool, consentTestID, "Title", time.Now().Add(time.Minute))
	rejected := filepath.Join(spool, consentTestID+".rejected")
	if err := os.WriteFile(rejected, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	body := `{"clientDataJSON":"YWJj","authenticatorData":"ZGVm","signature":"Z2hp"}`
	rec := postJSON(h, "/consent/"+consentTestID+"/approve", body, consentTestAgent)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	data, err := os.ReadFile(filepath.Join(spool, consentTestID+".response.json"))
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Decision  string            `json:"decision"`
		Assertion map[string]string `json:"assertion"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		t.Fatal(err)
	}
	if response.Decision != "approve" || response.Assertion["signature"] != "Z2hp" ||
		response.Assertion["clientDataJSON"] != "YWJj" || response.Assertion["authenticatorData"] != "ZGVm" {
		t.Fatalf("relayed %s", data)
	}
	if _, err := os.Stat(rejected); err == nil {
		t.Fatal("the earlier rejection was not cleared")
	}
}

func TestConsentApproveRefusesMalformedBodies(t *testing.T) {
	h, spool := newConsentServer(t)
	writeConsentRequest(t, spool, consentTestID, "Title", time.Now().Add(time.Minute))
	for _, body := range []string{
		"", "not json", "{}",
		`{"clientDataJSON":"YWJj","authenticatorData":"ZGVm"}`,
		`{"clientDataJSON":"!!","authenticatorData":"ZGVm","signature":"Z2hp"}`,
		`{"clientDataJSON":"YWJj","authenticatorData":"ZGVm","signature":"Z2hp","decision":"approve"}`,
		`{"clientDataJSON":"` + strings.Repeat("a", maxConsentAnswerBytes) + `","authenticatorData":"ZGVm","signature":"Z2hp"}`,
	} {
		if rec := postJSON(h, "/consent/"+consentTestID+"/approve", body, consentTestAgent); rec.Code != http.StatusBadRequest {
			t.Errorf("body %.40q: status %d", body, rec.Code)
		}
	}
	if _, err := os.Stat(filepath.Join(spool, consentTestID+".response.json")); err == nil {
		t.Fatal("a malformed approval was relayed")
	}
}

func TestConsentDenyAndOriginCheck(t *testing.T) {
	h, spool := newConsentServer(t)
	writeConsentRequest(t, spool, consentTestID, "Title", time.Now().Add(time.Minute))
	response := filepath.Join(spool, consentTestID+".response.json")
	for _, origin := range []string{"", "https://evil.example.com"} {
		if rec := postJSON(h, "/consent/"+consentTestID+"/deny", "", origin); rec.Code != http.StatusForbidden {
			t.Fatalf("origin %q: status %d", origin, rec.Code)
		}
	}
	if _, err := os.Stat(response); err == nil {
		t.Fatal("a cross-origin deny was relayed")
	}
	if rec := postJSON(h, "/consent/"+consentTestID+"/deny", "", consentTestAgent); rec.Code != http.StatusAccepted {
		t.Fatalf("status %d", rec.Code)
	}
	data, err := os.ReadFile(response)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"decision":"deny"}` {
		t.Fatalf("relayed %s", data)
	}
}

func TestConsentStatus(t *testing.T) {
	h, spool := newConsentServer(t)
	writeConsentRequest(t, spool, consentTestID, "Title", time.Now().Add(time.Minute))
	status := func() string {
		rec := get(t, h, "/consent/"+consentTestID+"/status")
		var body struct {
			State string `json:"state"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body.State
	}
	if got := status(); got != "pending" {
		t.Fatalf("state %s", got)
	}
	if err := os.WriteFile(filepath.Join(spool, consentTestID+".rejected"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := status(); got != "rejected" {
		t.Fatalf("state %s", got)
	}
	if err := os.Remove(filepath.Join(spool, consentTestID+".request.json")); err != nil {
		t.Fatal(err)
	}
	if got := status(); got != "gone" {
		t.Fatalf("state %s", got)
	}
}

func TestConsentIsNotHeldUpByAFIFO(t *testing.T) {
	h, spool := newConsentServer(t)
	if err := syscall.Mkfifo(filepath.Join(spool, consentTestID+".request.json"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() { done <- get(t, h, "/consent/"+consentTestID+"/request").Code }()
	select {
	case code := <-done:
		if code != http.StatusNotFound {
			t.Fatalf("status %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the request hung on a FIFO")
	}
}

func TestConsentPathIsNotAContentCollection(t *testing.T) {
	d, _ := newTestDeck(t)
	d.cfg.Consent = &ConsentConfig{SpoolDir: t.TempDir()}
	dir := filepath.Join(d.cfg.ContentDir, "consent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "page.html"), []byte("<p>bundle</p>"), 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := newHandler(d, nil, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	if rec := get(t, h, "/"); strings.Contains(rec.Body.String(), `href="/consent/page`) {
		t.Fatal("the index links a collection under the approvals path")
	}
}

func TestConsentRoutesAreOffWithoutConfig(t *testing.T) {
	_, h, _ := newTestServer(t)
	if rec := get(t, h, "/consent/"); rec.Code != http.StatusNotFound {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestConsentConfig(t *testing.T) {
	cfg := testConfig(t)
	cfg.Consent = &ConsentConfig{SpoolDir: "relative"}
	if err := cfg.normalize(); err == nil {
		t.Fatal("a relative spool was accepted")
	}
	cfg.Consent = &ConsentConfig{SpoolDir: "/run/user/1000/credential-consent"}
	cfg.BasePath = "/consent/"
	if err := cfg.normalize(); err == nil {
		t.Fatal("an overlapping basePath was accepted")
	}
}

func TestConsentPagesLoadTheirScript(t *testing.T) {
	h, _ := newConsentServer(t)
	for _, path := range []string{"/consent/" + consentTestID, "/consent/enroll"} {
		rec := get(t, h, path)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "/threads/static/consent.js") {
			t.Fatalf("%s: status %d", path, rec.Code)
		}
	}
	if rec := get(t, h, "/threads/static/consent.js"); rec.Code != http.StatusOK {
		t.Fatalf("script: status %d", rec.Code)
	}
}
