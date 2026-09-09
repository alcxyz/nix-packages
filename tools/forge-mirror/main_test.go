package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func unsetEnv(t *testing.T, key string) {
	t.Helper()
	value, present := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if present {
			_ = os.Setenv(key, value)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}

// Never let these tests use the operator's policy or credential files.
func isolatedMirrorConfig(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("FORGE_MIRROR_GITHUB_PRIMARY_REPOS", "")
	t.Setenv("FORGE_MIRROR_GITHUB_PRIMARY_REPOS_FILE", "")
	t.Setenv("FORGE_MIRROR_CODEBERG_REPOS", "")
	t.Setenv("FORGE_MIRROR_CODEBERG_REPOS_FILE", "")
	t.Setenv("FORGE_MIRROR_GITHUB_DENIED_REPOS", "")
	t.Setenv("FORGE_MIRROR_GITHUB_DENIED_REPOS_FILE", "")
	t.Setenv("FORGE_MIRROR_REQUIRED_PRIVATE_REPOS", "")
	t.Setenv("FORGE_MIRROR_REQUIRED_PRIVATE_REPOS_FILE", "")
	t.Setenv("FORGE_MIRROR_SCAN_ROOTS", "")
	t.Setenv("FORGE_MIRROR_SCAN_ROOTS_FILE", "")
	t.Setenv("GITHUB_MIRROR_PAT", "test-github-token")
	t.Setenv("GITHUB_MIRROR_PAT_FILE", "")
	t.Setenv("GITHUB_USER", "test-user")
	t.Setenv("FORGEJO_TOKEN", "test-token")
	t.Setenv("FORGEJO_TOKEN_FILE", "")
	t.Setenv("CODEBERG_MIRROR_PAT", "test-codeberg-token")
	t.Setenv("CODEBERG_MIRROR_PAT_FILE", "")
	t.Setenv("CODEBERG_URL", "https://codeberg.org")
	t.Setenv("CODEBERG_USER", "test-user")
	previousClient := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() != "127.0.0.1" {
			return nil, fmt.Errorf("test blocked non-local HTTP request")
		}
		return http.DefaultTransport.RoundTrip(r)
	})}
	t.Cleanup(func() { http.DefaultClient = previousClient })
}

