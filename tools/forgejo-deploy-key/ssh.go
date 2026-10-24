package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"sopsstore"
)

// Injectable for tests.
var (
	sshTimeout   = time.Minute
	agentTimeout = 10 * time.Second
)

// agent is a private ssh-agent holding only the new key, so that ssh can
// use the key without it ever being written to a file.
type agent struct {
	dir    string // 0700 directory with the socket and the public key
	socket string
	cmd    *exec.Cmd
	done   chan error
}

// startAgent starts ssh-agent on a socket in a new private directory and
// adds the key to it on standard input. The key expires after lifetime, in
// case this process is killed before it stops the agent.
func startAgent(ctx context.Context, private, public string, lifetime time.Duration) (*agent, error) {
	dir, err := os.MkdirTemp("", "forgejo-deploy-key-")
	if err != nil {
		return nil, err
	}
	a := &agent{dir: dir, socket: filepath.Join(dir, "agent.sock"), done: make(chan error, 1)}
	if err := os.WriteFile(a.publicKeyFile(), []byte(public+"\n"), 0o600); err != nil {
		a.stop()
		return nil, err
	}

	a.cmd = exec.Command("ssh-agent", "-D", "-a", a.socket)
	// Its own process group keeps a terminal's Ctrl-C from stopping it
	// before the key is no longer needed; stop kills it.
	a.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := a.cmd.Start(); err != nil {
		a.stop()
		return nil, fmt.Errorf("ssh-agent: %v", err)
	}
	go func() { a.done <- a.cmd.Wait() }()

	deadline := time.Now().Add(agentTimeout)
	for {
		if _, err := os.Stat(a.socket); err == nil {
			break
		}
		select {
		case err := <-a.done:
			a.done <- err
			a.stop()
			return nil, errors.New("ssh-agent exited before it was ready")
		case <-ctx.Done():
			a.stop()
			return nil, errors.New("interrupted")
		case <-time.After(50 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			a.stop()
			return nil, fmt.Errorf("ssh-agent did not start within %v", agentTimeout)
		}
	}

	addCtx, cancel := context.WithTimeout(ctx, agentTimeout)
	defer cancel()
	seconds := strconv.Itoa(int(lifetime / time.Second))
	add := sopsstore.BoundedCommand(addCtx, "ssh-add", "-q", "-t", seconds, "-")
	add.Env = append(withoutAuthSock(os.Environ()), "SSH_AUTH_SOCK="+a.socket)
	add.Stdin = strings.NewReader(private)
	// ssh-add's output is withheld: it is about the key alone.
	if err := sopsstore.RunError(addCtx, "ssh-add", agentTimeout, add.Run()); err != nil {
		a.stop()
		return nil, err
	}
	return a, nil
}

func (a *agent) publicKeyFile() string {
	return filepath.Join(a.dir, "id.pub")
}

// stop kills the agent, which drops the key, and removes its directory.
func (a *agent) stop() {
	if a.cmd != nil && a.cmd.Process != nil {
		syscall.Kill(-a.cmd.Process.Pid, syscall.SIGKILL)
		select {
		case <-a.done:
		case <-time.After(agentTimeout):
		}
	}
	os.RemoveAll(a.dir)
}

func withoutAuthSock(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if !strings.HasPrefix(kv, "SSH_AUTH_SOCK=") {
			out = append(out, kv)
		}
	}
	return out
}

// transport is how to reach Forgejo's SSH server. It comes from the
// user's ssh configuration (ssh -G), so that a host alias, port or proxy
// configured there still works, but nothing else from that configuration
// is used: no other identity, agent or host key policy.
type transport struct {
	alias       string // the host as given, which %n expands to
	host        string // the host name ssh resolved it to
	port        string
	user        string
	proxy       []string // ProxyCommand or ProxyJump options, if any
	hostKeyOpts []string
}

