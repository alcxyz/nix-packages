package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// Authenticator checks the OIDC ID token the proxy forwards in the
// Authorization header: issuer, audience, expiry and group membership.
// Discovery runs on first use and is retried, so an unreachable issuer at
// start does not stop the service.
type Authenticator struct {
	issuer         string
	clientID       string
	groups         []string
	readOnlyGroups []string
	client         *http.Client
	log            *slog.Logger

	mu       sync.Mutex
	verifier *oidc.IDTokenVerifier
	lastTry  time.Time
}

var errNoProvider = errors.New("identity provider unavailable")

func newAuthenticator(cfg AuthConfig, client *http.Client, log *slog.Logger) *Authenticator {
	return &Authenticator{
		issuer:         cfg.Issuer,
		clientID:       cfg.ClientID,
		groups:         cfg.fullAccessGroups(),
		readOnlyGroups: cfg.ReadOnlyGroups,
		client:         client,
		log:            log,
	}
}

func (a *Authenticator) getVerifier() (*oidc.IDTokenVerifier, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.verifier != nil {
		return a.verifier, nil
	}
	if time.Since(a.lastTry) < 5*time.Second {
		return nil, errNoProvider
	}
	a.lastTry = time.Now()
	// The key set keeps this context for its fetches, so it must outlive the
	// request.
	pctx := oidc.ClientContext(context.Background(), a.client)
	dctx, cancel := context.WithTimeout(pctx, 10*time.Second)
	defer cancel()
	provider, err := oidc.NewProvider(dctx, a.issuer)
	if err != nil {
		a.log.Warn("oidc discovery failed", "err", err.Error())
		return nil, errNoProvider
	}
	a.verifier = provider.VerifierContext(pctx, &oidc.Config{ClientID: a.clientID})
	return a.verifier, nil
}

type authResult int

const (
	authOK authResult = iota
	authReadOnly
	authMissing
	authInvalid
	authForbidden
	authUnavailable
)

// check verifies a request's bearer token. A token with a full-access group
// gives authOK; one with only a read-only group gives authReadOnly.
func (a *Authenticator) check(r *http.Request) (authResult, string) {
	h := r.Header.Get("Authorization")
	token, ok := strings.CutPrefix(h, "Bearer ")
	if !ok || strings.TrimSpace(token) == "" {
		return authMissing, ""
	}
	v, err := a.getVerifier()
	if err != nil {
		return authUnavailable, ""
	}
	idToken, err := v.Verify(r.Context(), strings.TrimSpace(token))
	if err != nil {
		// go-oidc flattens the key set's error into text; a token that could
		// not be checked because the keys could not be fetched is the
		// issuer's outage, not the user's token. Only the prefix of the
		// signature check's error is matched: other errors quote the
		// token's own claims, which the client chooses.
		if strings.HasPrefix(err.Error(), "failed to verify signature: fetching keys") {
			a.log.Warn("oidc key fetch failed", "err", err.Error())
			return authUnavailable, ""
		}
		return authInvalid, ""
	}
	var claims struct {
		Groups []string `json:"groups"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return authInvalid, ""
	}
	if hasAnyGroup(claims.Groups, a.groups) {
		return authOK, idToken.Subject
	}
	if hasAnyGroup(claims.Groups, a.readOnlyGroups) {
		return authReadOnly, idToken.Subject
	}
	return authForbidden, idToken.Subject
}

// requireAuth wraps a handler with the token check. A nil authenticator
// means -dev-no-auth.
func requireAuth(a *Authenticator, log *slog.Logger, next http.Handler) http.Handler {
	if a == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		res, _ := a.check(r)
		switch res {
		case authOK:
			next.ServeHTTP(w, r)
		case authReadOnly:
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				log.Info("request refused: read-only access", "path", r.URL.Path, "method", r.Method)
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		case authForbidden:
			log.Info("request refused: missing group", "path", r.URL.Path)
			http.Error(w, "forbidden", http.StatusForbidden)
		case authUnavailable:
			http.Error(w, "identity provider unavailable", http.StatusServiceUnavailable)
		default:
			w.Header().Set("WWW-Authenticate", `Bearer`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		}
	})
}

func hasAnyGroup(have, want []string) bool {
	for _, h := range have {
		for _, w := range want {
			if h == w {
				return true
			}
		}
	}
	return false
}

// sameOrigin requires state-changing requests to come from the public
// origin. Only POST changes state.
func sameOrigin(origin string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}
		got, ok := normalizeOrigin(r.Header.Get("Origin"))
		if !ok || got != origin {
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