func TestConfiguredRepoNamesSupportsExplicitAndAdditiveInputs(t *testing.T) {
	const valueKey = "FORGE_MIRROR_TEST_REPOS"
	const fileKey = "FORGE_MIRROR_TEST_REPOS_FILE"

	t.Run("additive", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "repos")
		if err := os.WriteFile(path, []byte("repo-b\nRepo-C\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv(valueKey, "Repo-A, repo-b")
		t.Setenv(fileKey, path)
		repos, err := configuredRepoNames("test", valueKey, fileKey, "")
		if err != nil || len(repos) != 3 || !repos["repo-a"] || !repos["repo-b"] || !repos["repo-c"] {
			t.Fatalf("repos=%v err=%v", repos, err)
		}
	})

	for _, variant := range []string{"empty-value", "empty-file-path", "empty-file"} {
		t.Run(variant, func(t *testing.T) {
			unsetEnv(t, valueKey)
			unsetEnv(t, fileKey)
			switch variant {
			case "empty-value":
				t.Setenv(valueKey, "")
			case "empty-file-path":
				t.Setenv(fileKey, "")
			case "empty-file":
				path := filepath.Join(t.TempDir(), "repos")
				if err := os.WriteFile(path, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				t.Setenv(fileKey, path)
			}
			repos, err := configuredRepoNames("test", valueKey, fileKey, "")
			if err != nil || len(repos) != 0 {
				t.Fatalf("repos=%v err=%v", repos, err)
			}
		})
	}

	t.Run("missing", func(t *testing.T) {
		unsetEnv(t, valueKey)
		unsetEnv(t, fileKey)
		if _, err := configuredRepoNames("test", valueKey, fileKey, ""); err == nil {
			t.Fatal("expected missing configuration error")
		}
	})

	t.Run("unreadable", func(t *testing.T) {
		unsetEnv(t, valueKey)
		t.Setenv(fileKey, t.TempDir())
		if _, err := configuredRepoNames("test", valueKey, fileKey, ""); err == nil {
			t.Fatal("expected unreadable configuration error")
		}
	})
}

func TestConfiguredScanPaths(t *testing.T) {
	isolatedMirrorConfig(t)
	filePath := filepath.Join(t.TempDir(), "roots")
	if err := os.WriteFile(filePath, []byte("/from/file\n/path with spaces\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FORGE_MIRROR_SCAN_ROOTS", strings.Join([]string{"/from/env", "/another env"}, string(os.PathListSeparator)))
	t.Setenv("FORGE_MIRROR_SCAN_ROOTS_FILE", filePath)
	paths, err := configuredScanPaths()
	want := []string{"/from/env", "/another env", "/from/file", "/path with spaces"}
	if err != nil || fmt.Sprint(paths) != fmt.Sprint(want) {
		t.Fatalf("paths=%v err=%v", paths, err)
	}

	unsetEnv(t, "FORGE_MIRROR_SCAN_ROOTS")
	unsetEnv(t, "FORGE_MIRROR_SCAN_ROOTS_FILE")
	if _, err := configuredScanPaths(); err == nil {
		t.Fatal("expected missing scan-root configuration error")
	}
	t.Setenv("FORGE_MIRROR_SCAN_ROOTS", "")
	paths, err = configuredScanPaths()
	if err != nil || len(paths) != 0 {
		t.Fatalf("explicit empty roots: paths=%v err=%v", paths, err)
	}
}

func TestCommandEnvironmentRequiresOnlyConsumedScalars(t *testing.T) {
	values := map[string]string{
		"FORGEJO_URL":      "https://forge.test",
		"FORGEJO_USER":     "forge-user",
		"FORGEJO_SSH_HOST": "ssh.forge.test",
		"GITHUB_USER":      "github-user",
		"CODEBERG_USER":    "codeberg-user",
	}
	for command, requiredKeys := range commandEnvironmentKeys {
		t.Run(command, func(t *testing.T) {
			for key := range values {
				unsetEnv(t, key)
			}
			for _, key := range requiredKeys {
				t.Setenv(key, values[key])
			}
			configured, err := configuredCommandEnvironment(command)
			if err != nil || len(configured) != len(requiredKeys) {
				t.Fatalf("configured=%v err=%v", configured, err)
			}
			for _, missingKey := range requiredKeys {
				t.Run("missing-"+missingKey, func(t *testing.T) {
					for _, key := range requiredKeys {
						t.Setenv(key, values[key])
					}
					unsetEnv(t, missingKey)
					if _, err := configuredCommandEnvironment(command); err == nil || !strings.Contains(err.Error(), missingKey) {
						t.Fatalf("expected missing %s, got %v", missingKey, err)
					}
				})
			}
		})
	}
}

func TestPrimaryUsesConfiguredForgejoEndpointAndSSHHost(t *testing.T) {
	isolatedMirrorConfig(t)
	root := t.TempDir()
	repoPath := filepath.Join(root, "sample-repo")
	if err := os.Mkdir(repoPath, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", "https://github.com/example/sample-repo.git"}} {
		if output, err := exec.Command("git", append([]string{"-C", repoPath}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/api/v1/repos/search" {
			t.Errorf("unexpected endpoint %s", r.URL.Path)
		}
		if r.URL.Query().Get("page") == "1" {
			fmt.Fprint(w, `{"data":[{"name":"sample-repo"}]}`)
		} else {
			fmt.Fprint(w, `{"data":[]}`)
		}
	}))
	defer server.Close()
	if err := cmdPrimary(server.URL, "forge-user", "ssh.forge.test", []string{root}); err != nil {
		t.Fatal(err)
	}
	if requests == 0 {
		t.Fatal("configured Forgejo endpoint was not queried")
	}
	if got := getGitOriginURL(repoPath); got != "git@ssh.forge.test:forge-user/sample-repo.git" {
		t.Fatalf("unexpected origin %q", got)
	}
}

func TestConfiguredForgejoRemoteMatching(t *testing.T) {
	for _, address := range []string{
		"https://forge.test/owner/repo.git",
		"ssh://git@ssh.forge.test/owner/repo.git",
		"git@ssh.forge.test:owner/repo.git",
	} {
		if !remoteMatchesForgejo(address, "https://forge.test", "ssh.forge.test") {
			t.Errorf("configured Forgejo remote not recognized: %s", address)
		}
	}
	for _, address := range []string{"https://forge.test.example/owner/repo.git", "https://github.com/owner/repo.git"} {
		if remoteMatchesForgejo(address, "https://forge.test", "ssh.forge.test") {
			t.Errorf("unrelated remote recognized: %s", address)
		}
	}
}

func TestStatusUsesConfiguredForgejoEndpointAndSSHHost(t *testing.T) {
	isolatedMirrorConfig(t)
	root := t.TempDir()
	repoPath := filepath.Join(root, "sample-repo")
	if err := os.Mkdir(repoPath, 0o700); err != nil {
		t.Fatal(err)
	}
	commands := [][]string{
		{"init", "-q"},
		{"remote", "add", "origin", "https://github.com/example/sample-repo.git"},
		{"remote", "set-url", "--add", "--push", "origin", "git@ssh.forge.test:forge-user/sample-repo.git"},
	}
	for _, args := range commands {
		if output, err := exec.Command("git", append([]string{"-C", repoPath}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos/search" {
			t.Errorf("unexpected endpoint %s", r.URL.Path)
		}
		if r.URL.Query().Get("page") == "1" {
			fmt.Fprint(w, `{"data":[{"name":"sample-repo","clone_url":"https://forge.test/forge-user/sample-repo.git"}]}`)
		} else {
			fmt.Fprint(w, `{"data":[]}`)
		}
	}))
	defer server.Close()

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	previousStdout := os.Stdout
	defer func() { os.Stdout = previousStdout }()
	os.Stdout = writer
	err = cmdStatus(server.URL, "forge-user", "ssh.forge.test", []string{root})
	_ = writer.Close()
	os.Stdout = previousStdout
	output, readErr := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || readErr != nil {
		t.Fatalf("status err=%v read=%v", err, readErr)
	}
	if !strings.Contains(string(output), "dual-push") {
		t.Fatalf("configured SSH host not reflected in status: %q", output)
	}
}