// resolveTransport asks ssh how it would connect to host. A non-empty
// knownHosts replaces the configured known hosts files.
func resolveTransport(ctx context.Context, host string, port int, user, knownHosts string) (transport, error) {
	args := []string{"-G", "-l", user}
	if port != 0 {
		args = append(args, "-p", strconv.Itoa(port))
	}
	args = append(args, host)
	ctx, cancel := context.WithTimeout(ctx, sshTimeout)
	defer cancel()
	var out bytes.Buffer
	cmd := sopsstore.BoundedCommand(ctx, "ssh", args...)
	cmd.Stdout = &out
	if err := sopsstore.RunError(ctx, "ssh -G", sshTimeout, cmd.Run()); err != nil {
		return transport{}, err
	}

	config := map[string]string{}
	scanner := bufio.NewScanner(&out)
	for scanner.Scan() {
		name, value, ok := strings.Cut(scanner.Text(), " ")
		if ok {
			if _, seen := config[name]; !seen {
				config[name] = value
			}
		}
	}
	t := transport{alias: host, host: config["hostname"], port: config["port"], user: user}
	if t.host == "" || t.port == "" {
		return transport{}, fmt.Errorf("ssh -G %s gave no hostname or port", host)
	}
	proxy, err := proxyCommand(config["proxycommand"], config["proxyjump"], os.Getenv("SSH_AUTH_SOCK"), os.Getenv("SHELL"))
	if err != nil {
		return transport{}, err
	}
	if proxy != "" {
		t.proxy = []string{"-o", "ProxyCommand=" + proxy}
	}
	if v := config["hostkeyalias"]; v != "" && v != "none" {
		t.hostKeyOpts = append(t.hostKeyOpts, "-o", "HostKeyAlias="+v)
	}
	if knownHosts != "" {
		t.hostKeyOpts = append(t.hostKeyOpts,
			"-o", "UserKnownHostsFile="+optionValue(knownHosts),
			"-o", "GlobalKnownHostsFile=/dev/null")
	} else {
		if v := config["userknownhostsfile"]; v != "" {
			t.hostKeyOpts = append(t.hostKeyOpts, "-o", "UserKnownHostsFile="+v)
		}
		if v := config["globalknownhostsfile"]; v != "" {
			t.hostKeyOpts = append(t.hostKeyOpts, "-o", "GlobalKnownHostsFile="+v)
		}
	}
	return t, nil
}

// jumpHopPattern is one ProxyJump hop as ssh -G prints it:
// [ssh://][USER@]HOST[:PORT], where a numeric HOST is in brackets.
var jumpHopPattern = regexp.MustCompile(`^(?:ssh://)?(?:([A-Za-z0-9_][A-Za-z0-9_.-]*)@)?(?:\[([0-9A-Fa-f:.]+)\]|([A-Za-z0-9_][A-Za-z0-9_.-]*))(?::([0-9]{1,5}))?$`)

// proxyCommand is the ProxyCommand for the configured proxy, or "" for
// none. It runs with the user's own ssh configuration and agent, as it
// would without this tool: ssh gives a proxy command its environment, in
// which SSH_AUTH_SOCK is the private agent's socket, and runs a ProxyJump
// hop with its own -F /dev/null. So the proxy gets userAuthSock back, a
// configured command runs in shell, the user's shell that ssh would use,
// and a ProxyJump becomes an explicit ssh -W through the same hops.
func proxyCommand(command, jump, userAuthSock, shell string) (string, error) {
	env := "env -u SSH_AUTH_SOCK "
	if userAuthSock != "" {
		env = "env SSH_AUTH_SOCK=" + shellQuote(percentEscape(userAuthSock)) + " "
	}
	if command != "" && command != "none" {
		if shell == "" {
			shell = "/bin/sh"
		}
		// The command keeps its own %h and %p, which ssh expands.
		return env + shellQuote(percentEscape(shell)) + " -c " + shellQuote(command), nil
	}
	if jump == "" || jump == "none" {
		return "", nil
	}
	hops := strings.Split(jump, ",")
	var last []string
	for _, hop := range hops {
		last = jumpHopPattern.FindStringSubmatch(hop)
		if last == nil {
			return "", fmt.Errorf("ssh -G gave a ProxyJump this command cannot pass on (%s); give --ssh-host a host reached without it", printable(jump))
		}
	}
	// The last hop, as ssh itself would run it; the earlier ones are
	// passed on with -J, which takes them in the same form.
	proxy := env + "ssh -o BatchMode=yes "
	if user := last[1]; user != "" {
		proxy += "-l " + shellQuote(user) + " "
	}
	if port := last[4]; port != "" {
		proxy += "-p " + port + " "
	}
	if len(hops) > 1 {
		proxy += "-J " + shellQuote(strings.Join(hops[:len(hops)-1], ",")) + " "
	}
	return proxy + "-W '[%h]:%p' -- " + shellQuote(last[2]+last[3]), nil
}

