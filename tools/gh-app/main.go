// Command gh-app routes gh invocations and Git credentials for the
// repositories a GitHub App is installed on through the App's installation
// token, and leaves every other repository on the operator's own gh login.
// It records who acted; it is not a security boundary.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

const usage = `usage: gh-app <command> [args...]

Commands:
  gh [args...]                  run gh, with the App's installation token for
                                installed repositories of GH_APP_OWNER
  credential <get|store|erase>  Git credential helper for those repositories
                                (needs credential.useHttpPath=true)
  status                        show the installation, token expiry and
                                installed repositories
  help                          show this help

Environment:
  GH_APP_ID          numeric GitHub App ID
  GH_APP_KEY_FILE    path to the App's PEM private key (PKCS#1 or PKCS#8 RSA)
  GH_APP_OWNER       account the App is installed on
  GH_APP_GH          absolute path of the real gh binary
  GH_APP_API_URL     API base URL (default https://api.github.com)
  GH_APP_CACHE_DIR   token cache directory (default $XDG_RUNTIME_DIR/gh-app)
  GH_AS_OPERATOR=1   run this gh command as the operator, without the App

Without GH_APP_ID, GH_APP_KEY_FILE and GH_APP_OWNER, gh runs unchanged and
the credential helper answers nothing; setting only some of them is an error.
With them, give flags after the command, and for the commands that use the
App after the subcommand too (gh <command> <subcommand> [flags]); only
-R/--repo, --hostname, --help and --version may come earlier, and anything
else there is refused, since a flag's value could be read as the command.
Only pr, issue, release, run, workflow, label, cache, api and some repo
subcommands use the App; every other command runs as the operator, as do
commands that also act on a second repository (issue transfer, label clone,
repo sync --source, issue develop --branch-repo, pr create --head OWNER:BRANCH,
issue create/edit relationship flags such as --parent, or issue close
--duplicate-of, naming another repository), pr and issue commands about the
viewer (status, and @me as a user filter in --author, --assignee,
--reviewer, --mention or --search), which an installation token cannot
answer, and local Git work that only reads (repo clone, pr checkout, a repo
sync without a repository), since gh's Git may fetch other repositories with
the token. In a checkout with submodules, pr create and merge and issue
develop run as the operator too, since Git may fetch or push the submodules.
pr create without --head also runs as the operator whenever the checkout has
a github.com remote of another owner, or a remote whose host is unknown, even
if the current branch tracks the installed repository.

SSH remotes on other hosts are resolved with ssh -G, so host aliases work;
when ssh cannot resolve one that gh might choose, gh runs as the operator.

Limit: with GH_HOST unset gh-app assumes github.com, where gh may fall back
to another configured host.
`

var errPartialConfig = errors.New("GH_APP_ID, GH_APP_KEY_FILE and GH_APP_OWNER must be set together")

var (
	ownerPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)
	appIDPattern = regexp.MustCompile(`^[0-9]+$`)
)

// config is the environment-derived configuration. Only the fields a command
// needs are validated, so routing to another owner never requires the App's
// key or cache directory.
type config struct {
	appID    string
	keyFile  string
	owner    string
	ghPath   string
	apiURL   string
	cacheDir string
}

func readConfig(getenv func(string) string) config {
	cfg := config{
		appID:    strings.TrimSpace(getenv("GH_APP_ID")),
		keyFile:  getenv("GH_APP_KEY_FILE"),
		owner:    strings.TrimSpace(getenv("GH_APP_OWNER")),
		ghPath:   getenv("GH_APP_GH"),
		apiURL:   strings.TrimRight(getenv("GH_APP_API_URL"), "/"),
		cacheDir: getenv("GH_APP_CACHE_DIR"),
	}
	if cfg.apiURL == "" {
		cfg.apiURL = "https://api.github.com"
	}
	if cfg.cacheDir == "" {
		if runtimeDir := getenv("XDG_RUNTIME_DIR"); runtimeDir != "" {
			cfg.cacheDir = filepath.Join(runtimeDir, "gh-app")
		}
	}
	return cfg
}

