package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeAuditPolicy(t *testing.T, repos []auditRepository) {
	t.Helper()
	data, err := json.Marshal(auditPolicy{SchemaVersion: 1, Repositories: repos})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FORGE_MIRROR_AUDIT_POLICY_FILE", path)
}
func captureAudit(t *testing.T, run func()) string {
	t.Helper()
	old := os.Stdout
	file, err := os.CreateTemp(t.TempDir(), "output")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	os.Stdout = file
	defer func() { os.Stdout = old }()
	run()
	file.Seek(0, 0)
	data, _ := io.ReadAll(file)
	return string(data)
}
func TestDeclaredAudit(t *testing.T) {
	tests := []struct {
		name, canonical, mirror, branch string
		retired, private                bool
		ghStatus, protectionStatus      int
		protect, actual                 map[string]interface{}
		mirrors                         []pushMirror
		fg, gh                          map[string]string
		want                            string
		code                            int
	}{
		{name: "optional 404 checks protection", canonical: "forgejo", mirror: "optional", branch: "main", ghStatus: 404, protect: map[string]interface{}{}, want: "main branch protection missing", code: 1},
		{name: "optional absent target", canonical: "forgejo", mirror: "optional", branch: "dev", ghStatus: 404},
		{name: "required mirror", canonical: "forgejo", mirror: "required", branch: "main", ghStatus: 404, want: "required GitHub push mirror missing", code: 1},
		{name: "records no target", canonical: "forgejo", mirror: "forbidden", branch: "main"},
		{name: "preservation no target", canonical: "upstream", mirror: "forbidden", branch: "master"},
		{name: "denied publication", canonical: "forgejo", mirror: "forbidden", branch: "dev", mirrors: []pushMirror{{RemoteAddress: "https://github.com/distribution/renamed.git"}}, want: "outgoing push mirror forbidden", code: 1},
		{name: "retired excluded", canonical: "archived", mirror: "disabled", branch: "main", retired: true},
		{name: "retired private history privacy", canonical: "archived", mirror: "forbidden", retired: true, private: true, want: "requires private visibility", code: 1},
		{name: "moved GitHub primary", canonical: "github", mirror: "continuity", branch: "main"},
		{name: "reverse mirror", canonical: "github", mirror: "continuity", branch: "main", mirrors: []pushMirror{{RemoteAddress: "https://github.com/distribution/renamed.git"}}, want: "GitHub-primary", code: 1},
		{name: "main only", canonical: "forgejo", mirror: "optional", branch: "main", fg: map[string]string{"main": "abc"}, gh: map[string]string{"main": "abc"}},
		{name: "dev only", canonical: "forgejo", mirror: "optional", branch: "dev", fg: map[string]string{"dev": "abc"}, gh: map[string]string{"dev": "abc"}},
		{name: "master only", canonical: "forgejo", mirror: "optional", branch: "master", fg: map[string]string{"master": "abc"}, gh: map[string]string{"master": "abc"}},
		{name: "explicit disabled merge whitelist", canonical: "forgejo", mirror: "optional", branch: "main", ghStatus: 404, protect: map[string]interface{}{"enable_merge_whitelist": false}, actual: map[string]interface{}{"enable_merge_whitelist": false}},
		{name: "exact automation actor", canonical: "forgejo", mirror: "optional", branch: "main", ghStatus: 404, protect: map[string]interface{}{"enable_push": true, "enable_push_whitelist": true, "push_whitelist_usernames": []string{"image-bot"}, "push_whitelist_teams": []string{}, "push_whitelist_deploy_keys": false}, actual: map[string]interface{}{"enable_push": true, "enable_push_whitelist": true, "push_whitelist_usernames": []string{"image-bot"}, "push_whitelist_teams": []string{}, "push_whitelist_deploy_keys": false}},
		{name: "extra automation actor rejected", canonical: "forgejo", mirror: "optional", branch: "main", ghStatus: 404, protect: map[string]interface{}{"push_whitelist_usernames": []string{"image-bot"}}, actual: map[string]interface{}{"push_whitelist_usernames": []string{"image-bot", "human"}}, want: "push_whitelist_usernames", code: 1},
		{name: "unrestricted push rejected", canonical: "forgejo", mirror: "optional", branch: "main", ghStatus: 404, protect: map[string]interface{}{"enable_push": false}, actual: map[string]interface{}{"enable_push": true}, want: "enable_push", code: 1},
		{name: "real refs", canonical: "forgejo", mirror: "optional", branch: "main", fg: map[string]string{"main": "abc", "local": "abc"}, gh: map[string]string{"main": "xyz", "remote": "xyz"}, want: "differs", code: 1},
		{name: "manual review unresolved", canonical: "review", mirror: "review", branch: "main", ghStatus: 404, want: "owner review", code: 1},
		{name: "execution and findings", canonical: "review", mirror: "review", branch: "main", ghStatus: 404, fg: map[string]string{"main": "abc"}, protectionStatus: 403, want: "owner review", code: 2},
		{name: "undeclared optional destination", canonical: "forgejo", mirror: "optional", branch: "main", ghStatus: 404, mirrors: []pushMirror{{RemoteAddress: "https://github.com/unexpected/example.git"}}, want: "undeclared destination", code: 1},
		{name: "mirror purpose unresolved independently", canonical: "forgejo", mirror: "review", branch: "main", want: "requires owner review", code: 1},
		{name: "review retains explicit contract", canonical: "review", mirror: "review", branch: "main", fg: map[string]string{"main": "abc"}, protect: map[string]interface{}{"enable_push": false}, actual: map[string]interface{}{"enable_push": true}, want: "enable_push", code: 1},
		{name: "continuity retains explicit contract", canonical: "github", mirror: "continuity", branch: "main", protect: map[string]interface{}{"enable_push": false}, actual: map[string]interface{}{"enable_push": true}, want: "enable_push", code: 1},
		{name: "upstream retains explicit contract", canonical: "upstream", mirror: "forbidden", branch: "main", protect: map[string]interface{}{"enable_push": false}, actual: map[string]interface{}{"enable_push": true}, want: "enable_push", code: 1},

		{name: "optional server failure", canonical: "forgejo", mirror: "optional", branch: "dev", ghStatus: 500, code: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolatedMirrorConfig(t)
			rule := auditRepository{FullName: "other-owner/example", Class: "synthetic", Canonical: tt.canonical, GitHub: "distribution/renamed", Mirror: tt.mirror, DefaultBranch: tt.branch, Retired: tt.retired, RequiredPrivate: tt.private}
			if tt.protect != nil {
				rule.Protections = map[string]map[string]interface{}{"main": tt.protect}
			}
			writeAuditPolicy(t, []auditRepository{rule})
			var paths []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.Path)
				if r.Method != "GET" {
					t.Errorf("mutation: %s", r.Method)
					http.Error(w, "mutation", 500)
					return
				}
				switch r.URL.Path {
				case "/api/v1/repos/search":
					if r.URL.Query().Has("owner") || r.URL.Query().Has("uid") {
						t.Error("wrong owner filter")
					}
					repos := []forgejoRepo{}
					if r.URL.Query().Get("page") == "1" {
						repos = append(repos, forgejoRepo{Name: "example", FullName: rule.FullName, DefaultBranch: tt.branch})
					}
					json.NewEncoder(w).Encode(map[string]interface{}{"data": repos})
				case "/api/v1/repos/other-owner/example/push_mirrors":
					mirrors := []pushMirror{}
					if r.URL.Query().Get("page") == "1" {
						mirrors = append(mirrors, tt.mirrors...)
					}
					json.NewEncoder(w).Encode(mirrors)
				case "/api/v1/repos/other-owner/example/branch_protections":
					if tt.protectionStatus != 0 {
						http.Error(w, "private response", tt.protectionStatus)
						return
					}
					protections := []map[string]interface{}{}
					if r.URL.RawQuery != "" {
						t.Error("protection endpoint is not paginated")
					}
					if tt.actual != nil {
						p := map[string]interface{}{"rule_name": "main"}
						for k, v := range tt.actual {
							p[k] = v
						}
						protections = append(protections, p)
					}
					json.NewEncoder(w).Encode(protections)
				case "/api/v1/repos/other-owner/example/branches", "/repos/distribution/renamed/branches":
					heads := tt.fg
					if strings.HasPrefix(r.URL.Path, "/repos/") {
						heads = tt.gh
					}
					batch := []map[string]interface{}{}
					if r.URL.Query().Get("page") == "1" {
						for name, sha := range heads {
							batch = append(batch, map[string]interface{}{"name": name, "protected": false, "commit": map[string]string{"id": sha, "sha": sha}})
						}
					}
					json.NewEncoder(w).Encode(batch)
				case "/repos/distribution/renamed":
					if tt.ghStatus != 0 {
						http.Error(w, "private response", tt.ghStatus)
						return
					}
					json.NewEncoder(w).Encode(githubRepo{DefaultBranch: tt.branch})
				default:
					t.Errorf("wrong owner or unexpected API: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			base, _ := url.Parse(server.URL)
			old := http.DefaultClient
			http.DefaultClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				clone := r.Clone(r.Context())
				clone.URL.Scheme = base.Scheme
				clone.URL.Host = base.Host
				return http.DefaultTransport.RoundTrip(clone)
			})}
			defer func() { http.DefaultClient = old }()
			var err error
			output := captureAudit(t, func() { err = cmdAudit(server.URL, "old-owner", "distribution", "test-token", nil) })
			if code := auditExitCode(err); code != tt.code {
				t.Fatalf("exit=%d want=%d: %v\n%s", code, tt.code, err, output)
			}
			if tt.want != "" && !strings.Contains(output, tt.want) {
				t.Fatalf("missing %q: %s", tt.want, output)
			}
			if strings.Contains(output, "private response") {
				t.Fatal("response body leaked")
			}
			if tt.mirror == "forbidden" || tt.mirror == "disabled" || tt.mirror == "continuity" {
				for _, path := range paths {
					if strings.HasPrefix(path, "/repos/") {
						t.Fatal("inappropriate target lookup")
					}
				}
			}
			if tt.name == "execution and findings" && !strings.Contains(output, "branch API") {
				t.Fatal(output)
			}
			if tt.name == "real refs" {
				for _, want := range []string{"exists only on Forgejo", "exists only on GitHub"} {
					if !strings.Contains(output, want) {
						t.Fatal(output)
					}
				}
			}
			if tt.name == "optional 404 checks protection" && !containsString(paths, "/api/v1/repos/other-owner/example/branch_protections") {
				t.Fatal("local protection check suppressed")
			}
		})
	}
}

