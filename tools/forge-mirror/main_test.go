package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Never let these tests use the operator's policy or credential files.
func isolatedMirrorConfig(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("FORGE_MIRROR_GITHUB_PRIMARY_REPOS", "")
	t.Setenv("FORGE_MIRROR_GITHUB_PRIMARY_REPOS_FILE", "")
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

func TestConfiguredGithubPrimaryReposDefaultAndExplicitEmptyFile(t *testing.T) {
	isolatedMirrorConfig(t)
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
		"mirror-github": func(base string) error { return cmdMirrorGitHub(base, "test-user", "test-token", nil, false) },
		"refresh":       func(base string) error { return cmdMirrorGitHub(base, "test-user", "test-token", nil, true) },
		"audit":         func(base string) error { return cmdAudit(base, "test-user", "test-token", nil) },
		"convert":       func(base string) error { return cmdConvert(base, "test-user", "test-token", nil) },
		"recreate":      func(base string) error { return cmdRecreate(base, "test-user", "test-token", []string{"repo-a"}) },
		"sync":          func(base string) error { return cmdSync(base, "test-user", nil) },
		"primary":       func(_ string) error { return cmdPrimary("test-user", nil) },
	}
	for name, command := range commands {
		for _, config := range []string{"missing", "unreadable", "missing-explicit-file-with-env"} {
			t.Run(name+"/"+config, func(t *testing.T) {
				isolatedMirrorConfig(t)
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

func TestGitHubPrimaryExplicitMutationDenied(t *testing.T) {
	commands := map[string]func(string, []string) error{
		"mirror-github": func(base string, names []string) error {
			return cmdMirrorGitHub(base, "test-user", "test-token", names, false)
		},
		"refresh": func(base string, names []string) error {
			return cmdMirrorGitHub(base, "test-user", "test-token", names, true)
		},
		"convert":  func(base string, names []string) error { return cmdConvert(base, "test-user", "test-token", names) },
		"recreate": func(base string, names []string) error { return cmdRecreate(base, "test-user", "test-token", names) },
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
				if err := cmdMirrorGitHub(server.URL, "test-user", "test-token", names, refresh); err != nil {
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
			err := cmdAudit(server.URL, "test-user", "test-token", nil)
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
				err = cmdConvert(server.URL, "test-user", "test-token", nil)
			} else {
				err = cmdRecreate(server.URL, "test-user", "test-token", []string{"--all"})
			}
			if err != nil || deleted != 1 || migrated != 1 {
				t.Fatalf("deleted=%d migrated=%d err=%v", deleted, migrated, err)
			}
		})
	}
}

func TestCodebergMirroringDoesNotRequireGitHubPrimaryPolicy(t *testing.T) {
	isolatedMirrorConfig(t)
	previousAllowlist := codebergAllowlist
	codebergAllowlist = map[string]bool{"public-app": true}
	t.Cleanup(func() { codebergAllowlist = previousAllowlist })
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
	if err := cmdMirrorCodeberg(server.URL, "test-user", "test-token", nil); err != nil {
		t.Fatal(err)
	}
	if created != 1 {
		t.Fatalf("created %d mirrors", created)
	}
}

func TestPrivateSourceAuditTakesPrecedence(t *testing.T) {
	isolatedMirrorConfig(t)
	t.Setenv("FORGE_MIRROR_GITHUB_PRIMARY_REPOS", "private-source")
	previousDenylist := githubMirrorDenylist
	githubMirrorDenylist = map[string]string{"private-source": "private-source policy"}
	t.Cleanup(func() { githubMirrorDenylist = previousDenylist })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/repos/search":
			if r.URL.Query().Get("page") == "1" {
				fmt.Fprint(w, `{"data":[{"name":"private-source","private":false}]}`)
			} else {
				fmt.Fprint(w, `{"data":[]}`)
			}
		case "/api/v1/repos/test-user/private-source/push_mirrors":
			fmt.Fprint(w, `[]`)
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer server.Close()
	if err := cmdAudit(server.URL, "test-user", "test-token", nil); err == nil {
		t.Fatal("private-source visibility violation masked by local GitHub-primary exclusion")
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
			err = cmdAudit(server.URL, "test-user", "test-token", nil)
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

	repos, err := fetchForgejoRepos(server.URL, "alcxyz", "test-token")
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

	_, err := fetchForgejoRepos(server.URL, "alcxyz", "test-token")
	if err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("expected HTTP 404 error, got %v", err)
	}
}