// shellQuote quotes s as one word for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// optionValue is a path as one ssh option value: ssh splits values at
// spaces and quotes, and expands % in these paths.
func optionValue(path string) string {
	quoted := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(percentEscape(path))
	return `"` + quoted + `"`
}

// percentEscape keeps ssh from expanding % in a literal value.
func percentEscape(s string) string {
	return strings.ReplaceAll(s, "%", "%%")
}

// sshArgs is an ssh command line that authenticates with the agent's key
// alone and accepts only an already known host key.
func (t transport) sshArgs(a *agent, remote ...string) []string {
	args := []string{
		"-F", "/dev/null",
		"-T",
		"-o", "BatchMode=yes",
		"-o", "IdentityAgent=" + optionValue(a.socket),
		"-o", "IdentityFile=" + optionValue(a.publicKeyFile()),
		"-o", "IdentitiesOnly=yes",
		"-o", "PreferredAuthentications=publickey",
		"-o", "PasswordAuthentication=no",
		"-o", "KbdInteractiveAuthentication=no",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "UpdateHostKeys=no",
		"-o", "CheckHostIP=no",
		"-o", "ForwardAgent=no",
		"-o", "ClearAllForwardings=yes",
		"-o", "ControlMaster=no",
		"-o", "ControlPath=none",
		"-o", "ConnectTimeout=20",
	}
	args = append(args, t.hostKeyOpts...)
	args = append(args, t.proxy...)
	// The resolved host name, with the host as given as the destination,
	// so that a proxy command's %h and %n expand as they do in plain ssh.
	args = append(args, "-o", "HostName="+percentEscape(t.host))
	args = append(args, "-p", t.port, "-l", t.user, "--", t.alias)
	return append(args, remote...)
}

type probeState int

const (
	probeNotRegistered probeState = iota // ssh refused the key
	probeReadOnly                        // a deploy key, but no write access
	probeTransient                       // any other failure; try again
	probeWritable                        // a deploy key that may push
	probeUserKey                         // the key belongs to a user account
	probeHostKey                         // the host key is unknown or wrong
)

type probeResult struct {
	state   probeState
	keyName string
	message string // one printable line from ssh or Forgejo
}

var deployGreeting = regexp.MustCompile(`successfully authenticated with the deploy key named (.*?), but `)