func TestEnsurePushURLUsesConfiguredForgejoHosts(t *testing.T) {
	repoPath := t.TempDir()
	commands := [][]string{
		{"init", "-q"},
		{"remote", "add", "origin", "https://github.com/example/sample-repo.git"},
		{"remote", "set-url", "--add", "--push", "origin", "https://github.com/example/sample-repo.git"},
		{"remote", "set-url", "--add", "--push", "origin", "git@ssh.forge.test:forge-user/sample-repo.git"},
	}
	for _, args := range commands {
		if output, err := exec.Command("git", append([]string{"-C", repoPath}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	changed, err := ensurePushURL(
		repoPath,
		"https://forge.test/forge-user/sample-repo.git",
		"https://forge.test",
		"ssh.forge.test",
	)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	pushURLs := getExplicitPushURLs(repoPath)
	joined := strings.Join(pushURLs, "\n")
	if strings.Contains(joined, "ssh.forge.test") || !strings.Contains(joined, "https://forge.test/") || !strings.Contains(joined, "https://github.com/") {
		t.Fatalf("unexpected push URLs: %v", pushURLs)
	}
}

func TestCreateUsesGitHubPATFileAndConfiguredEndpoint(t *testing.T) {
	isolatedMirrorConfig(t)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GITHUB_MIRROR_PAT", "")
	patFile := filepath.Join(t.TempDir(), "github-token")
	if err := os.WriteFile(patFile, []byte("file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITHUB_MIRROR_PAT_FILE", patFile)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/forge-user/sample-repo":
			http.NotFound(w, r)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/migrate":
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			if payload["clone_addr"] != "https://github.com/github-user/sample-repo.git" || payload["auth_token"] != "file-token" {
				t.Errorf("unexpected migration payload: %v", payload)
			}
			w.WriteHeader(http.StatusCreated)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer server.Close()
	if err := cmdCreate(server.URL, "forge-user", "github-user", "forge-token", "sample-repo"); err != nil {
		t.Fatal(err)
	}
}

func TestGitHubPATUnreadableFileCannotReachRealCredentialHelper(t *testing.T) {
	isolatedMirrorConfig(t)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GITHUB_MIRROR_PAT", "")
	t.Setenv("GITHUB_MIRROR_PAT_FILE", filepath.Join(t.TempDir(), "missing"))
	if token := getGitHubPAT(); token != "" {
		t.Fatalf("unexpected token from isolated credential environment: %q", token)
	}
}

func TestConfiguredGithubPrimaryReposDefaultAndExplicitEmptyFile(t *testing.T) {
	isolatedMirrorConfig(t)
	unsetEnv(t, "FORGE_MIRROR_GITHUB_PRIMARY_REPOS")
	unsetEnv(t, "FORGE_MIRROR_GITHUB_PRIMARY_REPOS_FILE")
	path := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "forge-mirror", "github-primary-repos")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("Repo-A\nrepo-b"), 0o600); err != nil {
		t.Fatal(err)
	}
	repos, err := configuredGithubPrimaryRepos()
	if err != nil || !repos["repo-a"] || !repos["repo-b"] {
		t.Fatalf("default config: repos=%v, err=%v", repos, err)
	}
	// A supplied list remains additive with both default and explicit files.
	t.Setenv("FORGE_MIRROR_GITHUB_PRIMARY_REPOS", "repo-c")
	repos, err = configuredGithubPrimaryRepos()
	if err != nil || len(repos) != 3 || !repos["repo-c"] {
		t.Fatalf("default file plus environment: repos=%v, err=%v", repos, err)
	}
	t.Setenv("FORGE_MIRROR_GITHUB_PRIMARY_REPOS_FILE", path)
	repos, err = configuredGithubPrimaryRepos()
	if err != nil || len(repos) != 3 {
		t.Fatalf("combined config: repos=%v, err=%v", repos, err)
	}
	t.Setenv("FORGE_MIRROR_GITHUB_PRIMARY_REPOS", "")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	repos, err = configuredGithubPrimaryRepos()
	if err != nil || len(repos) != 0 {
		t.Fatalf("explicit empty config: repos=%v, err=%v", repos, err)
	}
}

func TestPolicyCommandsFailClosedBeforeNetwork(t *testing.T) {
	commands := map[string]func(string) error{
		"mirror-github": func(base string) error {
			return cmdMirrorGitHub(base, "test-user", "test-user", "test-token", nil, false)
		},
		"refresh": func(base string) error {
			return cmdMirrorGitHub(base, "test-user", "test-user", "test-token", nil, true)
		},
		"audit":   func(base string) error { return cmdAudit(base, "test-user", "test-user", "test-token", nil) },
		"convert": func(base string) error { return cmdConvert(base, "test-user", "test-user", "test-token", nil) },
		"recreate": func(base string) error {
			return cmdRecreate(base, "test-user", "test-user", "test-token", []string{"repo-a"})
		},
		"sync":    func(base string) error { return cmdSync(base, "test-user", "", nil) },
		"primary": func(base string) error { return cmdPrimary(base, "test-user", "git.test", nil) },
	}
	for name, command := range commands {
		for _, config := range []string{"missing", "unreadable", "missing-explicit-file-with-env"} {
			t.Run(name+"/"+config, func(t *testing.T) {
				isolatedMirrorConfig(t)
				unsetEnv(t, "FORGE_MIRROR_GITHUB_PRIMARY_REPOS")
				unsetEnv(t, "FORGE_MIRROR_GITHUB_PRIMARY_REPOS_FILE")
				if config == "unreadable" {
					// Reading a directory fails even when tests run as root.
					t.Setenv("FORGE_MIRROR_GITHUB_PRIMARY_REPOS_FILE", t.TempDir())
				}
				if config == "missing-explicit-file-with-env" {
					t.Setenv("FORGE_MIRROR_GITHUB_PRIMARY_REPOS", "repo-a")
					t.Setenv("FORGE_MIRROR_GITHUB_PRIMARY_REPOS_FILE", filepath.Join(t.TempDir(), "missing"))
				}
				requests := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests++
					http.Error(w, "unexpected request", http.StatusInternalServerError)
				}))
				defer server.Close()
				err := command(server.URL)
				if err == nil || !strings.Contains(err.Error(), "github-primary repo config") {
					t.Fatalf("expected configuration error, got %v", err)
				}
				if requests != 0 {
					t.Fatalf("made %d requests before validating policy", requests)
				}
			})
		}
	}
}

