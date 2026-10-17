package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"revkit/review"
)

// config is the versioned JSON document a consumer generates (schema
// version 1). The binary ships no reviewers, repositories or lenses.
type config struct {
	SchemaVersion int               `json:"schemaVersion"`
	Reviewers     []review.Reviewer `json:"reviewers"`
	// Matcher, optional, groups findings that describe the same problem,
	// within a run and with earlier runs; without one, findings match by
	// lens and title only.
	Matcher *review.Reviewer `json:"matcher"`
	// TimeoutSeconds bounds each reviewer run.
	TimeoutSeconds int `json:"timeoutSeconds"`
	// Environment names further variables reviewers may see.
	Environment []string `json:"environment"`
	// HomeLinks names entries of the user's home linked into each Codex
	// reviewer's otherwise empty home, for tools that system-managed client
	// hooks find there; never client configuration directories.
	HomeLinks []string `json:"homeLinks"`
	// ReadablePaths are absolute paths a Codex reviewer may read besides
	// the tree and the platform minimum, for example where its tools live.
	ReadablePaths []string `json:"readablePaths"`
	// StateDir holds trees, runs and reports; default $XDG_STATE_HOME/revkit.
	StateDir     string       `json:"stateDir"`
	Repositories []repository `json:"repositories"`
}

// repository describes one reviewed repository: Context tells reviewers what
// it is, and each lens is one review job per reviewer.
type repository struct {
	Name    string `json:"name"`
	Context string `json:"context"`
	Lenses  []lens `json:"lenses"`
	// Issues, optional, is where `revkit findings file` files this
	// repository's findings.
	Issues *issueTracker `json:"issues"`
}

// issueTracker is a Forgejo or Gitea repository's issue tracker and the
// file holding the token issues are filed with.
type issueTracker struct {
	URL       string `json:"url"`
	Owner     string `json:"owner"`
	Repo      string `json:"repo"`
	TokenFile string `json:"tokenFile"`
	// Limit bounds the issues one command files; default 5.
	Limit int `json:"limit"`
}

// defaultIssueLimit bounds the issues one `findings file` command files.
const defaultIssueLimit = 5

func (t *issueTracker) limit() int {
	if t.Limit > 0 {
		return t.Limit
	}
	return defaultIssueLimit
}

var forgeNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func (t *issueTracker) validate() error {
	parsed, err := url.Parse(t.URL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return fmt.Errorf("issues: url %q must be an https URL of the forge", t.URL)
	}
	if !forgeNamePattern.MatchString(t.Owner) || !forgeNamePattern.MatchString(t.Repo) {
		return fmt.Errorf("issues: owner and repo must be plain names")
	}
	if !filepath.IsAbs(t.TokenFile) {
		return fmt.Errorf("issues: tokenFile must be an absolute path")
	}
	if t.Limit < 0 {
		return fmt.Errorf("issues: limit must not be negative")
	}
	return nil
}

type lens struct {
	Name   string `json:"name"`
	Prompt string `json:"prompt"`
}

