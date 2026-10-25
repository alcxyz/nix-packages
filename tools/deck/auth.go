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
// Discovery runs on first use, one attempt at a time; requests that arrive
// while it runs wait for it, and requests within five seconds after it failed
// get a 503 at once. An unreachable issuer at start therefore does not stop
// the service, and holds a request for at most one discovery timeout.
type Authenticator struct {
	issuer           string
	clientID         string
	groups           []string
	readOnlyGroups   []string
	client           *http.Client
	log              *slog.Logger
	discoveryTimeout time.Duration

	mu          sync.Mutex
	verifier    *oidc.IDTokenVerifier
	discovering chan struct{} // closed when the running attempt ends
	failedAt    time.Time
}

const retryAfter = 5 * time.Second

var errNoProvider = errors.New("identity provider unavailable")

func newAuthenticator(cfg AuthConfig, client *http.Client, log *slog.Logger) *Authenticator {
	return &Authenticator{
		issuer:           cfg.Issuer,
		clientID:         cfg.ClientID,
		groups:           cfg.fullAccessGroups(),
		readOnlyGroups:   cfg.ReadOnlyGroups,
		client:           client,
		log:              log,
		discoveryTimeout: 10 * time.Second,
	}
}

// getVerifier returns the verifier, running discovery if there is none yet.
// Only one attempt runs at a time, without holding the lock: callers that
// arrive while it runs wait for it, bounded by their own context and the
// discovery timeout, and use its result. For retryAfter after an attempt
// fails, callers get errNoProvider at once.
func (a *Authenticator) getVerifier(ctx context.Context) (*oidc.IDTokenVerifier, error) {
	a.mu.Lock()
	if a.verifier != nil {
		v := a.verifier
		a.mu.Unlock()
		return v, nil
	}
	if done := a.discovering; done != nil {
		a.mu.Unlock()
		return a.waitForDiscovery(ctx, done)
	}
	if !a.failedAt.IsZero() && time.Since(a.failedAt) < retryAfter {
		a.mu.Unlock()
		return nil, errNoProvider
	}
	done := make(chan struct{})
	a.discovering = done
	a.mu.Unlock()

	// The deferred reset also runs on a panic, so discovery cannot stay
	// marked as running.
	var verifier *oidc.IDTokenVerifier
	defer func() {
		a.mu.Lock()
		a.discovering = nil
		if verifier != nil {
			a.verifier = verifier
		} else {
			a.failedAt = time.Now()
		}
		a.mu.Unlock()
		close(done)
	}()

	// The key set keeps this context for its fetches, so it must outlive the
	// request.
	pctx := oidc.ClientContext(context.Background(), a.client)
	dctx, cancel := context.WithTimeout(pctx, a.discoveryTimeout)
	defer cancel()
	provider, err := oidc.NewProvider(dctx, a.issuer)
	if err != nil {
		a.log.Warn("oidc discovery failed", "err", err.Error())
		return nil, errNoProvider
	}
	verifier = provider.VerifierContext(pctx, &oidc.Config{ClientID: a.clientID})
	return verifier, nil
}

// waitForDiscovery waits for the attempt that closes done and returns its
// verifier, or errNoProvider if it failed or the wait ran out first.
func (a *Authenticator) waitForDiscovery(ctx context.Context, done <-chan struct{}) (*oidc.IDTokenVerifier, error) {
	timer := time.NewTimer(a.discoveryTimeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-ctx.Done():
		return nil, errNoProvider
	case <-timer.C:
		return nil, errNoProvider
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.verifier == nil {
		return nil, errNoProvider
	}
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
	v, err := a.getVerifier(r.Context())
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
