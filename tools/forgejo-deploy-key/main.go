// forgejo-deploy-key creates a write deploy key for one Forgejo repository
// and stores its private half in a new SOPS file without showing it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"sopsstore"
)

const usage = `usage: forgejo-deploy-key --repo OWNER/NAME --forgejo URL --sops-file PATH --sops-key NAME [options]

Creates an ed25519 key in memory, waits until it is a deploy key with write
access on the repository, and then writes its private half to a new SOPS
file. Only the public key, its fingerprint and the file written are
printed. Run it from the repository holding .sops.yaml, with a path
relative to it.

  --repo OWNER/NAME   the Forgejo repository the key may push to
  --forgejo URL       Forgejo's web address, for the settings link
  --sops-file PATH    the new SOPS file (.yaml or .yml); it must not exist
  --sops-key NAME     the key's name in the file (letters, digits, _)
  --title TITLE       the deploy key's title and comment (default: the
                      SOPS file's name without its extension)
  --ssh-host HOST     Forgejo's SSH host (default: the host of --forgejo)
  --ssh-port PORT     its port (default: from the ssh configuration, or 22)
  --ssh-user USER     its user (default: git)
  --known-hosts FILE  the only known hosts file to trust (default: the
                      ones the ssh configuration names); the host key must
                      already be known
  --wait DURATION     how long to wait for the deploy key (default: 30m)

The operator adds the printed public key in the repository's settings
(Deploy keys, with write access). The command checks every few seconds,
with only that key and a known host key, that Forgejo accepts it as a
deploy key and grants it write access (git-receive-pack, without pushing
anything), and only then encrypts the private key with sops encrypt.
`

// Injectable for tests.
var pollInterval = 10 * time.Second

var (
	repoPattern  = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	hostPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]*$`)
	userPattern  = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)
	titlePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 _.@:/-]{0,49}$`)
)

type options struct {
	repo       string
	forgejo    string
	sopsFile   string
	sopsKey    string
	title      string
	sshHost    string
	sshPort    int
	sshUser    string
	knownHosts string
	wait       time.Duration
}

func parseArgs(args []string) (options, error) {
	var opts options
	fs := flag.NewFlagSet("forgejo-deploy-key", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&opts.repo, "repo", "", "")
	fs.StringVar(&opts.forgejo, "forgejo", "", "")
	fs.StringVar(&opts.sopsFile, "sops-file", "", "")
	fs.StringVar(&opts.sopsKey, "sops-key", "", "")
	fs.StringVar(&opts.title, "title", "", "")
	fs.StringVar(&opts.sshHost, "ssh-host", "", "")
	fs.IntVar(&opts.sshPort, "ssh-port", 0, "")
	fs.StringVar(&opts.sshUser, "ssh-user", "git", "")
	fs.StringVar(&opts.knownHosts, "known-hosts", "", "")
	fs.DurationVar(&opts.wait, "wait", 30*time.Minute, "")
	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	if fs.NArg() > 0 {
		return opts, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	if !repoPattern.MatchString(opts.repo) || strings.HasSuffix(opts.repo, ".git") ||
		strings.Contains(opts.repo, "..") || strings.HasPrefix(opts.repo, ".") {
		return opts, errors.New("--repo must be OWNER/NAME")
	}
	base, err := url.Parse(opts.forgejo)
	if opts.forgejo == "" || err != nil || (base.Scheme != "https" && base.Scheme != "http") || base.Host == "" ||
		base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return opts, errors.New("--forgejo must be Forgejo's web address, such as https://forgejo.example.org")
	}
	opts.forgejo = strings.TrimRight(opts.forgejo, "/")
	if opts.sopsFile == "" || opts.sopsKey == "" {
		return opts, errors.New("--sops-file and --sops-key are required")
	}
	if !sopsstore.KeyPattern.MatchString(opts.sopsKey) {
		return opts, errors.New("--sops-key may only contain letters, digits and underscores")
	}
	if err := sopsstore.ValidPath(opts.sopsFile); err != nil {
		return opts, fmt.Errorf("--sops-file %v", err)
	}
	if opts.title == "" {
		opts.title = strings.TrimSuffix(filepath.Base(opts.sopsFile), filepath.Ext(opts.sopsFile))
	}
	if !titlePattern.MatchString(opts.title) {
		return opts, errors.New("--title must be at most 50 letters, digits, spaces and _.@:/-, starting with a letter or digit")
	}
	if opts.sshHost == "" {
		opts.sshHost = base.Hostname()
	}
	if !hostPattern.MatchString(opts.sshHost) {
		return opts, errors.New("--ssh-host is not a valid host name")
	}
	if opts.sshPort < 0 || opts.sshPort > 65535 {
		return opts, errors.New("--ssh-port must be between 1 and 65535")
	}
	if !userPattern.MatchString(opts.sshUser) {
		return opts, errors.New("--ssh-user is not a valid user name")
	}
	if opts.wait <= 0 {
		return opts, errors.New("--wait must be positive")
	}
	return opts, nil
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	opts, err := parseArgs(args)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprint(stdout, usage)
		return 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "forgejo-deploy-key: %v\n", err)
		return 2
	}
	// The programs run in their own process groups, which the terminal's
	// Ctrl-C and hangup do not reach: either ends ctx, which kills them.
	// A hangup that is ignored, as under nohup, stays ignored.
	signals := []os.Signal{os.Interrupt, syscall.SIGTERM}
	if !signal.Ignored(syscall.SIGHUP) {
		signals = append(signals, syscall.SIGHUP)
	}
	ctx, stop := signal.NotifyContext(context.Background(), signals...)
	defer stop()
	if err := provision(ctx, opts, stdout); err != nil {
		fmt.Fprintf(stderr, "forgejo-deploy-key: %v\n", err)
		return 1
	}
	return 0
}