var (
	namePattern     = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	variablePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// homeVariables are set from each reviewer's own home and cannot be passed.
var homeVariables = map[string]bool{
	"HOME": true, "TMPDIR": true, "CLAUDE_CONFIG_DIR": true, "CODEX_HOME": true,
	"XDG_CONFIG_HOME": true, "XDG_DATA_HOME": true, "XDG_STATE_HOME": true, "XDG_CACHE_HOME": true,
}

func loadConfig(path string) (*config, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	var c config
	if err := decoder.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if decoder.More() {
		return nil, fmt.Errorf("%s: trailing data after the configuration", path)
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

func (c *config) validate() error {
	if c.SchemaVersion != 1 {
		return fmt.Errorf("schemaVersion must be 1, not %d", c.SchemaVersion)
	}
	if len(c.Reviewers) == 0 {
		return fmt.Errorf("reviewers must name at least one reviewer")
	}
	seen := map[string]bool{}
	for _, r := range c.Reviewers {
		if err := r.Validate(); err != nil {
			return err
		}
		if seen[r.Name] {
			return fmt.Errorf("reviewer %s is listed twice", r.Name)
		}
		seen[r.Name] = true
	}
	if c.Matcher != nil {
		if err := c.Matcher.Validate(); err != nil {
			return fmt.Errorf("matcher: %w", err)
		}
	}
	if c.TimeoutSeconds <= 0 {
		return fmt.Errorf("timeoutSeconds must be a positive number of seconds")
	}
	for _, name := range c.Environment {
		if !variablePattern.MatchString(name) {
			return fmt.Errorf("environment: %q is not a variable name", name)
		}
		if homeVariables[name] || strings.HasPrefix(name, "XDG_") {
			return fmt.Errorf("environment: %s is set from the reviewer's own home", name)
		}
	}
	for _, name := range c.HomeLinks {
		if review.ReservedHomeNames[name] {
			return fmt.Errorf("homeLinks: %s is client configuration or the reviewer's own and stays out of reviewer homes", name)
		}
		if !review.ValidHomeLink(name) {
			return fmt.Errorf("homeLinks: %q must name one entry of the home directory", name)
		}
	}
	for _, path := range c.ReadablePaths {
		if !review.ValidReadablePath(path) {
			return fmt.Errorf("readablePaths: %q must be a clean absolute path other than / without quotes", path)
		}
	}
	if c.StateDir != "" && !filepath.IsAbs(c.StateDir) {
		return fmt.Errorf("stateDir must be an absolute path")
	}
	repositories := map[string]bool{}
	for _, repo := range c.Repositories {
		if !namePattern.MatchString(repo.Name) {
			return fmt.Errorf("repository name %q must match [A-Za-z0-9_-]+", repo.Name)
		}
		if repositories[repo.Name] {
			return fmt.Errorf("repository %s is listed twice", repo.Name)
		}
		repositories[repo.Name] = true
		if repo.Issues != nil {
			if err := repo.Issues.validate(); err != nil {
				return fmt.Errorf("repository %s: %w", repo.Name, err)
			}
		}
		if len(repo.Lenses) == 0 {
			return fmt.Errorf("repository %s has no lenses", repo.Name)
		}
		lenses := map[string]bool{}
		for _, l := range repo.Lenses {
			if !namePattern.MatchString(l.Name) {
				return fmt.Errorf("repository %s: lens name %q must match [A-Za-z0-9_-]+", repo.Name, l.Name)
			}
			if lenses[l.Name] {
				return fmt.Errorf("repository %s: lens %s is listed twice", repo.Name, l.Name)
			}
			lenses[l.Name] = true
			if strings.TrimSpace(l.Prompt) == "" {
				return fmt.Errorf("repository %s: lens %s has no prompt", repo.Name, l.Name)
			}
		}
	}
	return nil
}

func (c *config) repository(name string) (*repository, bool) {
	for i := range c.Repositories {
		if c.Repositories[i].Name == name {
			return &c.Repositories[i], true
		}
	}
	return nil, false
}

// hiddenFromCodex is the directory a Codex reviewer's permission profile
// denies besides its own temporary directory, which is inside its home; a
// variable so tests can keep their state directories in /tmp.
var hiddenFromCodex = "/tmp"

func (c *config) stateDir() (string, error) {
	if c.StateDir != "" {
		return c.StateDir, nil
	}
	if base := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(base) {
		return filepath.Join(base, "revkit"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "revkit"), nil
}

// codexCanUse reports why a Codex reviewer could not work with the trees
// and homes in dir, or "". Its profile denies /tmp, and a deny beats the
// grant for the tree; Codex also refuses to set up a home there.
func codexCanUse(dir string) string {
	if hiddenFromCodex != "" && within(dir, hiddenFromCodex) {
		return fmt.Sprintf("the state directory %s is under %s, which Codex reviewers cannot read; set stateDir or XDG_STATE_HOME elsewhere", dir, hiddenFromCodex)
	}
	return ""
}

// within reports whether path is dir or below it, comparing them with
// symbolic links resolved where they exist.
func within(path, dir string) bool {
	path, dir = resolveExisting(path), resolveExisting(dir)
	return path == dir || strings.HasPrefix(path, dir+string(filepath.Separator))
}

// resolveExisting resolves the longest existing prefix of path through
// symbolic links and keeps the rest as it is.
func resolveExisting(path string) string {
	path = filepath.Clean(path)
	rest := ""
	for {
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			return filepath.Join(resolved, rest)
		}
		parent := filepath.Dir(path)
		if parent == path {
			return filepath.Join(path, rest)
		}
		rest = filepath.Join(filepath.Base(path), rest)
		path = parent
	}
}
