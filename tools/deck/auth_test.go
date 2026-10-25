package main

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

// testIssuer is an OIDC provider with discovery and a key set, signing
// tokens with its own key.
type testIssuer struct {
	srv      *httptest.Server
	key      *rsa.PrivateKey
	keysDown atomic.Bool
}

func newTestIssuer(t *testing.T) *testIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ti := &testIssuer{key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                ti.srv.URL,
			"jwks_uri":                              ti.srv.URL + "/jwks",
			"authorization_endpoint":                ti.srv.URL + "/authorize",
			"token_endpoint":                        ti.srv.URL + "/token",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		if ti.keysDown.Load() {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	ti.srv = httptest.NewServer(mux)
	t.Cleanup(ti.srv.Close)
	return ti
}

func (ti *testIssuer) sign(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", "k1"))
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(claims)
	jws, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	s, err := jws.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (ti *testIssuer) claims(mod func(map[string]any)) map[string]any {
	c := map[string]any{
		"iss":    ti.srv.URL,
		"sub":    "user-1",
		"aud":    "deck-client",
		"exp":    time.Now().Add(time.Hour).Unix(),
		"iat":    time.Now().Unix(),
		"groups": []string{"other", "deck-users"},
	}
	if mod != nil {
		mod(c)
	}
	return c
}

func TestAuth(t *testing.T) {
	ti := newTestIssuer(t)
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	auth := newAuthenticator(AuthConfig{Issuer: ti.srv.URL, ClientID: "deck-client", Group: "deck-users"}, ti.srv.Client(), quietLog())
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })
	h := requireAuth(auth, quietLog(), ok)

	cases := []struct {
		name   string
		header string
		want   int
	}{
		{"missing", "", http.StatusUnauthorized},
		{"not bearer", "Basic dXNlcjpwdw==", http.StatusUnauthorized},
		{"garbage", "Bearer not-a-token", http.StatusUnauthorized},
		{"valid", "Bearer " + ti.sign(t, ti.key, ti.claims(nil)), http.StatusTeapot},
		{"wrong key", "Bearer " + ti.sign(t, other, ti.claims(nil)), http.StatusUnauthorized},
		{"expired", "Bearer " + ti.sign(t, ti.key, ti.claims(func(c map[string]any) {
			c["exp"] = time.Now().Add(-time.Hour).Unix()
			c["iat"] = time.Now().Add(-2 * time.Hour).Unix()
		})), http.StatusUnauthorized},
		{"wrong audience", "Bearer " + ti.sign(t, ti.key, ti.claims(func(c map[string]any) { c["aud"] = "another-client" })), http.StatusUnauthorized},
		{"wrong issuer", "Bearer " + ti.sign(t, ti.key, ti.claims(func(c map[string]any) { c["iss"] = "https://evil.example.com" })), http.StatusUnauthorized},
		// Claims quoted in an error must not pass for a key outage.
		{"issuer naming an outage", "Bearer " + ti.sign(t, ti.key, ti.claims(func(c map[string]any) { c["iss"] = "x fetching keys" })), http.StatusUnauthorized},
		{"forged outage", "Bearer " + ti.sign(t, other, ti.claims(func(c map[string]any) { c["iss"] = "x fetching keys" })), http.StatusUnauthorized},
		{"missing group", "Bearer " + ti.sign(t, ti.key, ti.claims(func(c map[string]any) { c["groups"] = []string{"other"} })), http.StatusForbidden},
		{"no groups claim", "Bearer " + ti.sign(t, ti.key, ti.claims(func(c map[string]any) { delete(c, "groups") })), http.StatusForbidden},
	}
	for _, c := range cases {
		req := httptest.NewRequest("GET", "/threads/", nil)
		if c.header != "" {
			req.Header.Set("Authorization", c.header)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s: got %d, want %d", c.name, rec.Code, c.want)
		}
	}
}

func TestAuthGroups(t *testing.T) {
	ti := newTestIssuer(t)
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })
	token := func(groups ...string) string {
		return ti.sign(t, ti.key, ti.claims(func(c map[string]any) { c["groups"] = groups }))
	}
	cases := []struct {
		name   string
		cfg    AuthConfig
		method string
		groups []string
		want   int
	}{
		{"group only, member GET", AuthConfig{Group: "admins"}, "GET", []string{"admins"}, http.StatusTeapot},
		{"group only, member POST", AuthConfig{Group: "admins"}, "POST", []string{"admins"}, http.StatusTeapot},
		{"group only, non-member", AuthConfig{Group: "admins"}, "GET", []string{"other"}, http.StatusForbidden},
		{"groups list, second group POST", AuthConfig{Groups: []string{"admins", "ops"}}, "POST", []string{"ops"}, http.StatusTeapot},
		{"group and groups both count", AuthConfig{Group: "admins", Groups: []string{"ops"}}, "POST", []string{"admins"}, http.StatusTeapot},
		{"read-only GET", AuthConfig{Groups: []string{"admins"}, ReadOnlyGroups: []string{"viewers"}}, "GET", []string{"viewers"}, http.StatusTeapot},
		{"read-only HEAD", AuthConfig{Groups: []string{"admins"}, ReadOnlyGroups: []string{"viewers"}}, "HEAD", []string{"viewers"}, http.StatusTeapot},
		{"read-only POST", AuthConfig{Groups: []string{"admins"}, ReadOnlyGroups: []string{"viewers"}}, "POST", []string{"viewers"}, http.StatusForbidden},
		{"read-only PUT", AuthConfig{Groups: []string{"admins"}, ReadOnlyGroups: []string{"viewers"}}, "PUT", []string{"viewers"}, http.StatusForbidden},
		{"read-only groups alone", AuthConfig{ReadOnlyGroups: []string{"viewers"}}, "GET", []string{"viewers"}, http.StatusTeapot},
		{"full and read-only POST", AuthConfig{Groups: []string{"admins"}, ReadOnlyGroups: []string{"viewers"}}, "POST", []string{"viewers", "admins"}, http.StatusTeapot},
		{"no matching group", AuthConfig{Groups: []string{"admins"}, ReadOnlyGroups: []string{"viewers"}}, "GET", []string{"other"}, http.StatusForbidden},
	}
	for _, c := range cases {
		c.cfg.Issuer, c.cfg.ClientID = ti.srv.URL, "deck-client"
		h := requireAuth(newAuthenticator(c.cfg, ti.srv.Client(), quietLog()), quietLog(), ok)
		req := httptest.NewRequest(c.method, "/threads/actions/ack", nil)
		req.Header.Set("Authorization", "Bearer "+token(c.groups...))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s: got %d, want %d", c.name, rec.Code, c.want)
		}
	}
}