func TestAuditPolicyFailsClosed(t *testing.T) {
	isolatedMirrorConfig(t)
	for _, raw := range []string{`{}`, `{"schema_version":2,"repositories":[]}`, `{"schema_version":1,"repositories":[{"full_name":"../example","canonical":"forgejo","mirror":"optional"}]}`, `{"schema_version":1,"unknown":true,"repositories":[]}`} {
		path := filepath.Join(t.TempDir(), "policy.json")
		os.WriteFile(path, []byte(raw), 0600)
		t.Setenv("FORGE_MIRROR_AUDIT_POLICY_FILE", path)
		_, err := loadAuditPolicy()
		if auditExitCode(err) != 2 {
			t.Fatalf("invalid policy accepted: %s", raw)
		}
	}
}

func TestAuditRetainsFindingsOnLaterExecutionFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/push_mirrors"):
			if r.URL.Query().Get("page") == "1" {
				io.WriteString(w, `[{"remote_address":"https://github.com/team/example.git"}]`)
			} else {
				http.Error(w, "secret-looking response body", 500)
			}
		case strings.HasSuffix(r.URL.Path, "/branches"):
			io.WriteString(w, `[]`)
		default:
			t.Error(r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	findings, execution := auditDeclaredRepo(server.URL, "token", "", forgejoRepo{}, auditRepository{FullName: "team/example", Canonical: "forgejo", Mirror: "forbidden", RequiredPrivate: true})
	if len(findings) != 2 || len(execution) != 1 || strings.Contains(strings.Join(execution, ""), "secret-looking") {
		t.Fatalf("findings=%v errors=%v", findings, execution)
	}
}