// appConfigured reports whether the App is configured. Without any of its
// settings gh-app passes everything through; a partial setup is an error.
func (cfg config) appConfigured() (bool, error) {
	set := 0
	for _, value := range []string{cfg.appID, cfg.keyFile, cfg.owner} {
		if value != "" {
			set++
		}
	}
	switch set {
	case 0:
		return false, nil
	case 3:
		return true, nil
	}
	return false, errPartialConfig
}

func (cfg config) checkOwner() error {
	if cfg.owner == "" {
		return errors.New("GH_APP_OWNER is not set")
	}
	if !ownerPattern.MatchString(cfg.owner) {
		return errors.New("GH_APP_OWNER is not a valid GitHub account name")
	}
	return nil
}

func (cfg config) checkGH() error {
	if cfg.ghPath == "" {
		return errors.New("GH_APP_GH is not set")
	}
	if !filepath.IsAbs(cfg.ghPath) {
		return errors.New("GH_APP_GH must be an absolute path")
	}
	return nil
}

// checkMint validates what minting a token needs.
func (cfg config) checkMint() error {
	if err := cfg.checkOwner(); err != nil {
		return err
	}
	if cfg.appID == "" {
		return errors.New("GH_APP_ID is not set")
	}
	if !appIDPattern.MatchString(cfg.appID) {
		return errors.New("GH_APP_ID must be numeric")
	}
	if cfg.keyFile == "" {
		return errors.New("GH_APP_KEY_FILE is not set")
	}
	if cfg.cacheDir == "" {
		return errors.New("neither GH_APP_CACHE_DIR nor XDG_RUNTIME_DIR is set")
	}
	if !filepath.IsAbs(cfg.cacheDir) {
		return errors.New("the cache directory must be an absolute path")
	}
	return nil
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stdout, usage)
		return 0
	}
	cfg := readConfig(os.Getenv)
	switch args[0] {
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	case "gh":
		return runGH(cfg, args[1:], stderr)
	case "credential":
		return runCredential(cfg, args[1:], stdin, stdout, stderr)
	case "status":
		return runStatus(cfg, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "gh-app: unknown command %q\n", args[0])
		fmt.Fprint(stderr, usage)
		return 2
	}
}

func runGH(cfg config, args []string, stderr io.Writer) int {
	dir, err := os.Getwd()
	if err != nil {
		dir = ""
	}
	p, err := planGH(cfg, args, os.Environ(), dir, newMinter(cfg))
	if err != nil {
		fmt.Fprintf(stderr, "gh-app: %v\n", err)
		return 1
	}
	err = syscall.Exec(p.path, p.argv, p.env)
	fmt.Fprintf(stderr, "gh-app: exec %s: %v\n", p.path, err)
	return 1
}

func runStatus(cfg config, stdout, stderr io.Writer) int {
	if err := cfg.checkMint(); err != nil {
		fmt.Fprintf(stderr, "gh-app: %v\n", err)
		return 1
	}
	m := newMinter(cfg)
	entry, err := m.token()
	if err != nil {
		fmt.Fprintf(stderr, "gh-app: %v\n", err)
		return 1
	}
	if entry.ListedAt.IsZero() {
		// Retry an unconfirmed listing once the re-list interval has passed,
		// as repository commands do.
		if listed, _, err := m.tokenFor(cfg.owner, ""); err == nil {
			entry = listed
		}
	}
	if entry.ListedAt.IsZero() {
		fmt.Fprintln(stderr, "gh-app: the installation's repositories have not been listed yet")
		return 1
	}
	fmt.Fprintf(stdout, "app id:          %s\n", cfg.appID)
	fmt.Fprintf(stdout, "owner:           %s\n", cfg.owner)
	fmt.Fprintf(stdout, "installation id: %d\n", entry.InstallationID)
	fmt.Fprintf(stdout, "token expires:   %s\n", entry.ExpiresAt.Local().Format("2006-01-02 15:04:05 MST"))
	fmt.Fprintf(stdout, "repositories:    %d\n", len(entry.Repositories))
	for _, name := range entry.Repositories {
		fmt.Fprintf(stdout, "  %s\n", name)
	}
	return 0
}