// probe checks the key in two steps. ssh without a command makes Forgejo
// say whether the key is a deploy key or a user's key. git-receive-pack,
// given only a flush packet, then makes Forgejo check write access, as
// git push --dry-run would, and exits without changing anything.
func probe(ctx context.Context, t transport, a *agent, repo string) probeResult {
	stdout, stderr, err := runSSH(ctx, t.sshArgs(a), "")
	if r, failed := t.sshFailure(stderr, err); failed {
		return r
	}
	greeting := string(stdout)
	match := deployGreeting.FindStringSubmatch(greeting)
	if match == nil {
		if strings.Contains(greeting, "successfully authenticated with the key named") {
			return probeResult{state: probeUserKey}
		}
		return probeResult{state: probeTransient, message: "unexpected answer: " + lastLine(stdout, stderr)}
	}
	name := printable(match[1])

	stdout, stderr, err = runSSH(ctx, t.sshArgs(a, "git-receive-pack '"+repo+".git'"), "0000")
	if r, failed := t.sshFailure(stderr, err); failed {
		r.keyName = name
		return r
	}
	if err != nil {
		// Forgejo (and Gitea) prefix every refusal with its name; only
		// "Deploy Key: ID:NAME is not authorized to write" means that this
		// repository's deploy key lacks write access. "Public (Deploy) Key"
		// is a deploy key of another repository.
		if forgejoRefusal.Match(stderr) {
			if readOnlyRefusal.Match(stderr) {
				return probeResult{state: probeReadOnly, keyName: name, message: lastLine(stderr)}
			}
			return probeResult{state: probeTransient, keyName: name, message: "Forgejo refused git-receive-pack: " + lastLine(stderr)}
		}
		return probeResult{state: probeTransient, keyName: name, message: "git-receive-pack failed: " + lastLine(stderr)}
	}
	if !pktLine.Match(stdout) {
		return probeResult{state: probeTransient, keyName: name, message: "git-receive-pack gave no reference advertisement"}
	}
	return probeResult{state: probeWritable, keyName: name}
}

var (
	pktLine         = regexp.MustCompile(`^[0-9a-f]{4}`)
	forgejoRefusal  = regexp.MustCompile(`(Forgejo|Gitea): `)
	readOnlyRefusal = regexp.MustCompile(`(Forgejo|Gitea): Deploy Key: [0-9]+:.* is not authorized to `)
)

func runSSH(ctx context.Context, args []string, input string) ([]byte, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, sshTimeout)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := sopsstore.BoundedCommand(ctx, "ssh", args...)
	cmd.Env = withoutAuthSock(os.Environ())
	cmd.Stdin = strings.NewReader(input)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		err = sopsstore.RunError(ctx, "ssh", sshTimeout, err)
	}
	return stdout.Bytes(), stderr.Bytes(), err
}

// sshFailure classifies ssh's own failures (exit status 255): its
// connection, the host key or authentication. Only ssh's own refusal of
// the key, "USER@HOST: Permission denied", means that Forgejo does not
// know it: a proxy's refusal names another user or host.
func (t transport) sshFailure(stderr []byte, err error) (probeResult, bool) {
	var exitErr *exec.ExitError
	if err == nil || (errors.As(err, &exitErr) && exitErr.ExitCode() != 255) {
		return probeResult{}, false
	}
	text := string(stderr)
	switch {
	case strings.Contains(text, "Host key verification failed"),
		strings.Contains(text, "REMOTE HOST IDENTIFICATION HAS CHANGED"):
		return probeResult{state: probeHostKey, message: lastLine(stderr)}, true
	case t.deniedKey(text):
		return probeResult{state: probeNotRegistered}, true
	}
	if exitErr == nil {
		return probeResult{state: probeTransient, message: err.Error()}, true
	}
	return probeResult{state: probeTransient, message: lastLine(stderr)}, true
}

func (t transport) deniedKey(stderr string) bool {
	prefix := strings.ToLower(t.user + "@" + t.host + ": Permission denied")
	for _, line := range strings.Split(stderr, "\n") {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), prefix) {
			return true
		}
	}
	return false
}

// lastLine is the last non-empty line of the given outputs, made printable.
func lastLine(outputs ...[]byte) string {
	var last string
	for _, out := range outputs {
		for _, line := range strings.Split(string(out), "\n") {
			if line = printable(line); strings.TrimSpace(line) != "" {
				last = strings.TrimSpace(line)
			}
		}
	}
	return last
}

// printable keeps a server-supplied string to one short printable line.
func printable(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsPrint(r) {
			b.WriteRune(r)
		}
		if b.Len() >= 200 {
			break
		}
	}
	return b.String()
}
