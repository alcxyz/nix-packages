package main

import (
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadOnly(t *testing.T) {
	fake, srv := newFakeGitHub(t, manyRepos())
	work := t.TempDir()
	record := filepath.Join(work, "record")
	bin := filepath.Join(work, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n{ printf 'args:%s\\n' \"$*\"\n" +
		"for v in GH_TOKEN GITHUB_TOKEN GH_HOST GH_APP_ID GH_APP_KEY_FILE GH_AS_OPERATOR GH_PROMPT_DISABLED KEEP; do eval \"printf '%s=%s\\n' $v \\\"\\${$v-unset}\\\"\"; done\n" +
		"printf 'GH_CONFIG_DIR=%s\\n' \"$GH_CONFIG_DIR\"; } > \"$RECORD\"\nexit 3\n"
	if err := os.WriteFile(filepath.Join(bin, "inventory"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t, srv.URL)
	env := []string{
		"RECORD=" + record,
		"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"GH_APP_ID=" + cfg.appID,
		"GH_APP_KEY_FILE=" + cfg.keyFile,
		"GH_APP_OWNER=" + cfg.owner,
		"GH_APP_API_URL=" + srv.URL,
		"GH_APP_CACHE_DIR=" + cfg.cacheDir,
		"GH_TOKEN=operator",
		"GITHUB_TOKEN=operator",
		"GH_HOST=example.invalid",
		"GH_AS_OPERATOR=1",
		"KEEP=kept",
	}
	readRecord := func() map[string]string {
		t.Helper()
		data, err := os.ReadFile(record)
		if err != nil {
			t.Fatal(err)
		}
		os.Remove(record)
		got := map[string]string{}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			key, value, _ := strings.Cut(line, "=")
			if rest, ok := strings.CutPrefix(line, "args:"); ok {
				key, value = "args", rest
			}
			got[key] = value
		}
		return got
	}

	t.Run("runs with a read-only token", func(t *testing.T) {
		code, stdout, stderr := runBinary(t, work, env, "read-only", "--", "inventory", "--days", "30")
		if code != 3 || stdout != "" || stderr != "" {
			t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
		}
		got := readRecord()
		want := map[string]string{
			"args":               "--days 30",
			"GH_TOKEN":           testToken + "read",
			"GITHUB_TOKEN":       "unset",
			"GH_HOST":            "unset",
			"GH_APP_ID":          "unset",
			"GH_APP_KEY_FILE":    "unset",
			"GH_AS_OPERATOR":     "unset",
			"GH_PROMPT_DISABLED": "1",
			"KEEP":               "kept",
			"GH_CONFIG_DIR":      filepath.Join(cfg.cacheDir, testAppID+"-"+testOwner+"-read-gh"),
		}
		if !maps.Equal(got, want) {
			t.Fatalf("record = %v, want %v", got, want)
		}
		fake.mu.Lock()
		requested := fake.requested
		fake.mu.Unlock()
		if !maps.Equal(requested, readOnlyPermissions) {
			t.Fatalf("requested %v", requested)
		}
		if info, err := os.Stat(got["GH_CONFIG_DIR"]); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Fatalf("config dir: %v, %v", info, err)
		}
		entries, _ := os.ReadDir(got["GH_CONFIG_DIR"])
		if len(entries) != 0 {
			t.Fatalf("config dir holds %d entries", len(entries))
		}
	})

	t.Run("cached apart from the full token", func(t *testing.T) {
		before := fake.calls()
		if code, _, stderr := runBinary(t, work, env, "read-only", "inventory"); code != 3 || stderr != "" {
			t.Fatalf("exit %d, stderr %q", code, stderr)
		}
		readRecord()
		if fake.calls() != before {
			t.Fatal("a cached read-only token was minted again")
		}
		full, err := newMinter(cfg).token()
		if err != nil {
			t.Fatal(err)
		}
		if full.Token != testToken {
			t.Fatalf("full token = %q", full.Token)
		}
		read, err := newReadOnlyMinter(cfg).token()
		if err != nil || read.Token != testToken+"read" {
			t.Fatalf("read-only token = %q, %v", read.Token, err)
		}
	})

	for _, tt := range []struct {
		name  string
		grant map[string]string
		want  string
	}{
		{"refuses a token with more than read", map[string]string{"metadata": "read", "issues": "write"}, "issues: write"},
		{"refuses a token with less than asked", map[string]string{"metadata": "read"}, "did not grant"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fake.mu.Lock()
			fake.grant = tt.grant
			fake.mu.Unlock()
			t.Cleanup(func() {
				fake.mu.Lock()
				fake.grant = nil
				fake.mu.Unlock()
			})
			fresh := append(append([]string{}, env...), "GH_APP_CACHE_DIR="+filepath.Join(t.TempDir(), "cache"))
			code, stdout, stderr := runBinary(t, work, fresh, "read-only", "inventory")
			if code != 1 || stdout != "" || !strings.Contains(stderr, tt.want) || strings.Contains(stderr, testToken) {
				t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
			}
			if _, err := os.Stat(record); !os.IsNotExist(err) {
				t.Fatal("the command ran with a token whose grant differs")
			}
		})
	}

	t.Run("repository listing is not needed", func(t *testing.T) {
		fake.mu.Lock()
		fake.failRepos = true
		repoCalls := fake.repoCalls
		fake.mu.Unlock()
		t.Cleanup(func() {
			fake.mu.Lock()
			fake.failRepos = false
			fake.mu.Unlock()
		})
		fresh := append(append([]string{}, env...), "GH_APP_CACHE_DIR="+filepath.Join(t.TempDir(), "cache"))
		if code, _, stderr := runBinary(t, work, fresh, "read-only", "inventory"); code != 3 || stderr != "" {
			t.Fatalf("exit %d, stderr %q", code, stderr)
		}
		readRecord()
		fake.mu.Lock()
		defer fake.mu.Unlock()
		if fake.repoCalls != repoCalls {
			t.Fatal("a read-only token listed the installation's repositories")
		}
	})

	t.Run("mint failure runs nothing", func(t *testing.T) {
		fake.mu.Lock()
		fake.failInstall = true
		fake.mu.Unlock()
		t.Cleanup(func() {
			fake.mu.Lock()
			fake.failInstall = false
			fake.mu.Unlock()
		})
		fresh := append(append([]string{}, env...), "GH_APP_CACHE_DIR="+filepath.Join(t.TempDir(), "cache"))
		code, _, stderr := runBinary(t, work, fresh, "read-only", "inventory")
		if code != 1 || !strings.Contains(stderr, "no read-only GitHub App token") {
			t.Fatalf("exit %d, stderr %q", code, stderr)
		}
		if _, err := os.Stat(record); !os.IsNotExist(err) {
			t.Fatal("the command ran without a token")
		}
	})

	t.Run("usage errors", func(t *testing.T) {
		for _, tt := range []struct {
			name string
			env  []string
			args []string
			want string
		}{
			{"no command", env, []string{"read-only", "--"}, "needs a command"},
			{"unknown command", env, []string{"read-only", "no-such-command"}, "executable file not found"},
			{"relative path", env, []string{"read-only", "./inventory"}, "absolute path"},
			{"unconfigured", []string{"RECORD=" + record, "PATH=" + bin}, []string{"read-only", "inventory"}, "needs GH_APP_ID"},
		} {
			t.Run(tt.name, func(t *testing.T) {
				code, _, stderr := runBinary(t, work, tt.env, tt.args...)
				if code != 1 || !strings.Contains(stderr, tt.want) {
					t.Fatalf("exit %d, stderr %q", code, stderr)
				}
			})
		}
		if _, err := os.Stat(record); !os.IsNotExist(err) {
			t.Fatal("a refused invocation ran the command")
		}
	})
}
