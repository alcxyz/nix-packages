package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"sopsstore"
)

// Injectable for tests.
var (
	conversionBaseURL = "https://api.github.com"
	sopsCommand       = "sops"
	// GitHub's manifest flow must finish within an hour.
	registerTimeout = 55 * time.Minute
)

const defaultRegisterPort = 3999

// manifestFields are the top-level keys GitHub accepts in an App manifest.
// redirect_url is accepted but always replaced by the local callback.
var manifestFields = map[string]bool{
	"name":                     true,
	"url":                      true,
	"description":              true,
	"hook_attributes":          true,
	"redirect_url":             true,
	"public":                   true,
	"default_permissions":      true,
	"default_events":           true,
	"request_oauth_on_install": true,
	"callback_urls":            true,
	"setup_url":                true,
	"setup_on_update":          true,
}

type registerOptions struct {
	manifest string
	org      string
	port     int
	sopsFile string
	sopsKey  string
	command  []string
}

// parseRegisterArgs parses the register command's arguments. A store
// command must follow a literal --.
func parseRegisterArgs(args []string) (registerOptions, error) {
	var opts registerOptions
	fs := flag.NewFlagSet("register", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&opts.manifest, "manifest", "", "")
	fs.StringVar(&opts.org, "org", "", "")
	fs.IntVar(&opts.port, "port", defaultRegisterPort, "")
	fs.StringVar(&opts.sopsFile, "sops-file", "", "")
	fs.StringVar(&opts.sopsKey, "sops-key", "", "")
	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	rest := fs.Args()
	if len(rest) > 0 {
		separator := len(args) - len(rest) - 1
		if separator < 0 || args[separator] != "--" {
			return opts, fmt.Errorf("unexpected argument %q; give a store command after --", rest[0])
		}
		opts.command = rest
	}

	if opts.manifest == "" {
		return opts, errors.New("--manifest is required")
	}
	if opts.org != "" && !ownerPattern.MatchString(opts.org) {
		return opts, errors.New("--org is not a valid GitHub login")
	}
	if opts.port < 0 || opts.port > 65535 {
		return opts, errors.New("--port must be between 0 and 65535")
	}
	sops := opts.sopsFile != "" || opts.sopsKey != ""
	if sops && len(opts.command) > 0 {
		return opts, errors.New("give either --sops-file and --sops-key or a store command after --, not both")
	}
	if !sops && len(opts.command) == 0 {
		return opts, errors.New("give --sops-file and --sops-key, or a store command after --")
	}
	if sops {
		if opts.sopsFile == "" || opts.sopsKey == "" {
			return opts, errors.New("--sops-file and --sops-key must be given together")
		}
		if !sopsstore.KeyPattern.MatchString(opts.sopsKey) {
			return opts, errors.New("--sops-key may only contain letters, digits and underscores")
		}
		if err := sopsstore.ValidPath(opts.sopsFile); err != nil {
			return opts, fmt.Errorf("--sops-file %v", err)
		}
	}
	return opts, nil
}

// loadManifest reads and validates a manifest file. It returns the fields
// unchanged, so that GitHub sees exactly what the file says.
func loadManifest(path string) (map[string]json.RawMessage, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	var manifest map[string]json.RawMessage
	if err := dec.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("manifest: %v", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("manifest: unexpected data after the JSON object")
	}
	if manifest == nil {
		return nil, errors.New("manifest: not a JSON object")
	}
	for key := range manifest {
		if !manifestFields[key] {
			return nil, fmt.Errorf("manifest: unknown field %q", key)
		}
	}
	for _, key := range []string{"name", "url"} {
		var value string
		raw, ok := manifest[key]
		if !ok {
			return nil, fmt.Errorf("manifest: %q is required", key)
		}
		if err := json.Unmarshal(raw, &value); err != nil || value == "" {
			return nil, fmt.Errorf("manifest: %q must be a non-empty string", key)
		}
	}
	return manifest, nil
}

// keyStore is where the App's private key goes: a new SOPS file, or the
// standard input of a command.
type keyStore struct {
	sopsFile string
	sopsKey  string
	command  []string
}

func (s keyStore) sops() sopsstore.File {
	return sopsstore.File{Path: s.sopsFile, Key: s.sopsKey, Command: sopsCommand, Timeout: storeTimeout}
}

