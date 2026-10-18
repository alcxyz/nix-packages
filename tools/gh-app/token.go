package main

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// A cached token is reused while more than this much of its life remains.
const minRemaining = 10 * time.Minute

const reposPerPage = 100

// A repository missing from the cached list triggers at most one new
// listing per this interval.
const relistInterval = 5 * time.Minute

// cacheEntry is the cached installation token and the repositories it covers.
type cacheEntry struct {
	InstallationID int64     `json:"installation_id"`
	Token          string    `json:"token"`
	ExpiresAt      time.Time `json:"expires_at"`
	Repositories   []string  `json:"repositories"`
	// ListedAt is when the repositories were last listed, ListTriedAt when
	// that was last tried.
	ListedAt    time.Time `json:"listed_at"`
	ListTriedAt time.Time `json:"list_tried_at"`
}

// listAttempt is the time of the last listing or attempt.
func (e cacheEntry) listAttempt() time.Time {
	if e.ListTriedAt.After(e.ListedAt) {
		return e.ListTriedAt
	}
	return e.ListedAt
}

func (e cacheEntry) installed(owner, repo string) bool {
	fullName := owner + "/" + repo
	for _, name := range e.Repositories {
		if strings.EqualFold(name, fullName) {
			return true
		}
	}
	return false
}

// minter mints and caches the App's installation token.
type minter struct {
	cfg    config
	client *http.Client
	now    func() time.Time
}

func newMinter(cfg config) *minter {
	return &minter{
		cfg:    cfg,
		client: &http.Client{Timeout: 30 * time.Second},
		now:    time.Now,
	}
}

func (m *minter) cachePath() string {
	return filepath.Join(m.cfg.cacheDir, m.cfg.appID+"-"+strings.ToLower(m.cfg.owner)+".json")
}

func (m *minter) fresh(entry cacheEntry) bool {
	return entry.Token != "" && entry.ExpiresAt.Sub(m.now()) > minRemaining
}

// token returns a cached token with enough life left, or mints a new one
// under an exclusive lock so concurrent invocations mint only once.
func (m *minter) token() (cacheEntry, error) {
	if err := m.cfg.checkMint(); err != nil {
		return cacheEntry{}, err
	}
	if entry, ok := m.readCache(); ok && m.fresh(entry) {
		return entry, nil
	}
	unlock, err := m.lock()
	if err != nil {
		return cacheEntry{}, err
	}
	defer unlock()

	// Another invocation may have refreshed the token while this one waited.
	if entry, ok := m.readCache(); ok && m.fresh(entry) {
		return entry, nil
	}
	entry, listErr, err := m.mint()
	if err != nil {
		// A cached token that has not yet expired still serves.
		if cached, ok := m.readCache(); ok && cached.Token != "" && cached.ExpiresAt.Sub(m.now()) > time.Minute {
			return cached, nil
		}
		return cacheEntry{}, err
	}
	if listErr != nil {
		// Keep the new token, so retries do not mint again, and the last
		// confirmed list of the same installation, if any.
		if cached, ok := m.readCache(); ok && cached.InstallationID == entry.InstallationID && !cached.ListedAt.IsZero() {
			entry.Repositories, entry.ListedAt = cached.Repositories, cached.ListedAt
		}
	}
	if err := m.writeCache(entry); err != nil {
		return cacheEntry{}, err
	}
	if listErr != nil && entry.ListedAt.IsZero() {
		return cacheEntry{}, listErr
	}
	return entry, nil
}

// tokenFor returns the token and whether owner/repo is installed. A
// repository missing from the cached list is looked up again with the
// current token, at most once per relistInterval, so a newly added
// repository is found before the list would otherwise refresh. Without a
// usable token nothing proves a repository is not installed, so every
// repository of the owner is then an error.
func (m *minter) tokenFor(owner, repo string) (cacheEntry, bool, error) {
	if err := m.cfg.checkMint(); err != nil {
		return cacheEntry{}, false, err
	}
	entry, err := m.token()
	if err != nil {
		return cacheEntry{}, false, err
	}
	if entry.installed(owner, repo) {
		return entry, true, nil
	}
	if m.now().Sub(entry.listAttempt()) < relistInterval {
		return notInstalled(entry, nil)
	}
	unlock, err := m.lock()
	if err != nil {
		return cacheEntry{}, false, err
	}
	defer unlock()
	// Another invocation may have listed while this one waited; its list
	// counts even when its token is too old to replace this one.
	if cached, ok := m.readCache(); ok {
		if m.fresh(cached) {
			entry = cached
		} else if cached.listAttempt().After(entry.listAttempt()) {
			entry.Repositories, entry.ListedAt, entry.ListTriedAt = cached.Repositories, cached.ListedAt, cached.ListTriedAt
		}
		if entry.installed(owner, repo) {
			return entry, true, nil
		}
		if m.now().Sub(entry.listAttempt()) < relistInterval {
			return notInstalled(entry, nil)
		}
	}
	// A failed listing still counts as an attempt, so it is not retried on
	// every invocation; the cached list keeps deciding while it is young.
	entry.ListTriedAt = m.now()
	repositories, listErr := m.listRepositories(entry.Token)
	if listErr == nil {
		entry.Repositories = repositories
		entry.ListedAt = entry.ListTriedAt
	}
	if err := m.writeCache(entry); err != nil {
		return cacheEntry{}, false, err
	}
	if entry.installed(owner, repo) {
		return entry, true, nil
	}
	return notInstalled(entry, listErr)
}

