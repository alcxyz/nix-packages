package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// runReadOnly replaces gh-app with a command that reads GitHub through a
// read-only installation token of the App.
func runReadOnly(cfg config, args []string, stderr io.Writer) int {
	p, err := planReadOnly(cfg, args, os.Environ(), newReadOnlyMinter(cfg))
	if err != nil {
		fmt.Fprintf(stderr, "gh-app: %v\n", err)
		return 1
	}
	err = syscall.Exec(p.path, p.argv, p.env)
	fmt.Fprintf(stderr, "gh-app: exec %s: %v\n", p.path, err)
	return 1
}

// readOnlyDropped are the variables a read-only command does not inherit:
// other GitHub credentials, the App's settings (so a gh routed through
// gh-app passes the token through unchanged), the operator override and gh's
// host and configuration choices.
var readOnlyDropped = map[string]bool{
	"GH_TOKEN":                true,
	"GITHUB_TOKEN":            true,
	"GH_ENTERPRISE_TOKEN":     true,
	"GITHUB_ENTERPRISE_TOKEN": true,
	"GH_HOST":                 true,
	"GH_CONFIG_DIR":           true,
	"GH_APP_ID":               true,
	"GH_APP_KEY_FILE":         true,
	"GH_APP_OWNER":            true,
	"GH_APP_API_URL":          true,
	"GH_APP_CACHE_DIR":        true,
	"GH_AS_OPERATOR":          true,
}

// planReadOnly mints (or reuses) a read-only token and prepares the command
// with it in GH_TOKEN and a gh configuration directory of its own beside the
// token cache, so the operator's gh login is never used. Without a token the
// command does not run.
func planReadOnly(cfg config, args, env []string, m *minter) (ghPlan, error) {
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		return ghPlan{}, errors.New("read-only needs a command to run")
	}
	configured, err := cfg.appConfigured()
	if err != nil {
		return ghPlan{}, err
	}
	if !configured {
		return ghPlan{}, errors.New("read-only needs GH_APP_ID, GH_APP_KEY_FILE and GH_APP_OWNER")
	}
	path, err := lookPath(args[0], env)
	if err != nil {
		return ghPlan{}, err
	}
	entry, err := m.token()
	if err != nil {
		return ghPlan{}, fmt.Errorf("no read-only GitHub App token: %v", err)
	}
	configDir := filepath.Join(cfg.cacheDir, m.cacheName()+"-gh")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return ghPlan{}, fmt.Errorf("create gh configuration directory: %w", err)
	}
	var out []string
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		if !readOnlyDropped[key] {
			out = append(out, kv)
		}
	}
	out = append(out,
		"GH_TOKEN="+entry.Token,
		"GH_CONFIG_DIR="+configDir,
		"GH_PROMPT_DISABLED=1",
		"GH_NO_UPDATE_NOTIFIER=1",
	)
	return ghPlan{path: path, argv: args, env: out, withToken: true}, nil
}

// lookPath finds name on the PATH of env, not of gh-app's own environment,
// as the replaced process would.
func lookPath(name string, env []string) (string, error) {
	if strings.Contains(name, "/") {
		if !filepath.IsAbs(name) {
			return "", fmt.Errorf("%s: give a command name or an absolute path", name)
		}
		return name, nil
	}
	for _, dir := range filepath.SplitList(lookupEnv(env, "PATH")) {
		if dir == "" || !filepath.IsAbs(dir) {
			continue
		}
		candidate := filepath.Join(dir, name)
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%s: %w", name, exec.ErrNotFound)
}