func TestAuditPushExceptionsRejectTeamsAndDeployKeys(t *testing.T) {
	for _, test := range []struct {
		field            string
		actual, expected interface{}
	}{
		{"enable_push_whitelist", false, true},
		{"push_whitelist_teams", []string{"extra-team"}, []string{}},
		{"push_whitelist_deploy_keys", true, false},
	} {
		if auditFieldMatches(test.field, test.actual, test.expected) {
			t.Fatal("widened exception accepted: " + test.field)
		}
	}
}

func TestAuditUnclassifiedDiscoveryAndAdditiveDenials(t *testing.T) {
	for _, denied := range []bool{false, true} {
		t.Run(fmt.Sprintf("denied=%v", denied), func(t *testing.T) {
			isolatedMirrorConfig(t)
			writeAuditPolicy(t, []auditRepository{{FullName: "archive/old", Canonical: "archived", Mirror: "disabled", Retired: true}})
			if denied {
				t.Setenv("FORGE_MIRROR_GITHUB_DENIED_REPOS", "example")
				t.Setenv("FORGE_MIRROR_REQUIRED_PRIVATE_REPOS", "new-owner/example")
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/api/v1/repos/search":
					if r.URL.Query().Get("page") == "1" {
						io.WriteString(w, `{"data":[{"full_name":"new-owner/example","name":"example","default_branch":"master"}]}`)
					} else {
						io.WriteString(w, `{"data":[]}`)
					}
				case r.URL.Path == "/api/v1/repos/new-owner/example/push_mirrors":
					if r.URL.Query().Get("page") == "1" {
						io.WriteString(w, `[{"remote_address":"https://github.com/distribution/example.git"}]`)
					} else {
						io.WriteString(w, `[]`)
					}
				case r.URL.Path == "/api/v1/repos/new-owner/example/branches":
					io.WriteString(w, `[]`)
				case r.URL.Path == "/repos/distribution/example":
					if denied {
						t.Error("activated denial allowed a target lookup")
					}
					http.NotFound(w, r)
				default:
					t.Error(r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			base, _ := url.Parse(server.URL)
			old := http.DefaultClient
			http.DefaultClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				clone := r.Clone(r.Context())
				clone.URL.Scheme = base.Scheme
				clone.URL.Host = base.Host
				return http.DefaultTransport.RoundTrip(clone)
			})}
			defer func() { http.DefaultClient = old }()
			var err error
			output := captureAudit(t, func() { err = cmdAudit(server.URL, "old-owner", "distribution", "token", nil) })
			if auditExitCode(err) != 1 || !strings.Contains(output, "[unclassified]") || !strings.Contains(output, "owner review") {
				t.Fatalf("%v: %s", err, output)
			}
			if denied && (!strings.Contains(output, "requires private visibility") || !strings.Contains(output, "push mirror forbidden")) {
				t.Fatal(output)
			}
		})
	}
}