// notInstalled answers for a repository missing from entry's list. Only a
// confirmed list shows that; without one the answer is an error.
func notInstalled(entry cacheEntry, listErr error) (cacheEntry, bool, error) {
	if entry.ListedAt.IsZero() {
		if listErr != nil {
			return cacheEntry{}, false, listErr
		}
		return cacheEntry{}, false, errors.New("the installation's repositories have not been listed yet")
	}
	return entry, false, nil
}

// lock creates the cache directory and takes the exclusive cache lock.
func (m *minter) lock() (func(), error) {
	if err := os.MkdirAll(m.cfg.cacheDir, 0o700); err != nil {
		return nil, fmt.Errorf("create cache directory: %w", err)
	}
	if err := os.Chmod(m.cfg.cacheDir, 0o700); err != nil {
		return nil, fmt.Errorf("restrict cache directory: %w", err)
	}
	lockPath := filepath.Join(m.cfg.cacheDir, m.cfg.appID+"-"+strings.ToLower(m.cfg.owner)+".lock")
	lock, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open cache lock: %w", err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		lock.Close()
		return nil, fmt.Errorf("lock cache: %w", err)
	}
	return func() {
		syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		lock.Close()
	}, nil
}

func (m *minter) readCache() (cacheEntry, bool) {
	data, err := os.ReadFile(m.cachePath())
	if err != nil {
		return cacheEntry{}, false
	}
	var entry cacheEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return cacheEntry{}, false
	}
	return entry, true
}

func (m *minter) writeCache(entry cacheEntry) error {
	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("encode cache: %w", err)
	}
	tmp, err := os.CreateTemp(m.cfg.cacheDir, ".cache-*.tmp")
	if err != nil {
		return fmt.Errorf("create cache file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("restrict cache file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write cache file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write cache file: %w", err)
	}
	if err := os.Rename(tmpPath, m.cachePath()); err != nil {
		return fmt.Errorf("replace cache file: %w", err)
	}
	return nil
}

// mint looks up the installation, creates an installation token and lists
// the repositories it covers.
//
// When only the listing fails, the token is returned with listErr set and
// no list, so the caller can keep it.
func (m *minter) mint() (entry cacheEntry, listErr error, err error) {
	key, err := readPrivateKey(m.cfg.keyFile)
	if err != nil {
		return cacheEntry{}, nil, err
	}
	jwt, err := signJWT(key, m.cfg.appID, m.now())
	if err != nil {
		return cacheEntry{}, nil, err
	}

	var installation struct {
		ID int64 `json:"id"`
	}
	err = m.call("GET", "/users/"+m.cfg.owner+"/installation", jwt, &installation)
	var status *statusError
	if errors.As(err, &status) && status.code == http.StatusNotFound {
		err = m.call("GET", "/orgs/"+m.cfg.owner+"/installation", jwt, &installation)
	}
	if err != nil {
		return cacheEntry{}, nil, fmt.Errorf("find the App installation for %s: %w", m.cfg.owner, err)
	}
	if installation.ID == 0 {
		return cacheEntry{}, nil, fmt.Errorf("find the App installation for %s: no installation id in response", m.cfg.owner)
	}

	var access struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := m.call("POST", fmt.Sprintf("/app/installations/%d/access_tokens", installation.ID), jwt, &access); err != nil {
		return cacheEntry{}, nil, fmt.Errorf("create an installation token: %w", err)
	}
	if access.Token == "" || access.ExpiresAt.IsZero() {
		return cacheEntry{}, nil, errors.New("create an installation token: incomplete response")
	}

	entry = cacheEntry{
		InstallationID: installation.ID,
		Token:          access.Token,
		ExpiresAt:      access.ExpiresAt,
		ListTriedAt:    m.now(),
	}
	repositories, listErr := m.listRepositories(access.Token)
	if listErr != nil {
		return entry, listErr, nil
	}
	entry.Repositories = repositories
	entry.ListedAt = entry.ListTriedAt
	return entry, nil, nil
}