// check runs before the server starts, so that a store that cannot work
// is found before the App exists. For a command it checks that the program
// exists; for SOPS, see sopsstore.File.Check.
func (s keyStore) check(ctx context.Context) error {
	if s.sopsFile == "" {
		if _, err := exec.LookPath(s.command[0]); err != nil {
			return fmt.Errorf("store command: %v", err)
		}
		return nil
	}
	return s.sops().Check(ctx)
}

func (s keyStore) sopsDocument(key string) string {
	return s.sops().Document(key)
}

func (s keyStore) describe() string {
	if s.sopsFile != "" {
		return s.sops().Describe()
	}
	quoted := make([]string, 0, len(s.command))
	for _, arg := range s.command {
		quoted = append(quoted, strconv.Quote(arg))
	}
	return "the standard input of " + strings.Join(quoted, " ")
}

// save stores the key. Neither the key nor the store's output is ever
// reported, since a command may echo its input.
func (s keyStore) save(ctx context.Context, key string) error {
	if s.sopsFile != "" {
		return s.sops().Save(ctx, key)
	}
	ctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()
	cmd := sopsstore.BoundedCommand(ctx, s.command[0], s.command[1:]...)
	cmd.Stdin = strings.NewReader(strings.TrimRight(key, "\n") + "\n")
	return sopsstore.RunError(ctx, "the store command", storeTimeout, cmd.Run())
}

// storeTimeout bounds sops and the store command, so a store waiting on a
// prompt it cannot show does not keep gh-app running past its time limit.
var storeTimeout = sopsstore.DefaultTimeout

// createdApp is what is reported about a new App: none of its secrets.
type createdApp struct {
	id      int64
	slug    string
	name    string
	htmlURL string
}

func (a createdApp) installURL() string {
	return "https://github.com/apps/" + url.PathEscape(a.slug) + "/installations/new"
}

type registerResult struct {
	app createdApp
	err error
}

// registrar serves the manifest form and the callback.
type registrar struct {
	manifest string // JSON, with redirect_url set to the callback
	appName  string
	// pagePath is the form page's random path, printed only to the
	// terminal. Other local users can reach 127.0.0.1 too; without the path
	// they cannot read the state and so cannot send a callback.
	pagePath string
	postURL  string // GitHub's new App page, with the state
	state    string
	host     string // the Host the browser must use
	apiURL   string
	store    keyStore
	client   *http.Client
	ctx      context.Context // ends on an interrupt, stopping a running store

	mu       sync.Mutex
	finished bool
	done     chan registerResult
}

func newRegistrar(ctx context.Context, manifest map[string]json.RawMessage, org, host string, store keyStore) (*registrar, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	state := base64.RawURLEncoding.EncodeToString(buf)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	pagePath := "/" + base64.RawURLEncoding.EncodeToString(buf) + "/"

	redirect, err := json.Marshal("http://" + host + "/callback")
	if err != nil {
		return nil, err
	}
	fields := make(map[string]json.RawMessage, len(manifest)+1)
	for key, value := range manifest {
		fields[key] = value
	}
	fields["redirect_url"] = redirect
	body, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	var name string
	if err := json.Unmarshal(fields["name"], &name); err != nil {
		return nil, err
	}

	newURL := "https://github.com/settings/apps/new"
	if org != "" {
		newURL = "https://github.com/organizations/" + org + "/settings/apps/new"
	}
	return &registrar{
		manifest: string(body),
		appName:  name,
		pagePath: pagePath,
		postURL:  newURL + "?state=" + url.QueryEscape(state),
		state:    state,
		host:     host,
		apiURL:   strings.TrimRight(conversionBaseURL, "/"),
		store:    store,
		ctx:      ctx,
		client: &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		done: make(chan registerResult, 1),
	}, nil
}