func TestValidateAuthGroups(t *testing.T) {
	cases := []struct {
		name string
		auth AuthConfig
		ok   bool
	}{
		{"group", AuthConfig{Group: "admins"}, true},
		{"groups", AuthConfig{Groups: []string{"admins", "ops"}}, true},
		{"read-only groups", AuthConfig{ReadOnlyGroups: []string{"viewers"}}, true},
		{"all", AuthConfig{Group: "admins", Groups: []string{"ops"}, ReadOnlyGroups: []string{"viewers"}}, true},
		{"none", AuthConfig{}, false},
		{"empty lists", AuthConfig{Groups: []string{}, ReadOnlyGroups: []string{}}, false},
		{"empty name", AuthConfig{Groups: []string{""}}, false},
		{"empty read-only name", AuthConfig{Group: "admins", ReadOnlyGroups: []string{""}}, false},
	}
	for _, c := range cases {
		c.auth.Issuer, c.auth.ClientID = "https://id.example.com", "deck"
		cfg := Config{Listen: "127.0.0.1:3775", Auth: c.auth}
		if err := cfg.validateAuth(false); (err == nil) != c.ok {
			t.Errorf("%s: err %v", c.name, err)
		}
	}
}

func TestLoadConfigGroups(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	body := `publicOrigin = "https://deck.example.com/"
[auth]
issuer = "https://id.example.com"
clientID = "deck"
groups = ["admins", "ops"]
readOnlyGroups = ["viewers"]
[[threads.instance]]
name = "main"
db = "/var/lib/example/state.sqlite"
webURL = "https://t3.example.com"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.Auth.Groups, []string{"admins", "ops"}) || !reflect.DeepEqual(cfg.Auth.ReadOnlyGroups, []string{"viewers"}) {
		t.Fatalf("auth: %+v", cfg.Auth)
	}
	if err := cfg.validateAuth(false); err != nil {
		t.Fatal(err)
	}
}

func TestAuthIssuerUnavailable(t *testing.T) {
	ti := newTestIssuer(t)
	token := ti.sign(t, ti.key, ti.claims(nil))
	ti.srv.Close()
	auth := newAuthenticator(AuthConfig{Issuer: ti.srv.URL, ClientID: "deck-client", Group: "deck-users"}, http.DefaultClient, quietLog())
	h := requireAuth(auth, quietLog(), http.NotFoundHandler())
	req := httptest.NewRequest("GET", "/threads/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestSameOrigin(t *testing.T) {
	h := sameOrigin("https://deck.example.com", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	cases := []struct {
		method, origin string
		want           int
	}{
		{"GET", "", http.StatusTeapot},
		{"POST", "", http.StatusForbidden},
		{"POST", "https://evil.example.com", http.StatusForbidden},
		{"POST", "https://deck.example.com.evil.example", http.StatusForbidden},
		{"POST", "null", http.StatusForbidden},
		{"POST", "https://deck.example.com", http.StatusTeapot},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, "/threads/actions/ack", nil)
		if c.origin != "" {
			req.Header.Set("Origin", c.origin)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s with origin %q: got %d, want %d", c.method, c.origin, rec.Code, c.want)
		}
	}
}

func TestDevNoAuthNeedsLoopback(t *testing.T) {
	for listen, ok := range map[string]bool{
		"127.0.0.1:3775": true,
		"[::1]:3775":     true,
		"localhost:3775": true,
		"0.0.0.0:3775":   false,
		":3775":          false,
		"10.0.0.5:3775":  false,
		"[::]:3775":      false,
	} {
		cfg := Config{Listen: listen}
		err := cfg.validateAuth(true)
		if (err == nil) != ok {
			t.Errorf("listen %s: err %v", listen, err)
		}
	}
	cfg := Config{Listen: "127.0.0.1:3775"}
	if err := cfg.validateAuth(false); err == nil {
		t.Error("auth without settings was accepted")
	}
	cfg.Auth = AuthConfig{Issuer: "http://id.example.com", ClientID: "c", Group: "g"}
	if err := cfg.validateAuth(false); err == nil || !strings.Contains(err.Error(), "https") {
		t.Errorf("a plain http issuer was accepted: %v", err)
	}
}

func TestAuthKeySetUnavailable(t *testing.T) {
	ti := newTestIssuer(t)
	ti.keysDown.Store(true)
	auth := newAuthenticator(AuthConfig{Issuer: ti.srv.URL, ClientID: "deck-client", Group: "deck-users"}, ti.srv.Client(), quietLog())
	h := requireAuth(auth, quietLog(), http.NotFoundHandler())
	req := httptest.NewRequest("GET", "/threads/", nil)
	req.Header.Set("Authorization", "Bearer "+ti.sign(t, ti.key, ti.claims(nil)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("a valid token with the keys unreachable: %d", rec.Code)
	}
}