// statusError is a GitHub API response outside 2xx.
type statusError struct {
	code int
	text string
}

func (e *statusError) Error() string { return e.text }

// listRepositories lists the installation's repositories by page counting:
// it stops at a short page or once total_count is reached.
func (m *minter) listRepositories(token string) ([]string, error) {
	var repositories []string
	for page := 1; page <= 1000; page++ {
		var list struct {
			TotalCount   int `json:"total_count"`
			Repositories []struct {
				FullName string `json:"full_name"`
			} `json:"repositories"`
		}
		path := fmt.Sprintf("/installation/repositories?per_page=%d&page=%d", reposPerPage, page)
		if err := m.call("GET", path, token, &list); err != nil {
			return nil, fmt.Errorf("list installed repositories: %w", err)
		}
		for _, repo := range list.Repositories {
			repositories = append(repositories, repo.FullName)
		}
		if len(list.Repositories) < reposPerPage || len(repositories) >= list.TotalCount {
			break
		}
	}
	return repositories, nil
}

// call sends an authenticated API request and decodes the JSON response.
// Errors carry the status and GitHub's error message only, never the
// credential or the raw body.
func (m *minter) call(method, path, bearer string, out any) error {
	req, err := http.NewRequest(method, m.cfg.apiURL+path, nil)
	if err != nil {
		return fmt.Errorf("%s %s: invalid request", method, stripQuery(path))
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "gh-app")
	resp, err := m.client.Do(req)
	if err != nil {
		// Transport errors name the URL, which holds no credential.
		return fmt.Errorf("%s %s: %v", method, stripQuery(path), redact(err.Error(), bearer))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("%s %s: read response: %v", method, stripQuery(path), err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var apiErr struct {
			Message string `json:"message"`
		}
		msg := ""
		if json.Unmarshal(body, &apiErr) == nil && apiErr.Message != "" {
			msg = ": " + redact(apiErr.Message, bearer)
		}
		return &statusError{code: resp.StatusCode, text: fmt.Sprintf("%s %s: %s%s", method, stripQuery(path), resp.Status, msg)}
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%s %s: invalid JSON response", method, stripQuery(path))
	}
	return nil
}

func stripQuery(path string) string {
	if i := strings.IndexByte(path, '?'); i >= 0 {
		return path[:i]
	}
	return path
}

func redact(s, secret string) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "[redacted]")
}

func readPrivateKey(path string) (*rsa.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read GH_APP_KEY_FILE: %w", err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("GH_APP_KEY_FILE holds no PEM block")
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, errors.New("GH_APP_KEY_FILE: invalid PKCS#1 RSA private key")
		}
		return key, nil
	case "PRIVATE KEY":
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, errors.New("GH_APP_KEY_FILE: invalid PKCS#8 private key")
		}
		key, ok := parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("GH_APP_KEY_FILE: the PKCS#8 key is not an RSA key")
		}
		return key, nil
	default:
		return nil, fmt.Errorf("GH_APP_KEY_FILE: unsupported PEM block type %q", block.Type)
	}
}

// signJWT returns the App's RS256 JSON Web Token, backdated a minute against
// clock drift and valid for nine minutes after now.
func signJWT(key *rsa.PrivateKey, appID string, now time.Time) (string, error) {
	header, err := json.Marshal(struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
	}{"RS256", "JWT"})
	if err != nil {
		return "", err
	}
	claims, err := json.Marshal(struct {
		IAT int64  `json:"iat"`
		EXP int64  `json:"exp"`
		ISS string `json:"iss"`
	}{now.Unix() - 60, now.Unix() + 540, appID})
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	var signed bytes.Buffer
	signed.WriteString(enc.EncodeToString(header))
	signed.WriteByte('.')
	signed.WriteString(enc.EncodeToString(claims))
	digest := sha256.Sum256(signed.Bytes())
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", errors.New("sign the App JWT: RSA signing failed")
	}
	signed.WriteByte('.')
	signed.WriteString(enc.EncodeToString(sig))
	return signed.String(), nil
}