func (r *registrar) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	// Refuse other Host names, so a page rebinding its DNS name to this
	// address can read neither the state nor send a callback.
	if req.Host != r.host {
		writePage(w, http.StatusBadRequest, "<p>Unexpected host.</p>")
		return
	}
	if req.URL.Path != r.pagePath && req.URL.Path != "/callback" {
		writePage(w, http.StatusNotFound, "<p>Not found.</p>")
		return
	}
	if req.Method != http.MethodGet {
		writePage(w, http.StatusMethodNotAllowed, "<p>Method not allowed.</p>")
		return
	}
	if req.URL.Path == r.pagePath {
		writePage(w, http.StatusOK, fmt.Sprintf(
			"<h2>Register %s</h2>"+
				"<p>This posts the App's manifest to GitHub. Check that you are signed in to the right account, "+
				"then choose Create GitHub App on the next page.</p>"+
				`<form method="post" action="%s">`+
				`<input type="hidden" name="manifest" value="%s">`+
				`<button type="submit">Continue to GitHub</button></form>`,
			html.EscapeString(r.appName), html.EscapeString(r.postURL), html.EscapeString(r.manifest)))
		return
	}
	r.callback(w, req)
}

func (r *registrar) callback(w http.ResponseWriter, req *http.Request) {
	query := req.URL.Query()
	code := query.Get("code")
	if subtle.ConstantTimeCompare([]byte(query.Get("state")), []byte(r.state)) != 1 || code == "" {
		writePage(w, http.StatusBadRequest, "<p>State mismatch or no code; nothing was stored.</p>")
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finished {
		writePage(w, http.StatusConflict, "<p>This registration has already finished.</p>")
		return
	}
	r.finished = true

	app, key, err := r.convert(code)
	if err != nil && app.id == 0 {
		// GitHub created the App before it redirected here.
		err = fmt.Errorf("%v; GitHub has probably created the App already: check its settings (https://github.com/settings/apps, or the organisation's), then delete it or generate its private key there", err)
	}
	if err == nil {
		if err = r.store.save(r.ctx, key); err != nil {
			err = fmt.Errorf("App %d (%s) was created, but storing its private key failed: %v; generate a new private key in the App's settings",
				app.id, app.slug, err)
		}
	}
	if err != nil {
		writePage(w, http.StatusInternalServerError, "<p>"+html.EscapeString(err.Error())+"</p>")
		r.done <- registerResult{err: err}
		return
	}
	writePage(w, http.StatusOK, fmt.Sprintf(
		"<h2>Created %s (App ID %d)</h2>"+
			"<p>The private key is stored. Next, install the App on the accounts and repositories it should act on.</p>"+
			`<p><a href="%s">Install %s</a></p>`,
		html.EscapeString(app.name), app.id, html.EscapeString(app.installURL()), html.EscapeString(app.name)))
	r.done <- registerResult{app: app}
}

// convert exchanges the one-time code for the App's credentials and returns
// the private key; the client secret and webhook secret are dropped.
func (r *registrar) convert(code string) (createdApp, string, error) {
	endpoint := r.apiURL + "/app-manifests/" + url.PathEscape(code) + "/conversions"
	req, err := http.NewRequestWithContext(r.ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return createdApp{}, "", errors.New("conversion request could not be built")
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := r.client.Do(req)
	if err != nil {
		// A *url.Error names the URL, which contains the code.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return createdApp{}, "", fmt.Errorf("conversion request failed: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return createdApp{}, "", fmt.Errorf("conversion failed: HTTP %d, reading the response: %v", resp.StatusCode, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var apiErr struct {
			Message string `json:"message"`
		}
		json.Unmarshal(body, &apiErr)
		message := printable(apiErr.Message)
		if message == "" {
			return createdApp{}, "", fmt.Errorf("conversion failed: HTTP %d", resp.StatusCode)
		}
		return createdApp{}, "", fmt.Errorf("conversion failed: HTTP %d: %s", resp.StatusCode, message)
	}
	var app struct {
		ID      int64  `json:"id"`
		Slug    string `json:"slug"`
		Name    string `json:"name"`
		HTMLURL string `json:"html_url"`
		PEM     string `json:"pem"`
	}
	if err := json.Unmarshal(body, &app); err != nil {
		return createdApp{}, "", errors.New("conversion failed: the response is not valid JSON")
	}
	created := createdApp{id: app.ID, slug: printable(app.Slug), name: printable(app.Name), htmlURL: printable(app.HTMLURL)}
	if app.ID <= 0 || created.slug == "" {
		return createdApp{}, "", errors.New("conversion failed: the response has no App ID or slug")
	}
	if created.name == "" {
		created.name = created.slug
	}
	if !strings.Contains(app.PEM, "PRIVATE KEY-----") {
		return created, "", fmt.Errorf("App %d (%s) was created, but the response has no private key; generate one in the App's settings",
			created.id, created.slug)
	}
	return created, app.PEM, nil
}

// printable keeps a server-supplied string to one short printable line.
func printable(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsPrint(r) {
			b.WriteRune(r)
		}
		if b.Len() >= 200 {
			break
		}
	}
	return b.String()
}