func TestAdditionalPolicyInputsFailBeforeNetwork(t *testing.T) {
	tests := []struct {
		name      string
		valueKey  string
		fileKey   string
		command   func(string) error
		errorText string
	}{
		{
			name:     "mirror-github-denial",
			valueKey: "FORGE_MIRROR_GITHUB_DENIED_REPOS",
			fileKey:  "FORGE_MIRROR_GITHUB_DENIED_REPOS_FILE",
			command: func(base string) error {
				return cmdMirrorGitHub(base, "forge-user", "github-user", "token", nil, false)
			},
			errorText: "GitHub mirror denial",
		},
		{
			name:      "audit-required-private",
			valueKey:  "FORGE_MIRROR_REQUIRED_PRIVATE_REPOS",
			fileKey:   "FORGE_MIRROR_REQUIRED_PRIVATE_REPOS_FILE",
			command:   func(base string) error { return cmdAudit(base, "forge-user", "github-user", "token", nil) },
			errorText: "required-private",
		},
		{
			name:      "mirror-codeberg-membership",
			valueKey:  "FORGE_MIRROR_CODEBERG_REPOS",
			fileKey:   "FORGE_MIRROR_CODEBERG_REPOS_FILE",
			command:   func(base string) error { return cmdMirrorCodeberg(base, "forge-user", "codeberg-user", "token", nil) },
			errorText: "Codeberg mirror",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			isolatedMirrorConfig(t)
			unsetEnv(t, test.valueKey)
			unsetEnv(t, test.fileKey)
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests++
				http.Error(w, "unexpected request", http.StatusInternalServerError)
			}))
			defer server.Close()
			err := test.command(server.URL)
			if err == nil || !strings.Contains(err.Error(), test.errorText) {
				t.Fatalf("expected %q configuration error, got %v", test.errorText, err)
			}
			if requests != 0 {
				t.Fatalf("made %d requests before validating policy", requests)
			}
		})
	}
}

