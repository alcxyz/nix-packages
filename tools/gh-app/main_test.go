package main

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testOwner = "example-owner"
	testAppID = "12345"
	testToken = "ghs_testinstallationtoken0123456789"
)

func TestMain(m *testing.M) {
	// The test binary doubles as gh-app for end-to-end runs.
	if os.Getenv("GH_APP_TEST_MAIN") == "1" {
		main()
		os.Exit(0)
	}
	// A fake ssh keeps SSH alias resolution independent of the host.
	dir, err := os.MkdirTemp("", "gh-app-ssh-")
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(fakeSSH), 0o755); err != nil {
		panic(err)
	}
	os.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

const fakeSSH = `#!/bin/sh
[ "$1" = -G ] || exit 2
case "$2" in
  gh-alias) printf 'user git\nhostname github.com\n' ;;
  gh-443) printf 'hostname ssh.github.com\nport 443\n' ;;
  GH-Mixed) printf 'hostname GitHub.COM\n' ;;
  broken) exit 255 ;;
  *) printf 'hostname %s\n' "$2" ;;
esac
`

var (
	testKeyOnce sync.Once
	testKey     *rsa.PrivateKey
)

func rsaKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	testKeyOnce.Do(func() {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		testKey = key
	})
	return testKey
}

func writeKey(t *testing.T, pkcs8 bool) string {
	t.Helper()
	key := rsaKey(t)
	var block *pem.Block
	if pkcs8 {
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		block = &pem.Block{Type: "PRIVATE KEY", Bytes: der}
	} else {
		block = &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}
	}
	path := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// verifyJWT checks the signature with the public key and returns the claims.
func verifyJWT(t *testing.T, pub *rsa.PublicKey, token string) map[string]any {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT has %d parts", len(parts))
	}
	for _, part := range parts {
		if strings.ContainsAny(part, "=+/") {
			t.Fatalf("JWT part %q is not unpadded base64url", part)
		}
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig); err != nil {
		t.Fatalf("JWT signature: %v", err)
	}
	var header map[string]any
	decodeJWTPart(t, parts[0], &header)
	if header["alg"] != "RS256" || header["typ"] != "JWT" {
		t.Fatalf("JWT header = %v", header)
	}
	var claims map[string]any
	decodeJWTPart(t, parts[1], &claims)
	return claims
}

func decodeJWTPart(t *testing.T, part string, out any) {
	t.Helper()
	data, err := base64.RawURLEncoding.DecodeString(part)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		t.Fatal(err)
	}
}

func TestSignJWT(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	token, err := signJWT(rsaKey(t), testAppID, now)
	if err != nil {
		t.Fatal(err)
	}
	claims := verifyJWT(t, &rsaKey(t).PublicKey, token)
	if claims["iat"] != float64(now.Unix()-60) || claims["exp"] != float64(now.Unix()+540) || claims["iss"] != testAppID {
		t.Fatalf("claims = %v", claims)
	}
}

func TestReadPrivateKey(t *testing.T) {
	for _, pkcs8 := range []bool{false, true} {
		key, err := readPrivateKey(writeKey(t, pkcs8))
		if err != nil {
			t.Fatalf("pkcs8=%v: %v", pkcs8, err)
		}
		if !key.Equal(rsaKey(t)) {
			t.Fatalf("pkcs8=%v: parsed a different key", pkcs8)
		}
	}

	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(ecKey)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ec.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivateKey(path); err == nil || !strings.Contains(err.Error(), "not an RSA key") {
		t.Fatalf("EC key error = %v", err)
	}
}

// fakeGitHub emulates the three API endpoints gh-app uses.
type fakeGitHub struct {
	t     *testing.T
	pub   *rsa.PublicKey
	repos []string
	token string

	mu            sync.Mutex
	installCalls  int
	tokenCalls    int
	repoCalls     int
	failRepos     bool
	failInstall   bool
	orgOnly       bool
	orgCalls      int
	expiresIn     time.Duration
	headerProblem string
	// requested holds the permissions of the last token request, nil
	// for a full token; grant, when set, replaces what a restricted
	// request is granted.
	requested map[string]string
	grant     map[string]string
}

