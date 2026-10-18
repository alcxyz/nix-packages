package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func credentialRequest(path string) string {
	return "protocol=https\nhost=github.com\npath=" + path + "\n\n"
}

func TestCredential(t *testing.T) {
	fake, srv := newFakeGitHub(t, manyRepos())
	cfg := testConfig(t, srv.URL)
	entry, err := newMinter(cfg).token()
	if err != nil {
		t.Fatal(err)
	}
	answer := fmt.Sprintf("username=x-access-token\npassword=%s\npassword_expiry_utc=%d\n", testToken, entry.ExpiresAt.Unix())

	tests := []struct {
		name  string
		op    string
		input string
		want  string
	}{
		{"installed", "get", credentialRequest(testOwner + "/installed.git"), answer},
		{"installed without .git", "get", credentialRequest(testOwner + "/Installed"), answer},
		{"not installed", "get", credentialRequest(testOwner + "/absent.git"), ""},
		{"other owner", "get", credentialRequest("someone/else.git"), ""},
		{"other host", "get", "protocol=https\nhost=forge.example.test\npath=" + testOwner + "/installed.git\n", ""},
		{"http", "get", "protocol=http\nhost=github.com\npath=" + testOwner + "/installed.git\n", ""},
		{"store", "store", "protocol=https\nhost=github.com\npath=" + testOwner + "/installed.git\nusername=x\npassword=y\n", ""},
		{"erase", "erase", credentialRequest(testOwner + "/installed.git"), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := runCredential(cfg, []string{tt.op}, strings.NewReader(tt.input), &stdout, &stderr); code != 0 {
				t.Fatalf("exit %d", code)
			}
			if stdout.String() != tt.want || stderr.Len() != 0 {
				t.Fatalf("stdout = %q, stderr = %q", stdout.String(), stderr.String())
			}
		})
	}

	t.Run("no path stops Git", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		runCredential(cfg, []string{"get"}, strings.NewReader("protocol=https\nhost=github.com\n\n"), &stdout, &stderr)
		if stdout.String() != "quit=1\n" || !strings.Contains(stderr.String(), "useHttpPath") {
			t.Fatalf("stdout = %q, stderr = %q", stdout.String(), stderr.String())
		}
	})

	t.Run("operator override answers nothing", func(t *testing.T) {
		t.Setenv("GH_AS_OPERATOR", "1")
		failing := testConfig(t, srv.URL)
		failing.keyFile = filepath.Join(t.TempDir(), "missing.pem")
		for _, c := range []config{cfg, failing} {
			var stdout, stderr bytes.Buffer
			runCredential(c, []string{"get"}, strings.NewReader(credentialRequest(testOwner+"/installed.git")), &stdout, &stderr)
			if stdout.Len() != 0 || stderr.Len() != 0 {
				t.Fatalf("stdout = %q, stderr = %q", stdout.String(), stderr.String())
			}
		}
	})

	t.Run("unconfigured answers nothing", func(t *testing.T) {
		before := fake.calls()
		var stdout, stderr bytes.Buffer
		unconfigured := config{apiURL: srv.URL}
		runCredential(unconfigured, []string{"get"}, strings.NewReader(credentialRequest(testOwner+"/installed.git")), &stdout, &stderr)
		if stdout.Len() != 0 || stderr.Len() != 0 || fake.calls() != before {
			t.Fatalf("stdout = %q, stderr = %q", stdout.String(), stderr.String())
		}
	})

	t.Run("invalid owner stops Git", func(t *testing.T) {
		before := fake.calls()
		var stdout, stderr bytes.Buffer
		invalid := cfg
		invalid.owner = "acme/x"
		runCredential(invalid, []string{"get"}, strings.NewReader(credentialRequest("someone/else.git")), &stdout, &stderr)
		if stdout.String() != "quit=1\n" || stderr.Len() == 0 || fake.calls() != before {
			t.Fatalf("stdout = %q, stderr = %q", stdout.String(), stderr.String())
		}
	})

	failures := []struct {
		name   string
		mutate func(*config)
	}{
		{"listing fails", nil},
		{"key missing", func(c *config) { c.keyFile = filepath.Join(t.TempDir(), "missing.pem") }},
		{"partial configuration", func(c *config) { c.appID = "" }},
	}
	for _, tt := range failures {
		t.Run("quit when "+tt.name, func(t *testing.T) {
			if tt.mutate == nil {
				fake.mu.Lock()
				fake.failRepos = true
				fake.mu.Unlock()
				defer func() {
					fake.mu.Lock()
					fake.failRepos = false
					fake.mu.Unlock()
				}()
			}
			c := testConfig(t, srv.URL)
			if tt.mutate != nil {
				tt.mutate(&c)
			}
			var stdout, stderr bytes.Buffer
			if code := runCredential(c, []string{"get"}, strings.NewReader(credentialRequest(testOwner+"/installed.git")), &stdout, &stderr); code != 0 {
				t.Fatalf("exit %d", code)
			}
			if stdout.String() != "quit=1\n" || strings.Count(stderr.String(), "\n") != 1 || strings.Contains(stderr.String(), testToken) {
				t.Fatalf("stdout = %q, stderr = %q", stdout.String(), stderr.String())
			}
			// Another owner's repository is still left to the next helper.
			stdout.Reset()
			stderr.Reset()
			runCredential(c, []string{"get"}, strings.NewReader(credentialRequest("someone/else.git")), &stdout, &stderr)
			if stdout.Len() != 0 || stderr.Len() != 0 {
				t.Fatalf("other owner: stdout = %q, stderr = %q", stdout.String(), stderr.String())
			}
		})
	}
}