func TestAuditInaccessibleUnresolvedRepository(t *testing.T) {
	for _, canonical := range []string{"review", "forgejo"} {
		t.Run(canonical, func(t *testing.T) {
			isolatedMirrorConfig(t)
			writeAuditPolicy(t, []auditRepository{{FullName: "team/undecided", Class: "unresolved", Canonical: canonical, Mirror: "review"}})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/repos/search" {
					io.WriteString(w, `{"data":[]}`)
				} else {
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			var err error
			output := captureAudit(t, func() { err = cmdAudit(server.URL, "team", "distribution", "token", nil) })
			if auditExitCode(err) != 2 || !strings.Contains(output, "requires owner review") || !strings.Contains(output, "absent or inaccessible") {
				t.Fatalf("%v: %s", err, output)
			}
		})
	}
}

func TestAuditLegacyDirectionAndSourceDenials(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "1" {
			io.WriteString(w, `[{"remote_address":"https://github.com/distribution/example.git","sync_on_commit":true,"last_update":"2026-01-01T00:00:00Z"}]`)
		} else {
			io.WriteString(w, `[]`)
		}
	}))
	defer server.Close()
	findings, execution := auditDeclaredRepo(server.URL, "token", "", forgejoRepo{Mirror: true, OriginalURL: "https://github.com/source/example.git"}, auditRepository{FullName: "team/example", GitHub: "distribution/example", Canonical: "upstream", Mirror: "optional", Retired: true, GitHubPrimary: true, DenyGitHubSource: true})
	output := strings.Join(findings, "\n")
	if len(execution) != 0 || !strings.Contains(output, "GitHub-primary") || !strings.Contains(output, "source denied") {
		t.Fatalf("findings=%v execution=%v", findings, execution)
	}
}

func TestAuditRetiredLegacyPrimaryStillChecks(t *testing.T) {
	isolatedMirrorConfig(t)
	t.Setenv("FORGE_MIRROR_GITHUB_PRIMARY_REPOS", "example")
	writeAuditPolicy(t, []auditRepository{{FullName: "team/example", Canonical: "archived", Mirror: "disabled", Retired: true}})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/repos/search":
			if r.URL.Query().Get("page") == "1" {
				io.WriteString(w, `{"data":[{"full_name":"team/example","name":"example"}]}`)
			} else {
				io.WriteString(w, `{"data":[]}`)
			}
		case "/api/v1/repos/team/example/push_mirrors":
			if r.URL.Query().Get("page") == "1" {
				io.WriteString(w, `[{"remote_address":"https://github.com/team/example.git"}]`)
			} else {
				io.WriteString(w, `[]`)
			}
		default:
			t.Error("retired active/target lookup: " + r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	var err error
	output := captureAudit(t, func() { err = cmdAudit(server.URL, "team", "distribution", "token", nil) })
	if auditExitCode(err) != 1 || !strings.Contains(output, "push mirror present for retired repository") {
		t.Fatalf("%v: %s", err, output)
	}
}