func TestGitHubPrimaryExplicitMutationDenied(t *testing.T) {
	commands := map[string]func(string, []string) error{
		"mirror-github": func(base string, names []string) error {
			return cmdMirrorGitHub(base, "test-user", "test-user", "test-token", names, false)
		},
		"refresh": func(base string, names []string) error {
			return cmdMirrorGitHub(base, "test-user", "test-user", "test-token", names, true)
		},
		"convert": func(base string, names []string) error {
			return cmdConvert(base, "test-user", "test-user", "test-token", names)
		},
		"recreate": func(base string, names []string) error {
			return cmdRecreate(base, "test-user", "test-user", "test-token", names)
		},
	}
	for name, command := range commands {
		t.Run(name, func(t *testing.T) {
			isolatedMirrorConfig(t)
			t.Setenv("FORGE_MIRROR_GITHUB_PRIMARY_REPOS", "public-app")
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests++
				http.Error(w, "unexpected request", http.StatusInternalServerError)
			}))
			defer server.Close()
			err := command(server.URL, []string{"forgejo-app", "PUBLIC-App"})
			if err == nil || !strings.Contains(err.Error(), "GitHub-primary") {
				t.Fatalf("expected direction rejection, got %v", err)
			}
			if requests != 0 {
				t.Fatalf("partially applied rejected request: %d requests", requests)
			}
		})
	}
}

func TestMirrorGitHubBulkPreservesForgejoFirst(t *testing.T) {
	for _, refresh := range []bool{false, true} {
		for _, names := range [][]string{nil, {"--all"}, {"forgejo-app"}} {
			t.Run(fmt.Sprintf("refresh=%v/names=%v", refresh, names), func(t *testing.T) {
				isolatedMirrorConfig(t)
				t.Setenv("FORGE_MIRROR_GITHUB_PRIMARY_REPOS", "public-app")
				created, deleted := 0, 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch {
					case strings.Contains(r.URL.Path, "/repos/search"):
						if r.URL.Query().Get("page") == "1" {
							fmt.Fprint(w, `{"data":[{"name":"Public-App"},{"name":"forgejo-app"}]}`)
						} else {
							fmt.Fprint(w, `{"data":[]}`)
						}
					case r.URL.Path == "/api/v1/repos/test-user/forgejo-app/push_mirrors" && r.Method == "GET":
						if refresh && r.URL.Query().Get("page") == "1" {
							fmt.Fprint(w, `[{"remote_address":"https://github.com/test-user/forgejo-app.git","remote_name":"old-mirror"}]`)
						} else {
							fmt.Fprint(w, `[]`)
						}
					case r.URL.Path == "/api/v1/repos/test-user/forgejo-app/push_mirrors/old-mirror" && r.Method == "DELETE":
						deleted++
						w.WriteHeader(http.StatusNoContent)
					case r.URL.Path == "/api/v1/repos/test-user/forgejo-app/push_mirrors" && r.Method == "POST":
						var payload struct {
							Address string `json:"remote_address"`
							Sync    bool   `json:"sync_on_commit"`
						}
						if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
							t.Error(err)
						}
						if payload.Address != "https://github.com/test-user/forgejo-app.git" || !payload.Sync {
							t.Errorf("unexpected mirror: %+v", payload)
						}
						created++
						w.WriteHeader(http.StatusCreated)
					default:
						t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
						http.Error(w, "unexpected request", http.StatusNotFound)
					}
				}))
				defer server.Close()
				if err := cmdMirrorGitHub(server.URL, "test-user", "test-user", "test-token", names, refresh); err != nil {
					t.Fatal(err)
				}
				if created != 1 || (refresh && deleted != 1) || (!refresh && deleted != 0) {
					t.Fatalf("created=%d deleted=%d", created, deleted)
				}
			})
		}
	}
}

func TestAuditGitHubPrimaryDirectionOnly(t *testing.T) {
	for _, address := range []string{"", "https://codeberg.org/test-user/public-app.git", "https://github.com/another-owner/renamed", "git@github.com:test-user/public-app.git", "ssh://git@GitHub.com/test-user/public-app.git"} {
		t.Run(address, func(t *testing.T) {
			isolatedMirrorConfig(t)
			t.Setenv("FORGE_MIRROR_GITHUB_PRIMARY_REPOS", "public-app")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/repos/search":
					if r.URL.Query().Get("page") == "1" {
						fmt.Fprint(w, `{"data":[{"name":"public-app","mirror":true,"private":false,"default_branch":"main","original_url":"https://github.com/test-user/public-app.git"}]}`)
					} else {
						fmt.Fprint(w, `{"data":[]}`)
					}
				case "/api/v1/repos/test-user/public-app/push_mirrors":
					if r.URL.Query().Get("page") == "1" && address != "" {
						json.NewEncoder(w).Encode([]pushMirror{{RemoteAddress: address}})
					} else {
						fmt.Fprint(w, `[]`)
					}
				default:
					t.Errorf("GitHub-primary audit made unexpected request: %s", r.URL.Path)
					http.Error(w, "unexpected request", http.StatusNotFound)
				}
			}))
			defer server.Close()
			err := cmdAudit(server.URL, "test-user", "test-user", "test-token", nil)
			wantDrift := address != "" && !strings.Contains(address, "codeberg.org")
			if (err != nil) != wantDrift {
				t.Fatalf("expected drift=%v, got %v", wantDrift, err)
			}
		})
	}
}

