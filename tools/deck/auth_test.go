package main

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