func newFakeGitHub(t *testing.T, repos []string) (*fakeGitHub, *httptest.Server) {
	f := &fakeGitHub{t: t, pub: &rsaKey(t).PublicKey, repos: repos, token: testToken, expiresIn: time.Hour}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeGitHub) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.installCalls + f.tokenCalls + f.repoCalls
}

func (f *fakeGitHub) problem(format string, args ...any) {
	if f.headerProblem == "" {
		f.headerProblem = fmt.Sprintf(format, args...)
	}
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Accept") != "application/vnd.github+json" {
		f.problem("Accept = %q", r.Header.Get("Accept"))
	}
	if r.Header.Get("X-GitHub-Api-Version") != "2022-11-28" {
		f.problem("X-GitHub-Api-Version = %q", r.Header.Get("X-GitHub-Api-Version"))
	}
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	w.Header().Set("Content-Type", "application/json")

	switch {
	case r.Method == "GET" && r.URL.Path == "/users/"+testOwner+"/installation":
		f.installCalls++
		f.checkJWT(bearer)
		if f.failInstall || f.orgOnly {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found","documentation_url":"https://docs.github.com/rest"}`)
			return
		}
		fmt.Fprint(w, `{"id":777,"account":{"login":"`+testOwner+`"}}`)
	case r.Method == "GET" && r.URL.Path == "/orgs/"+testOwner+"/installation":
		f.orgCalls++
		f.checkJWT(bearer)
		if !f.orgOnly {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
			return
		}
		fmt.Fprint(w, `{"id":777,"account":{"login":"`+testOwner+`"}}`)
	case r.Method == "POST" && r.URL.Path == "/app/installations/777/access_tokens":
		f.tokenCalls++
		f.checkJWT(bearer)
		var request struct {
			Permissions map[string]string `json:"permissions"`
		}
		if r.ContentLength > 0 {
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				f.problem("token request body: %v", err)
			}
		}
		f.requested = request.Permissions
		expires := time.Now().Add(f.expiresIn).UTC().Format(time.RFC3339)
		if request.Permissions == nil {
			fmt.Fprintf(w, `{"token":%q,"expires_at":%q,"permissions":{"issues":"write","metadata":"read"}}`, f.token, expires)
			return
		}
		granted := request.Permissions
		if f.grant != nil {
			granted = f.grant
		}
		data, _ := json.Marshal(granted)
		fmt.Fprintf(w, `{"token":%q,"expires_at":%q,"permissions":%s}`, f.token+"read", expires, data)
	case r.Method == "GET" && r.URL.Path == "/installation/repositories":
		f.repoCalls++
		if bearer != f.token {
			f.problem("repositories bearer is not the full installation token")
		}
		if f.failRepos {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintf(w, `{"message":"backend trouble for %s"}`, bearer)
			return
		}
		if r.URL.Query().Get("per_page") != "100" {
			f.problem("per_page = %q", r.URL.Query().Get("per_page"))
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page < 1 {
			page = 1
		}
		start := (page - 1) * 100
		end := start + 100
		if start > len(f.repos) {
			start = len(f.repos)
		}
		if end > len(f.repos) {
			end = len(f.repos)
		}
		var items []string
		for _, name := range f.repos[start:end] {
			items = append(items, fmt.Sprintf(`{"full_name":%q}`, name))
		}
		fmt.Fprintf(w, `{"total_count":%d,"repositories":[%s]}`, len(f.repos), strings.Join(items, ","))
	default:
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"Not Found"}`)
	}
}

func (f *fakeGitHub) checkJWT(token string) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		f.problem("bearer is not a JWT")
		return
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		f.problem("JWT signature encoding: %v", err)
		return
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(f.pub, crypto.SHA256, digest[:], sig); err != nil {
		f.problem("JWT signature: %v", err)
	}
}

func (f *fakeGitHub) assertHeaders(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.headerProblem != "" {
		t.Fatal(f.headerProblem)
	}
}

// manyRepos returns more repositories than one page holds, including the
// ones the routing tests use.
func manyRepos() []string {
	repos := []string{testOwner + "/Installed"}
	for i := 0; i < 140; i++ {
		repos = append(repos, fmt.Sprintf("%s/repo-%03d", testOwner, i))
	}
	return append(repos, testOwner+"/last-page")
}

func testConfig(t *testing.T, apiURL string) config {
	t.Helper()
	return config{
		appID:    testAppID,
		keyFile:  writeKey(t, false),
		owner:    testOwner,
		ghPath:   "/nonexistent/gh",
		apiURL:   apiURL,
		cacheDir: filepath.Join(t.TempDir(), "cache"),
	}
}

func TestTokenMintAndCache(t *testing.T) {
	fake, srv := newFakeGitHub(t, manyRepos())
	cfg := testConfig(t, srv.URL)
	m := newMinter(cfg)

	entry, err := m.token()
	if err != nil {
		t.Fatal(err)
	}
	fake.assertHeaders(t)
	if entry.Token != testToken || entry.InstallationID != 777 {
		t.Fatalf("entry = %+v", entry)
	}
	if len(entry.Repositories) != 142 || !entry.installed(testOwner, "last-page") || !entry.installed(strings.ToUpper(testOwner), "installed") {
		t.Fatalf("repositories not fully paginated or matched: %d", len(entry.Repositories))
	}
	if entry.installed(testOwner, "absent") {
		t.Fatal("absent repository reported as installed")
	}
	if fake.installCalls != 1 || fake.tokenCalls != 1 || fake.repoCalls != 2 {
		t.Fatalf("calls: install=%d token=%d repos=%d", fake.installCalls, fake.tokenCalls, fake.repoCalls)
	}

	info, err := os.Stat(cfg.cacheDir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("cache dir mode = %v, %v", info.Mode().Perm(), err)
	}
	files, err := os.ReadDir(cfg.cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("cache dir holds %d files, want cache and lock", len(files))
	}
	for _, file := range files {
		info, err := file.Info()
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %v, %v", file.Name(), info.Mode().Perm(), err)
		}
	}

	// A second call is served from the cache, also by a new process.
	before := fake.calls()
	if _, err := newMinter(cfg).token(); err != nil {
		t.Fatal(err)
	}
	if fake.calls() != before {
		t.Fatal("cached token was not reused")
	}

	// Less than ten minutes left: refresh.
	m.now = func() time.Time { return time.Now().Add(51 * time.Minute) }
	if _, err := m.token(); err != nil {
		t.Fatal(err)
	}
	if fake.tokenCalls != 2 {
		t.Fatalf("near-expiry token was not refreshed (token calls %d)", fake.tokenCalls)
	}
}

func TestTokenConcurrentMintsOnce(t *testing.T) {
	fake, srv := newFakeGitHub(t, []string{testOwner + "/one"})
	cfg := testConfig(t, srv.URL)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := newMinter(cfg).token()
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if fake.tokenCalls != 1 {
		t.Fatalf("minted %d tokens, want 1", fake.tokenCalls)
	}
}

func TestTokenErrors(t *testing.T) {
	fake, srv := newFakeGitHub(t, nil)
	fake.failInstall = true
	_, err := newMinter(testConfig(t, srv.URL)).token()
	if err == nil || !strings.Contains(err.Error(), "404 Not Found: Not Found") || strings.Contains(err.Error(), "documentation_url") {
		t.Fatalf("error = %v", err)
	}

	fake.failInstall = false
	fake.failRepos = true
	_, err = newMinter(testConfig(t, srv.URL)).token()
	if err == nil || strings.Contains(err.Error(), testToken) || !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("error = %v", err)
	}

	cfg := testConfig(t, srv.URL)
	cfg.cacheDir = ""
	if _, err := newMinter(cfg).token(); err == nil || !strings.Contains(err.Error(), "XDG_RUNTIME_DIR") {
		t.Fatalf("missing cache dir error = %v", err)
	}
	if got := readConfig(func(string) string { return "" }); got.cacheDir != "" {
		t.Fatalf("cache dir fell back to %q", got.cacheDir)
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// isolateGit keeps the operator's Git configuration out of the tests.
func isolateGit(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	for _, key := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_CONFIG", "GIT_CONFIG_COUNT"} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
}

func gitRepo(t *testing.T, remotes [][2]string, resolved map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "--quiet")
	for _, remote := range remotes {
		git(t, dir, "remote", "add", remote[0], remote[1])
	}
	for name, value := range resolved {
		git(t, dir, "config", "remote."+name+".gh-resolved", value)
	}
	return dir
}

func TestRemoteTarget(t *testing.T) {
	isolateGit(t)
	tests := []struct {
		name     string
		remotes  [][2]string
		resolved map[string]string
		want     string
	}{
		{"scp form", [][2]string{{"origin", "git@github.com:o/r.git"}}, nil, "o/r"},
		{"ssh form", [][2]string{{"origin", "ssh://git@github.com/o/r.git"}}, nil, "o/r"},
		{"https form", [][2]string{{"origin", "https://github.com/o/r"}}, nil, "o/r"},
		{"non-github only", [][2]string{{"origin", "https://forge.example.test/o/r.git"}}, nil, ""},
		{"name priority upstream", [][2]string{
			{"origin", "git@github.com:me/fork.git"},
			{"upstream", "git@github.com:o/r.git"},
			{"github", "git@github.com:g/h.git"},
		}, nil, "o/r"},
		{"name priority github over origin", [][2]string{
			{"origin", "git@github.com:me/fork.git"},
			{"github", "git@github.com:g/h.git"},
		}, nil, "g/h"},
		{"gh-resolved base wins", [][2]string{
			{"upstream", "git@github.com:o/r.git"},
			{"mine", "git@github.com:me/fork.git"},
		}, map[string]string{"mine": "base"}, "me/fork"},
		{"gh-resolved explicit repository", [][2]string{
			{"upstream", "git@github.com:o/r.git"},
			{"origin", "git@github.com:me/fork.git"},
		}, map[string]string{"origin": "chosen/repo"}, "chosen/repo"},
		{"gh-resolved explicit with host", [][2]string{
			{"origin", "git@github.com:me/fork.git"},
		}, map[string]string{"origin": "github.com/chosen/repo"}, "chosen/repo"},
		{"first github remote in git remote order", [][2]string{
			{"forge", "https://forge.example.test/o/r.git"},
			{"zeta", "https://github.com/z/one.git"},
			{"alpha", "https://github.com/a/two.git"},
		}, nil, "a/two"},
		{"github origin with forge first", [][2]string{
			{"forge", "https://forge.example.test/o/r.git"},
			{"origin", "https://github.com/o/r.git"},
		}, nil, "o/r"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := gitRepo(t, tt.remotes, tt.resolved)
			ref, ok := resolveTarget([]string{"pr", "list"}, func(string) string { return "" }, dir)
			got := ""
			if ok {
				got = ref.Owner + "/" + ref.Name
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}

	if _, ok := remoteTarget(t.TempDir()); ok {
		t.Fatal("a directory outside Git has a target")
	}
}

func TestPlanGH(t *testing.T) {
	isolateGit(t)
	fake, srv := newFakeGitHub(t, manyRepos())
	nonRepo := t.TempDir()
	baseEnv := []string{"PATH=/bin", "GITHUB_TOKEN=operator", "GH_TOKEN=operator", "GH_ENTERPRISE_TOKEN=x", "GITHUB_ENTERPRISE_TOKEN=y", "KEEP=1"}

	t.Run("installed repository gets the token", func(t *testing.T) {
		cfg := testConfig(t, srv.URL)
		p, err := planGH(cfg, []string{"pr", "list", "-R", testOwner + "/installed"}, baseEnv, nonRepo, newMinter(cfg))
		if err != nil {
			t.Fatal(err)
		}
		if !p.withToken || p.path != cfg.ghPath || strings.Join(p.argv, " ") != cfg.ghPath+" pr list -R "+testOwner+"/installed" {
			t.Fatalf("plan = %+v", p)
		}
		want := "PATH=/bin KEEP=1 GH_TOKEN=" + testToken
		if strings.Join(p.env, " ") != want {
			t.Fatalf("env = %v", p.env)
		}
	})

	t.Run("installed repository from remotes", func(t *testing.T) {
		cfg := testConfig(t, srv.URL)
		dir := gitRepo(t, [][2]string{{"origin", "git@github.com:" + testOwner + "/repo-007.git"}}, nil)
		p, err := planGH(cfg, []string{"pr", "list"}, baseEnv, dir, newMinter(cfg))
		if err != nil || !p.withToken {
			t.Fatalf("plan = %+v, %v", p, err)
		}
	})

	t.Run("not installed keeps the operator", func(t *testing.T) {
		cfg := testConfig(t, srv.URL)
		p, err := planGH(cfg, []string{"pr", "list", "-R", testOwner + "/absent"}, baseEnv, nonRepo, newMinter(cfg))
		if err != nil || p.withToken || strings.Join(p.env, " ") != strings.Join(baseEnv, " ") {
			t.Fatalf("plan = %+v, %v", p, err)
		}
	})

	t.Run("other owner and no target never mint", func(t *testing.T) {
		cfg := testConfig(t, srv.URL)
		before := fake.calls()
		for _, args := range [][]string{{"pr", "list", "-R", "someone/else"}, {"auth", "status"}, {"api", "-R", "ghe.example.test/" + testOwner + "/installed"}} {
			p, err := planGH(cfg, args, baseEnv, nonRepo, newMinter(cfg))
			if err != nil || p.withToken {
				t.Fatalf("%v: plan = %+v, %v", args, p, err)
			}
		}
		if fake.calls() != before {
			t.Fatal("routing to another owner touched the API")
		}
		if _, err := os.Stat(cfg.cacheDir); !os.IsNotExist(err) {
			t.Fatal("routing to another owner touched the cache")
		}
	})

	t.Run("operator override", func(t *testing.T) {
		cfg := testConfig(t, srv.URL)
		env := append([]string{"GH_AS_OPERATOR=1"}, baseEnv...)
		before := fake.calls()
		p, err := planGH(cfg, []string{"pr", "list", "-R", testOwner + "/installed"}, env, nonRepo, newMinter(cfg))
		if err != nil || p.withToken || fake.calls() != before {
			t.Fatalf("plan = %+v, %v", p, err)
		}
	})

	t.Run("mint failure is loud", func(t *testing.T) {
		cfg := testConfig(t, srv.URL)
		cfg.keyFile = filepath.Join(t.TempDir(), "missing.pem")
		_, err := planGH(cfg, []string{"pr", "list", "-R", testOwner + "/installed"}, baseEnv, nonRepo, newMinter(cfg))
		if err == nil || !strings.Contains(err.Error(), "GH_AS_OPERATOR=1") || !strings.Contains(err.Error(), "GH_APP_KEY_FILE") {
			t.Fatalf("error = %v", err)
		}
	})
}

// runBinary runs the test binary as gh-app with the given environment.
func runBinary(t *testing.T, dir string, env []string, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Dir = dir
	cmd.Env = append([]string{"GH_APP_TEST_MAIN=1", "PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull}, env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return code, stdout.String(), stderr.String()
}

func TestEndToEnd(t *testing.T) {
	isolateGit(t)
	fake, srv := newFakeGitHub(t, manyRepos())
	work := t.TempDir()
	record := filepath.Join(work, "record")
	fakeGH := filepath.Join(work, "gh")
	script := "#!/bin/sh\n{ printf 'args:%s\\n' \"$*\"; printf 'GH_TOKEN=%s\\n' \"${GH_TOKEN-unset}\"; printf 'GITHUB_TOKEN=%s\\n' \"${GITHUB_TOKEN-unset}\"; } > \"$RECORD\"\nexit 7\n"
	if err := os.WriteFile(fakeGH, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t, srv.URL)
	env := []string{
		"RECORD=" + record,
		"GH_APP_ID=" + cfg.appID,
		"GH_APP_KEY_FILE=" + cfg.keyFile,
		"GH_APP_OWNER=" + cfg.owner,
		"GH_APP_GH=" + fakeGH,
		"GH_APP_API_URL=" + srv.URL,
		"GH_APP_CACHE_DIR=" + cfg.cacheDir,
		"GITHUB_TOKEN=operator",
	}
	readRecord := func() string {
		data, err := os.ReadFile(record)
		if err != nil {
			t.Fatal(err)
		}
		os.Remove(record)
		return string(data)
	}

	tests := []struct {
		name  string
		extra []string
		args  []string
		want  string
	}{
		{"installed", nil, []string{"pr", "view", "https://github.com/" + testOwner + "/installed/pull/3"},
			"args:pr view https://github.com/" + testOwner + "/installed/pull/3\nGH_TOKEN=" + testToken + "\nGITHUB_TOKEN=unset\n"},
		{"operator", []string{"GH_AS_OPERATOR=1"}, []string{"pr", "list", "-R", testOwner + "/installed"},
			"args:pr list -R " + testOwner + "/installed\nGH_TOKEN=unset\nGITHUB_TOKEN=operator\n"},
		{"other owner", nil, []string{"pr", "list", "-R", "someone/else"},
			"args:pr list -R someone/else\nGH_TOKEN=unset\nGITHUB_TOKEN=operator\n"},
		{"not installed", nil, []string{"pr", "list", "-R", testOwner + "/absent"},
			"args:pr list -R " + testOwner + "/absent\nGH_TOKEN=unset\nGITHUB_TOKEN=operator\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, stderr := runBinary(t, work, append(env, tt.extra...), append([]string{"gh"}, tt.args...)...)
			if code != 7 || stdout != "" || stderr != "" {
				t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
			}
			if got := readRecord(); got != tt.want {
				t.Fatalf("record = %q, want %q", got, tt.want)
			}
		})
	}

	t.Run("status", func(t *testing.T) {
		code, stdout, stderr := runBinary(t, work, env, "status")
		if code != 0 || stderr != "" || strings.Contains(stdout, testToken) || !strings.Contains(stdout, "installation id: 777") || !strings.Contains(stdout, testOwner+"/last-page") {
			t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
		}
	})

	t.Run("credential", func(t *testing.T) {
		cmd := exec.Command(os.Args[0], "credential", "get")
		cmd.Env = append([]string{"GH_APP_TEST_MAIN=1"}, env...)
		cmd.Stdin = strings.NewReader("protocol=https\nhost=github.com\npath=" + testOwner + "/installed.git\n\n")
		out, err := cmd.Output()
		if err != nil || !strings.HasPrefix(string(out), "username=x-access-token\npassword="+testToken+"\npassword_expiry_utc=") {
			t.Fatalf("output %q, %v", out, err)
		}
	})

	t.Run("unconfigured passes through", func(t *testing.T) {
		passEnv := []string{"RECORD=" + record, "GH_APP_GH=" + fakeGH, "GITHUB_TOKEN=operator"}
		code, stdout, stderr := runBinary(t, work, passEnv, "gh", "pr", "list", "-R", testOwner+"/installed")
		if code != 7 || stdout != "" || stderr != "" {
			t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
		}
		if got := readRecord(); got != "args:pr list -R "+testOwner+"/installed\nGH_TOKEN=unset\nGITHUB_TOKEN=operator\n" {
			t.Fatalf("record = %q", got)
		}
		code, stdout, stderr = runBinary(t, work, append(passEnv, "GH_APP_OWNER="+testOwner), "gh", "auth", "status")
		if code != 1 || stdout != "" || strings.Count(stderr, "\n") != 1 || !strings.Contains(stderr, "set together") {
			t.Fatalf("partial: exit %d, stdout %q, stderr %q", code, stdout, stderr)
		}
	})

	t.Run("mint failure never prints the token", func(t *testing.T) {
		fake.mu.Lock()
		fake.failRepos = true
		fake.mu.Unlock()
		failEnv := append([]string{}, env...)
		failEnv = append(failEnv, "GH_APP_CACHE_DIR="+filepath.Join(t.TempDir(), "fresh"))
		code, stdout, stderr := runBinary(t, work, failEnv, "gh", "pr", "list", "-R", testOwner+"/installed")
		if code != 1 || stdout != "" || strings.Count(stderr, "\n") != 1 || !strings.Contains(stderr, "GH_AS_OPERATOR=1") || strings.Contains(stderr, testToken) {
			t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
		}
		if _, err := os.Stat(record); !os.IsNotExist(err) {
			t.Fatal("gh ran despite the mint failure")
		}
		code, stdout, stderr = runBinary(t, work, failEnv, "status")
		if code == 0 || stdout != "" || strings.Contains(stderr, testToken) {
			t.Fatalf("status: exit %d, stdout %q, stderr %q", code, stdout, stderr)
		}
	})

	t.Run("missing runtime dir", func(t *testing.T) {
		var noCache []string
		for _, kv := range env {
			if !strings.HasPrefix(kv, "GH_APP_CACHE_DIR=") {
				noCache = append(noCache, kv)
			}
		}
		code, _, stderr := runBinary(t, work, noCache, "gh", "pr", "list", "-R", testOwner+"/installed")
		if code != 1 || !strings.Contains(stderr, "XDG_RUNTIME_DIR") {
			t.Fatalf("exit %d, stderr %q", code, stderr)
		}
	})
}