func TestFetchPushMirrorsPaginatesAndFailsClosed(t *testing.T) {
	for _, secondPage := range []string{"mirror", "http-error", "invalid-json"} {
		t.Run(secondPage, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "token test-token" {
					t.Error("missing test authorization")
				}
				switch r.URL.Query().Get("page") {
				case "1":
					fmt.Fprint(w, `[{"remote_address":"https://codeberg.org/test-user/public-app.git"}]`)
				case "2":
					switch secondPage {
					case "mirror":
						fmt.Fprint(w, `[{"remote_address":"https://github.com/test-user/public-app.git"}]`)
					case "http-error":
						http.Error(w, "denied", http.StatusForbidden)
					case "invalid-json":
						fmt.Fprint(w, `not-json`)
					}
				default:
					fmt.Fprint(w, `[]`)
				}
			}))
			defer server.Close()
			result, err := auditGitHubPrimaryRepo(server.URL, "test-user", "test-token", forgejoRepo{Name: "public-app"})
			if secondPage == "mirror" {
				if err != nil || len(result.issues) != 1 {
					t.Fatalf("second-page mirror missed: result=%v err=%v", result, err)
				}
			} else if err == nil {
				t.Fatal("expected failed audit when mirror list is incomplete")
			}
		})
	}
}

func TestIsGitHubRemote(t *testing.T) {
	for _, address := range []string{"https://github.com/a/b.git", "https://GitHub.com/a/b", "git@github.com:a/b.git", "another@github.com:a/b.git", "github.com:a/b.git", "git@ssh.github.com:a/b.git", "ssh://git@ssh.github.com:443/a/b.git", "ssh://git@github.com:22/a/b.git", "https://user:dummy@github.com/a/b.git"} {
		if !isGitHubRemote(address) {
			t.Errorf("not recognized: %s", address)
		}
	}
	for _, address := range []string{"https://codeberg.org/a/b.git", "https://github.com.example.org/a/b.git", "https://example.org/github.com/a/b", "https://github.com@example.org/a/b", "bad%url"} {
		if isGitHubRemote(address) {
			t.Errorf("incorrectly recognized: %s", address)
		}
	}
}

func TestBulkConversionSkipsGitHubPrimary(t *testing.T) {
	for _, command := range []string{"convert", "recreate"} {
		t.Run(command, func(t *testing.T) {
			isolatedMirrorConfig(t)
			t.Setenv("FORGE_MIRROR_GITHUB_PRIMARY_REPOS", "public-app")
			// recreate --all obtains its selection from gh; do not invoke real gh.
			binDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(binDir, "gh"), []byte("#!/bin/sh\nprintf '%s\\n' Public-App forgejo-app\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", binDir)
			deleted, migrated := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/api/v1/repos/search":
					if r.URL.Query().Get("page") == "1" {
						fmt.Fprint(w, `{"data":[{"name":"Public-App","mirror":true},{"name":"forgejo-app","mirror":true}]}`)
					} else {
						fmt.Fprint(w, `{"data":[]}`)
					}
				case r.URL.Path == "/api/v1/repos/test-user/forgejo-app" && r.Method == "DELETE":
					deleted++
					w.WriteHeader(http.StatusNoContent)
				case r.URL.Path == "/api/v1/repos/migrate" && r.Method == "POST":
					var payload struct {
						Name   string `json:"repo_name"`
						Mirror bool   `json:"mirror"`
					}
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Error(err)
					}
					if payload.Name != "forgejo-app" || payload.Mirror {
						t.Errorf("unexpected migration: %+v", payload)
					}
					migrated++
					w.WriteHeader(http.StatusCreated)
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					http.Error(w, "unexpected request", http.StatusNotFound)
				}
			}))
			defer server.Close()
			var err error
			if command == "convert" {
				err = cmdConvert(server.URL, "test-user", "test-user", "test-token", nil)
			} else {
				err = cmdRecreate(server.URL, "test-user", "test-user", "test-token", []string{"--all"})
			}
			if err != nil || deleted != 1 || migrated != 1 {
				t.Fatalf("deleted=%d migrated=%d err=%v", deleted, migrated, err)
			}
		})
	}
}