func writePage(w http.ResponseWriter, status int, body string) {
	page := "<!doctype html><meta charset=utf-8><title>gh-app register</title>" +
		"<body style='font-family:sans-serif;max-width:40em;margin:3em auto'>" + body
	header := w.Header()
	header.Set("Content-Type", "text/html; charset=utf-8")
	header.Set("Cache-Control", "no-store")
	// The callback URL carries the one-time code; keep it out of Referer.
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("Content-Length", strconv.Itoa(len(page)))
	w.WriteHeader(status)
	io.WriteString(w, page)
}

func runRegister(args []string, stdout, stderr io.Writer) int {
	opts, err := parseRegisterArgs(args)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprint(stdout, usage)
		return 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "gh-app register: %v\n", err)
		return 2
	}
	app, err := register(opts, stdout)
	if err != nil {
		fmt.Fprintf(stderr, "gh-app register: %v\n", err)
		return 1
	}
	store := keyStore{sopsFile: opts.sopsFile, sopsKey: opts.sopsKey, command: opts.command}
	fmt.Fprintf(stdout, "app id:   %d\n", app.id)
	fmt.Fprintf(stdout, "slug:     %s\n", app.slug)
	fmt.Fprintf(stdout, "url:      %s\n", app.htmlURL)
	fmt.Fprintf(stdout, "install:  %s\n", app.installURL())
	fmt.Fprintf(stdout, "key:      stored in %s\n", store.describe())
	fmt.Fprintln(stdout, "The client secret and webhook secret were discarded; regenerate them in the App's settings if needed.")
	return 0
}

// register serves the manifest flow on 127.0.0.1 until one callback has
// been handled or the time runs out.
func register(opts registerOptions, stdout io.Writer) (createdApp, error) {
	manifest, err := loadManifest(opts.manifest)
	if err != nil {
		return createdApp{}, err
	}
	// The store runs in its own process group, which the terminal's Ctrl-C
	// does not reach: an interrupt ends ctx, which kills it.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	store := keyStore{sopsFile: opts.sopsFile, sopsKey: opts.sopsKey, command: opts.command}
	if err := store.check(ctx); err != nil {
		return createdApp{}, err
	}

	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(opts.port)))
	if err != nil {
		return createdApp{}, err
	}
	host := net.JoinHostPort("127.0.0.1", strconv.Itoa(ln.Addr().(*net.TCPAddr).Port))
	r, err := newRegistrar(ctx, manifest, opts.org, host, store)
	if err != nil {
		ln.Close()
		return createdApp{}, err
	}
	srv := &http.Server{Handler: r, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	fmt.Fprintf(stdout, "open http://%s%s in a browser signed in to GitHub\n", host, r.pagePath)

	var result registerResult
	timer := time.NewTimer(registerTimeout)
	select {
	case result = <-r.done:
	case <-timer.C:
		// Let a callback already in progress finish, and refuse later ones.
		r.mu.Lock()
		r.finished = true
		r.mu.Unlock()
		select {
		case result = <-r.done:
		default:
			result.err = errors.New("timed out waiting for GitHub's callback; nothing was stored. If GitHub's page is still open, do not create the App there: its callback could reach another listener on this port. Delete any App created after this")
		}
	case <-ctx.Done():
		// A second interrupt now ends gh-app at once. A callback in progress
		// sees ctx end too: its conversion or store stops and it reports,
		// which the lock waits for.
		stop()
		r.mu.Lock()
		r.finished = true
		r.mu.Unlock()
		select {
		case result = <-r.done:
		default:
			result.err = errors.New("interrupted before GitHub's callback; nothing was stored. If GitHub's page is still open, do not create the App there: its callback could reach another listener on this port. Delete any App created after this")
		}
	}
	timer.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	srv.Shutdown(ctx)
	cancel()
	return result.app, result.err
}
