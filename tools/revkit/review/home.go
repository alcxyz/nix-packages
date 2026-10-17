package review

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Codex reads AGENTS.md, config, skills and memories from CODEX_HOME even
// with --ignore-user-config, and skills and plugins from ~/.agents, so a
// Codex reviewer gets a home of its own. Its login comes from auth.json in
// CODEX_HOME, which Codex rewrites in place on a refresh, through a link.
//
// Claude Code keeps its own: --restricted already skips the user's CLAUDE.md
// and settings and keeps the file tools in the tree, while a refresh
// replaces .credentials.json by renaming a new file over it, which would
// break a link and leave the user's file with a used refresh token (and on
// macOS the login is in the keychain, keyed by the configuration directory).
const codexCredentials = "auth.json"

// ReservedHomeNames are entries of a home that hold a client's
// instructions, skills, settings, plugins or memories, or that the home
// uses itself; a home link may not name them.
var ReservedHomeNames = map[string]bool{
	".claude": true, ".claude.json": true, ".codex": true, ".agents": true,
	".config": true, ".local": true, ".cache": true,
	// The reviewer's own TMPDIR.
	"tmp": true,
}

// Home is a Codex reviewer's temporary home directory. It holds a link to
// the user's Codex login and the entries named by the consumer's home links,
// so Codex starts without the user's instructions, skills, memories,
// settings, plugins or MCP servers.
type Home struct {
	Dir string
	// link is the login link in the home and source the user's file.
	link   string
	source string
}

// NewHome creates dir as a Codex home, linking the login Codex uses under
// environ. A missing login is not an error: Codex may sign in from an
// allowed environment variable instead, and otherwise fails, which counts as
// a missing review. links names entries of the user's home to link in as
// well, for tools that system-managed client hooks find there (for example
// a Nix profile); missing ones are skipped.
func NewHome(dir string, environ, links []string) (*Home, error) {
	if err := os.MkdirAll(filepath.Join(dir, ".codex"), 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dir, "tmp"), 0o700); err != nil {
		return nil, err
	}
	home := &Home{Dir: dir}

	userHome := lookup(environ, "HOME")
	if filepath.IsAbs(userHome) {
		for _, name := range links {
			if !ValidHomeLink(name) {
				return nil, fmt.Errorf("cannot link %q into a reviewer's home", name)
			}
			source := filepath.Join(userHome, name)
			if _, err := os.Lstat(source); err != nil {
				continue
			}
			if err := os.Symlink(source, filepath.Join(dir, name)); err != nil {
				return nil, err
			}
		}
	}

	configDir := lookup(environ, "CODEX_HOME")
	if !filepath.IsAbs(configDir) {
		if !filepath.IsAbs(userHome) {
			return home, nil
		}
		configDir = filepath.Join(userHome, ".codex")
	}
	source, err := filepath.EvalSymlinks(filepath.Join(configDir, codexCredentials))
	if err != nil {
		return home, nil
	}
	if info, err := os.Stat(source); err != nil || !info.Mode().IsRegular() {
		return home, nil
	}
	link := filepath.Join(dir, ".codex", codexCredentials)
	if err := os.Symlink(source, link); err != nil {
		return nil, err
	}
	home.link, home.source = link, source
	return home, nil
}

// ValidHomeLink reports whether name may be linked into a reviewer's home:
// one entry of the home directory, not a client's configuration.
func ValidHomeLink(name string) bool {
	return name != "" && name != "." && name != ".." && name == filepath.Base(name) && !ReservedHomeNames[name]
}

// Environment returns the variables that point Codex and its children at
// the home.
func (h *Home) Environment() []string {
	return []string{
		"HOME=" + h.Dir,
		"CODEX_HOME=" + filepath.Join(h.Dir, ".codex"),
		"XDG_CONFIG_HOME=" + filepath.Join(h.Dir, ".config"),
		"XDG_DATA_HOME=" + filepath.Join(h.Dir, ".local", "share"),
		"XDG_STATE_HOME=" + filepath.Join(h.Dir, ".local", "state"),
		"XDG_CACHE_HOME=" + filepath.Join(h.Dir, ".cache"),
		"TMPDIR=" + filepath.Join(h.Dir, "tmp"),
	}
}

// ConfigDir is Codex's configuration directory inside the home.
func (h *Home) ConfigDir() string {
	return filepath.Join(h.Dir, ".codex")
}

// CheckLogin returns a note if Codex replaced the login link with a file of
// its own instead of writing through it, which would leave the user's login
// behind a refresh. The file is not read or moved.
func (h *Home) CheckLogin() string {
	if h.link == "" {
		return ""
	}
	info, err := os.Lstat(h.link)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		return ""
	}
	return fmt.Sprintf("codex replaced the link to %s instead of writing through it; if codex asks you to log in, log in again", h.source)
}

// Remove deletes the home.
func (h *Home) Remove() error {
	return os.RemoveAll(h.Dir)
}

func lookup(environ []string, name string) string {
	value := ""
	for _, entry := range environ {
		if key, v, ok := strings.Cut(entry, "="); ok && key == name {
			value = v
		}
	}
	return value
}