// provision checks everything it can before the key exists, then creates
// it, waits for it to be registered with write access, and stores it.
func provision(ctx context.Context, opts options, stdout io.Writer) error {
	store := sopsstore.File{Path: opts.sopsFile, Key: opts.sopsKey}
	if err := store.Check(ctx); err != nil {
		return err
	}
	for _, program := range []string{"ssh", "ssh-agent", "ssh-add"} {
		if _, err := exec.LookPath(program); err != nil {
			return fmt.Errorf("%s: %v", program, err)
		}
	}
	if opts.knownHosts != "" {
		if _, err := os.Stat(opts.knownHosts); err != nil {
			return fmt.Errorf("--known-hosts: %v", err)
		}
	}
	t, err := resolveTransport(ctx, opts.sshHost, opts.sshPort, opts.sshUser, opts.knownHosts)
	if err != nil {
		return err
	}

	key, err := newDeployKey(opts.title)
	if err != nil {
		return err
	}
	private, err := key.openSSHPrivateKey()
	if err != nil {
		return err
	}
	a, err := startAgent(ctx, private, key.authorizedKey(), opts.wait+10*time.Minute)
	if err != nil {
		return err
	}
	defer a.stop()
	// Before anyone sees the key, check that the server is reachable, its
	// host key is known and it refuses the new key.
	r := probe(ctx, t, a, opts.repo)
	if ctx.Err() != nil {
		return errors.New("interrupted; nothing was stored")
	}
	switch r.state {
	case probeNotRegistered:
	case probeHostKey:
		return hostKeyError(opts, r)
	case probeTransient:
		return fmt.Errorf("could not check the key with %s: %s", opts.sshHost, r.message)
	default:
		return fmt.Errorf("%s accepted a key that was just created; check --ssh-host", opts.sshHost)
	}

	fmt.Fprintf(stdout, "public key:   %s\n", key.authorizedKey())
	fmt.Fprintf(stdout, "fingerprint:  %s\n", key.fingerprint())
	fmt.Fprintf(stdout, "Add it at %s/%s/settings/keys: Add deploy key, title %q, paste the public key, "+
		"tick write access and save. Waiting up to %v.\n", opts.forgejo, opts.repo, opts.title, opts.wait)

	name, err := waitForWriteAccess(ctx, t, a, opts, stdout)
	if err != nil {
		return fmt.Errorf("%v; nothing was stored. If you added the key, delete it (%s)", err, key.fingerprint())
	}
	fmt.Fprintf(stdout, "verified:     deploy key %q can push to %s\n", name, opts.repo)

	if err := store.Save(ctx, private); err != nil {
		return fmt.Errorf("the deploy key works, but storing its private key failed: %v; delete the deploy key (%s) and run this again", err, key.fingerprint())
	}
	fmt.Fprintf(stdout, "key:          stored in %s\n", store.Describe())
	return nil
}

// waitForWriteAccess probes until the key is a deploy key with write
// access, and reports each new state once.
func waitForWriteAccess(ctx context.Context, t transport, a *agent, opts options, stdout io.Writer) (string, error) {
	// The wait bounds the probes too: a probe still running when it ends
	// counts for nothing, even if it would have succeeded.
	waitCtx, cancel := context.WithTimeout(ctx, opts.wait)
	defer cancel()
	ended := func() error {
		if ctx.Err() != nil {
			return errors.New("interrupted")
		}
		return fmt.Errorf("timed out after %v waiting for the deploy key", opts.wait)
	}
	last := probeResult{state: probeNotRegistered}
	for {
		r := probe(waitCtx, t, a, opts.repo)
		if waitCtx.Err() != nil {
			return "", ended()
		}
		switch r.state {
		case probeWritable:
			return r.keyName, nil
		case probeUserKey:
			return "", errors.New("the key was added to a user account, not as the repository's deploy key; remove it from that account's SSH keys")
		case probeHostKey:
			return "", hostKeyError(opts, r)
		}
		if r != last {
			switch r.state {
			case probeReadOnly:
				fmt.Fprintf(stdout, "waiting:      deploy key %q has no write access (%s); delete it and add it again with write access\n", r.keyName, r.message)
			case probeTransient:
				fmt.Fprintf(stdout, "waiting:      %s\n", r.message)
			case probeNotRegistered:
				fmt.Fprintln(stdout, "waiting:      the key is not registered yet")
			}
			last = r
		}
		select {
		case <-waitCtx.Done():
			return "", ended()
		case <-time.After(pollInterval):
		}
	}
}

func hostKeyError(opts options, r probeResult) error {
	return fmt.Errorf("the SSH host key of %s is not known or does not match (%s); pin it in known_hosts or give --known-hosts", opts.sshHost, r.message)
}