func TestPlanGHConfigurationAndCommands(t *testing.T) {
	isolateGit(t)
	fake, srv := newFakeGitHub(t, manyRepos())
	installedCheckout := gitRepo(t, [][2]string{{"origin", "git@github.com:" + testOwner + "/installed.git"}}, nil)
	env := []string{"PATH=/bin"}

	t.Run("unconfigured passes through", func(t *testing.T) {
		cfg := config{ghPath: "/nonexistent/gh", apiURL: srv.URL}
		p, err := planGH(cfg, []string{"pr", "list"}, env, installedCheckout, newMinter(cfg))
		if err != nil || p.withToken || p.path != cfg.ghPath {
			t.Fatalf("plan = %+v, %v", p, err)
		}
	})

	t.Run("partial configuration fails", func(t *testing.T) {
		for _, mutate := range []func(*config){
			func(c *config) { c.appID = "" },
			func(c *config) { c.keyFile = "" },
			func(c *config) { c.appID, c.keyFile = "", "" },
		} {
			cfg := testConfig(t, srv.URL)
			mutate(&cfg)
			_, err := planGH(cfg, []string{"auth", "status"}, env, installedCheckout, newMinter(cfg))
			if err != errPartialConfig {
				t.Fatalf("error = %v", err)
			}
		}
	})

	t.Run("operator commands in an installed checkout", func(t *testing.T) {
		cfg := testConfig(t, srv.URL)
		before := fake.calls()
		for _, args := range [][]string{
			{"repo", "list"}, {"repo", "create", testOwner + "/new"}, {"repo", "fork"},
			{"search", "repos", "x"}, {"gist", "list"}, {"auth", "status"}, {"org", "list"},
			{"status"}, {"secret", "list"}, {"variable", "list"}, {"browse"}, {"api", "user"},
			{"config", "get", "editor"}, {"extension", "list"}, {"version"}, {"help"},
		} {
			p, err := planGH(cfg, args, env, installedCheckout, newMinter(cfg))
			if err != nil || p.withToken {
				t.Fatalf("%v: plan = %+v, %v", args, p, err)
			}
		}
		if fake.calls() != before {
			t.Fatal("an operator command touched the API")
		}
		if _, err := os.Stat(cfg.cacheDir); !os.IsNotExist(err) {
			t.Fatal("an operator command touched the cache")
		}
	})

	t.Run("app commands in an installed checkout", func(t *testing.T) {
		cfg := testConfig(t, srv.URL)
		for _, args := range [][]string{
			{"pr", "list"}, {"issue", "view", "3"}, {"release", "list"}, {"run", "list"},
			{"workflow", "list"}, {"label", "list"}, {"cache", "list"}, {"repo", "view"},
			{"repo", "deploy-key", "list"}, {"api", "repos/{owner}/{repo}/pulls"},
		} {
			p, err := planGH(cfg, args, env, installedCheckout, newMinter(cfg))
			if err != nil || !p.withToken {
				t.Fatalf("%v: plan = %+v, %v", args, p, err)
			}
		}
	})

	t.Run("another host runs as the operator", func(t *testing.T) {
		cfg := testConfig(t, srv.URL)
		for _, tc := range []struct {
			args []string
			env  []string
		}{
			{[]string{"pr", "list"}, []string{"GH_HOST=ghe.example.test"}},
			{[]string{"api", "--hostname", "ghe.example.test", "repos/" + testOwner + "/installed"}, nil},
		} {
			p, err := planGH(cfg, tc.args, append(env, tc.env...), installedCheckout, newMinter(cfg))
			if err != nil || p.withToken {
				t.Fatalf("%v: plan = %+v, %v", tc.args, p, err)
			}
		}
		p, err := planGH(cfg, []string{"api", "--hostname", "github.com", "repos/" + testOwner + "/installed"}, env, installedCheckout, newMinter(cfg))
		if err != nil || !p.withToken {
			t.Fatalf("hostname github.com: plan = %+v, %v", p, err)
		}
	})
}