func TestCodebergMirroringDoesNotRequireGitHubPrimaryPolicy(t *testing.T) {
	isolatedMirrorConfig(t)
	t.Setenv("FORGE_MIRROR_CODEBERG_REPOS", "public-app")
	created := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/repos/search":
			if r.URL.Query().Get("page") == "1" {
				fmt.Fprint(w, `{"data":[{"name":"public-app"},{"name":"not-allowlisted"}]}`)
			} else {
				fmt.Fprint(w, `{"data":[]}`)
			}
		case r.URL.Path == "/api/v1/repos/test-user/public-app/push_mirrors" && r.Method == "GET":
			fmt.Fprint(w, `[]`)
		case r.URL.Path == "/api/v1/repos/test-user/public-app/push_mirrors" && r.Method == "POST":
			var payload struct {
				Address string `json:"remote_address"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			if payload.Address != "https://codeberg.org/test-user/public-app.git" {
				t.Errorf("unexpected Codeberg target %q", payload.Address)
			}
			created++
			w.WriteHeader(http.StatusCreated)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer server.Close()
	if err := cmdMirrorCodeberg(server.URL, "test-user", "test-user", "test-token", nil); err != nil {
		t.Fatal(err)
	}
	if created != 1 {
		t.Fatalf("created %d mirrors", created)
	}
}

func TestGitHubDenialAndPrivateVisibilityAreIndependent(t *testing.T) {
	isolatedMirrorConfig(t)
	t.Setenv("FORGE_MIRROR_GITHUB_DENIED_REPOS", "publication-denied")
	t.Setenv("FORGE_MIRROR_REQUIRED_PRIVATE_REPOS", "private-required")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/repos/search":
			if r.URL.Query().Get("page") == "1" {
				fmt.Fprint(w, `{"data":[{"name":"publication-denied","private":false},{"name":"private-required","private":false,"default_branch":"dev"}]}`)
			} else {
				fmt.Fprint(w, `{"data":[]}`)
			}
		case "/api/v1/repos/test-user/publication-denied/push_mirrors":
			fmt.Fprint(w, `[]`)
		case "/repos/test-user/private-required":
			fmt.Fprint(w, `{"default_branch":"dev"}`)
		case "/api/v1/repos/test-user/private-required/branch_protections":
			fmt.Fprint(w, `[{"rule_name":"main","apply_to_admins":true,"enable_push":false,"enable_merge_whitelist":true,"merge_whitelist_usernames":["test-user"]}]`)
		case "/api/v1/repos/test-user/private-required/push_mirrors":
			if r.URL.Query().Get("page") == "1" {
				fmt.Fprint(w, `[{"remote_address":"https://github.com/test-user/private-required.git","sync_on_commit":true,"last_update":"2026-01-01T00:00:00Z"}]`)
			} else {
				fmt.Fprint(w, `[]`)
			}
		case "/api/v1/repos/test-user/private-required/branches", "/repos/test-user/private-required/branches":
			fmt.Fprint(w, `[]`)
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer server.Close()
	base, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	http.DefaultClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		clone := r.Clone(r.Context())
		clone.URL.Scheme = base.Scheme
		clone.URL.Host = base.Host
		return http.DefaultTransport.RoundTrip(clone)
	})
	if err := cmdAudit(server.URL, "test-user", "test-user", "test-token", nil); err == nil {
		t.Fatal("required-private visibility violation was not reported")
	}
}

func TestGitHubMirrorDenialDoesNotRequirePrivateVisibility(t *testing.T) {
	isolatedMirrorConfig(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos/forge-user/publication-denied/push_mirrors" {
			t.Errorf("unexpected request: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `[]`)
	}))
	defer server.Close()
	result, err := auditGitHubMirrorDeniedRepo(
		server.URL,
		"forge-user",
		"token",
		"github-user",
		forgejoRepo{Name: "publication-denied", Private: false},
	)
	if err != nil || len(result.issues) != 0 {
		t.Fatalf("public publication-denied repo: result=%v err=%v", result, err)
	}
}

func TestForgejoFirstAuditStillChecksBranchPolicy(t *testing.T) {
	for _, branch := range []string{"dev", "main"} {
		t.Run(branch, func(t *testing.T) {
			isolatedMirrorConfig(t)
			t.Setenv("FORGE_MIRROR_GITHUB_PRIMARY_REPOS", "public-app")
			protectionReads := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/repos/search":
					if r.URL.Query().Get("page") == "1" {
						fmt.Fprintf(w, `{"data":[{"name":"forgejo-app","default_branch":%q}]}`, branch)
					} else {
						fmt.Fprint(w, `{"data":[]}`)
					}
				case "/repos/test-user/forgejo-app":
					fmt.Fprint(w, `{"default_branch":"dev"}`)
				case "/api/v1/repos/test-user/forgejo-app/branch_protections":
					protectionReads++
					fmt.Fprint(w, `[{"rule_name":"main","apply_to_admins":true,"enable_push":false,"enable_merge_whitelist":true,"merge_whitelist_usernames":["test-user"]}]`)
				case "/api/v1/repos/test-user/forgejo-app/push_mirrors":
					if r.URL.Query().Get("page") == "1" {
						fmt.Fprint(w, `[{"remote_address":"https://github.com/test-user/forgejo-app.git","sync_on_commit":true,"last_update":"2026-01-01T00:00:00Z"}]`)
					} else {
						fmt.Fprint(w, `[]`)
					}
				case "/api/v1/repos/test-user/forgejo-app/branches", "/repos/test-user/forgejo-app/branches":
					fmt.Fprint(w, `[]`)
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
					http.Error(w, "unexpected request", http.StatusNotFound)
				}
			}))
			defer server.Close()
			base, err := url.Parse(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			http.DefaultClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				clone := r.Clone(r.Context())
				clone.URL.Scheme = base.Scheme
				clone.URL.Host = base.Host
				return http.DefaultTransport.RoundTrip(clone)
			})
			err = cmdAudit(server.URL, "test-user", "test-user", "test-token", nil)
			if (err != nil) != (branch != "dev") || protectionReads != 1 {
				t.Fatalf("branch=%s protectionReads=%d err=%v", branch, protectionReads, err)
			}
		})
	}
}

func TestConfiguredGithubPrimaryReposUsesEnvOverride(t *testing.T) {
	isolatedMirrorConfig(t)
	t.Setenv("FORGE_MIRROR_GITHUB_PRIMARY_REPOS", "repo-a, repo-b\nrepo-c")

	repos, err := configuredGithubPrimaryRepos()
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"repo-a", "repo-b", "repo-c"} {
		if !repos[name] {
			t.Fatalf("expected %q in configured repo set", name)
		}
	}

	if repos["unconfigured-repo"] {
		t.Fatal("unexpected repo in configured set")
	}
}

func TestConfiguredGithubPrimaryReposReadsFile(t *testing.T) {
	isolatedMirrorConfig(t)
	path := filepath.Join(t.TempDir(), "repos.txt")
	t.Setenv("FORGE_MIRROR_GITHUB_PRIMARY_REPOS_FILE", path)

	if err := os.WriteFile(path, []byte("repo-a\nrepo-b,repo-c"), 0o600); err != nil {
		t.Fatal(err)
	}

	repos, err := configuredGithubPrimaryRepos()
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"repo-a", "repo-b", "repo-c"} {
		if !repos[name] {
			t.Fatalf("expected %q in configured repo set", name)
		}
	}
}

func TestConfiguredGithubPrimaryReposFailsClosedWithoutConfig(t *testing.T) {
	isolatedMirrorConfig(t)
	unsetEnv(t, "FORGE_MIRROR_GITHUB_PRIMARY_REPOS")
	unsetEnv(t, "FORGE_MIRROR_GITHUB_PRIMARY_REPOS_FILE")

	repos, err := configuredGithubPrimaryRepos()
	if err == nil {
		t.Fatal("expected missing configuration to be reported")
	}
	if len(repos) != 0 {
		t.Fatalf("expected empty repo set, got %d entries", len(repos))
	}
}

func TestFetchForgejoReposPaginates(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "token test-token" {
			t.Fatalf("unexpected authorization header: %q", got)
		}

		switch r.URL.Query().Get("page") {
		case "1":
			fmt.Fprint(w, `{"data":[{"name":"one"}]}`)
		case "2":
			fmt.Fprint(w, `{"data":[]}`)
		default:
			t.Fatalf("unexpected page: %s", r.URL.Query().Get("page"))
		}
	}))
	defer server.Close()

	repos, err := fetchForgejoRepos(server.URL, "test-user", "test-token")
	if err != nil {
		t.Fatalf("fetchForgejoRepos returned error: %v", err)
	}
	if len(repos) != 1 || repos[0].Name != "one" {
		t.Fatalf("unexpected repositories: %#v", repos)
	}
}

func TestFetchForgejoReposReportsHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, "404 page not found")
	}))
	defer server.Close()

	_, err := fetchForgejoRepos(server.URL, "test-user", "test-token")
	if err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("expected HTTP 404 error, got %v", err)
	}
}
