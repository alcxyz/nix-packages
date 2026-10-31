// desk starts and ends a remote-desk session on a host in one step: an SSH
// tunnel to the host's loopback VNC server, `remote-desk on`, a VNC viewer
// on the forwarded port and, when the viewer exits, `remote-desk off` and
// the tunnel's close.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const usage = `usage: desk [options] HOST [-- VIEWER-OPTIONS...]
       desk [options] off|status HOST

Starts a remote session on HOST and ends it when the viewer exits:

 1. opens an SSH tunnel from 127.0.0.1:PORT to the host's loopback VNC
    server, through HOST-lan first when the SSH configuration defines that
    name and through HOST otherwise or when HOST-lan does not answer;
 2. runs remote-desk on, which moves the host's desktop to a virtual output
    and turns its displays off;
 3. starts the VNC viewer on 127.0.0.1::PORT and waits for it to exit;
 4. runs remote-desk off and closes the tunnel, also after Ctrl-C.

remote-desk off restarts the host's VNC server, so a viewer that is still
connected then drops. desk off HOST and desk status HOST run remote-desk off
or status alone, for example when a session ended without off.

The command uses ssh and its configuration as they are; it never handles
the VNC password, which the viewer asks for. Options go before HOST:

  -via A,B         SSH names to try in order (default: HOST-lan if the SSH
                   configuration defines it, then HOST)
  -lan-timeout D   how long each name but the last may take to answer; they
                   are tried with strict host key checking (default 2s)
  -port N          local port the tunnel listens on (default 5900)
  -remote-port N   the VNC server's port on the host's loopback (default 5900)
  -viewer PATH     the VNC viewer (default: vncviewer on PATH, else
                   TigerVNC.app's)
  -version         print the version and exit

Options after -- go to the viewer, before the server address.
`

// Set by the package build.
var version = "dev"

// Injectable for tests.
var (
	sshPath        = "ssh"
	viewerApps     = []string{"/Applications/TigerVNC.app"}
	attemptTimeout = 45 * time.Second
	exitTimeout    = 10 * time.Second
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

type options struct {
	via         string
	lanTimeout  time.Duration
	port        int
	remotePort  int
	viewer      string
	viewerArgs  []string
	action      string // "" for a session, "off" or "status"
	host        string
	routes      []string
	stdout      io.Writer
	stderr      io.Writer
	controlPath string
}

func run(args []string, stdout, stderr io.Writer) int {
	o := options{stdout: stdout, stderr: stderr}
	fs := flag.NewFlagSet("desk", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usage) }
	fs.StringVar(&o.via, "via", "", "")
	fs.DurationVar(&o.lanTimeout, "lan-timeout", 2*time.Second, "")
	fs.IntVar(&o.port, "port", 5900, "")
	fs.IntVar(&o.remotePort, "remote-port", 5900, "")
	fs.StringVar(&o.viewer, "viewer", "", "")
	showVersion := fs.Bool("version", false, "")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *showVersion {
		fmt.Fprintf(stdout, "desk %s\n", version)
		return 0
	}

	rest := fs.Args()
	if len(rest) > 0 && (rest[0] == "off" || rest[0] == "status") {
		o.action = rest[0]
		rest = rest[1:]
	}
	for i, a := range rest {
		if a == "--" {
			o.viewerArgs = rest[i+1:]
			rest = rest[:i]
			break
		}
	}
	if len(rest) != 1 || rest[0] == "" || strings.HasPrefix(rest[0], "-") {
		fmt.Fprint(stderr, usage)
		return 2
	}
	if o.action != "" && len(o.viewerArgs) > 0 {
		fmt.Fprintf(stderr, "desk: viewer options apply to a session, not to %s\n", o.action)
		return 2
	}
	if o.lanTimeout <= 0 || !validPort(o.port) || !validPort(o.remotePort) {
		fmt.Fprint(stderr, "desk: -lan-timeout must be positive and ports between 1 and 65535\n")
		return 2
	}
	o.host = rest[0]
	o.routes = routes(o.host, o.via)

	if len(o.routes) == 0 {
		fmt.Fprint(stderr, "desk: -via names no SSH names\n")
		return 2
	}

	if o.action != "" {
		if err := o.retry(o.action, ""); err != nil {
			fmt.Fprintf(stderr, "desk: remote-desk %s: %v\n", o.action, err)
			return 1
		}
		return 0
	}
	if err := o.session(); err != nil {
		fmt.Fprintf(stderr, "desk: %v\n", err)
		return 1
	}
	return 0
}

func validPort(p int) bool { return p > 0 && p < 65536 }

// routes lists the SSH names to try in order: the -via list, or HOST-lan
// when the SSH configuration defines it, followed by HOST.
func routes(host, via string) []string {
	if via != "" {
		var out []string
		for _, r := range strings.Split(via, ",") {
			if r = strings.TrimSpace(r); r != "" {
				out = append(out, r)
			}
		}
		return out
	}
	lan := host + "-lan"
	if sshDefines(lan) {
		return []string{lan, host}
	}
	return []string{host}
}

// sshDefines reports whether the SSH configuration gives name a host name
// of its own; for an unknown name ssh -G reports the name itself.
func sshDefines(name string) bool {
	out, err := exec.Command(sshPath, "-G", name).Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(line, "hostname "); ok {
			return !strings.EqualFold(strings.TrimSpace(v), name)
		}
	}
	return false
}

// sshArgs are the options for connecting through route. Every name but the
// last one tried for a session must answer quickly and must already be a
// known host, because a LAN address on another network may belong to a
// different machine. That holds by name, wherever the name is tried.
func (o *options) sshArgs(route string) []string {
	if route == o.routes[len(o.routes)-1] {
		return nil
	}
	secs := int((o.lanTimeout + time.Second - 1) / time.Second)
	return []string{"-o", "ConnectTimeout=" + strconv.Itoa(secs), "-o", "StrictHostKeyChecking=yes"}
}

// remoteDesk runs remote-desk with action on the host through route, on a
// connection of its own: sharing is off, so neither the tunnel's master
// nor one from the user's configuration can hold it up.
func (o *options) remoteDesk(ctx context.Context, route, action string) error {
	args := append([]string{"-o", "ControlPath=none"}, o.sshArgs(route)...)
	args = append(args, route, "remote-desk", action)
	cmd := exec.CommandContext(ctx, sshPath, args...)
	cmd.Stdout = o.stdout
	cmd.Stderr = o.stderr
	cmd.WaitDelay = time.Second
	return cmd.Run()
}

// connectFailed reports whether ssh, rather than remote-desk, failed: ssh
// exits with 255 when it cannot connect or loses the connection, and an
// attempt that ran out of time was killed.
func connectFailed(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return true
	}
	var exit *exec.ExitError
	return errors.As(err, &exit) && exit.ExitCode() == 255
}

// retry runs an idempotent remote-desk action (off or status) through
// first, when given, and then through every other route in turn, each
// attempt with its own deadline, until one connects. A failure of
// remote-desk itself is not repeated.
func (o *options) retry(action, first string) error {
	var order []string
	if first != "" {
		order = append(order, first)
	}
	for _, r := range o.routes {
		if r != first {
			order = append(order, r)
		}
	}
	var errs []error
	for _, r := range order {
		ctx, cancel := context.WithTimeout(context.Background(), attemptTimeout)
		err := o.remoteDesk(ctx, r, action)
		failed := err != nil && connectFailed(ctx, err)
		cancel()
		if err == nil {
			return nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", r, err))
		if !failed {
			break
		}
	}
	return errors.Join(errs...)
}

func (o *options) session() error {
	viewer, err := findViewer(o.viewer)
	if err != nil {
		return err
	}
	// Fail before the host's displays go off, not after.
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(o.port)))
	if err != nil {
		return fmt.Errorf("local port %d is in use (choose another with -port): %w", o.port, err)
	}
	l.Close()

	dir, err := os.MkdirTemp("", "desk")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	o.controlPath = filepath.Join(dir, "ctl")

	// The first signal cancels ctx; later ones are absorbed until the end,
	// so off still runs.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	route, err := o.openTunnel(ctx)
	// A master may have started even when ssh was interrupted.
	defer o.closeTunnel()
	if err != nil {
		return err
	}

	// on runs once, through the route the tunnel took: another route could
	// repeat an on that already ran.
	fmt.Fprintf(o.stderr, "desk: turning the remote desk on (the host's displays go off)\n")
	onErr := o.remoteDesk(ctx, route, "on")
	onInterrupted := ctx.Err() != nil
	var viewErr error
	if onErr == nil && !onInterrupted {
		viewErr = o.view(ctx, viewer)
	}

	// Always after on, which may have turned the displays off before failing.
	fmt.Fprintf(o.stderr, "desk: turning the remote desk off\n")
	offErr := o.retry("off", route)
	// An interrupted on, or one whose connection dropped, may still be
	// running on the host after off.
	if offErr != nil || onErr != nil || onInterrupted {
		fmt.Fprintf(o.stderr, "desk: if the host's displays stay off, run desk off %s\n", o.host)
	}

	switch {
	case onErr != nil:
		return fmt.Errorf("remote-desk on: %w", onErr)
	case offErr != nil:
		return fmt.Errorf("remote-desk off: %w", offErr)
	case viewErr != nil:
		return viewErr
	case ctx.Err() != nil:
		return errors.New("interrupted")
	}
	return nil
}

// openTunnel starts a backgrounded ssh master that holds the forward and
// returns once the forward listens, through the first route that answers.
func (o *options) openTunnel(ctx context.Context) (string, error) {
	forward := fmt.Sprintf("127.0.0.1:%d:127.0.0.1:%d", o.port, o.remotePort)
	for i, r := range o.routes {
		if ctx.Err() != nil {
			return "", errors.New("interrupted")
		}
		fmt.Fprintf(o.stderr, "desk: connecting through %s\n", r)
		args := []string{"-f", "-N", "-M", "-S", o.controlPath,
			"-o", "ControlPersist=no", "-o", "ExitOnForwardFailure=yes",
			"-L", forward}
		args = append(args, o.sshArgs(r)...)
		args = append(args, r)
		cmd := exec.CommandContext(ctx, sshPath, args...)
		cmd.Stdin = os.Stdin
		cmd.Stdout = o.stderr
		cmd.Stderr = o.stderr
		err := cmd.Run()
		if err == nil {
			return r, nil
		}
		if ctx.Err() != nil {
			return "", errors.New("interrupted")
		}
		if i < len(o.routes)-1 {
			fmt.Fprintf(o.stderr, "desk: %s did not answer (%v); trying %s\n", r, err, o.routes[i+1])
			continue
		}
		return "", fmt.Errorf("no route to %s answered: %s: %w", o.host, r, err)
	}
	return "", errors.New("no SSH names to try")
}

// closeTunnel stops the master, if one is listening on the control socket.
func (o *options) closeTunnel() {
	if _, err := os.Stat(o.controlPath); err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), exitTimeout)
	defer cancel()
	// The name is required but unused: the socket identifies the master.
	cmd := exec.CommandContext(ctx, sshPath, "-S", o.controlPath, "-O", "exit", o.host)
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(o.stderr, "desk: closing the tunnel: %v: %s\n", err, strings.TrimSpace(string(out)))
	}
}

// view runs the viewer until it exits or ctx ends, which ends the viewer.
// The address is IPv4 like the forward, so a listener on [::1] cannot
// answer instead.
func (o *options) view(ctx context.Context, viewer string) error {
	args := append(append([]string{}, o.viewerArgs...), fmt.Sprintf("127.0.0.1::%d", o.port))
	cmd := exec.Command(viewer, args...)
	cmd.Stdout = o.stderr
	cmd.Stderr = o.stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting the viewer: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		// A viewer that exits with an error after the connection drops is
		// still a finished session.
		if err != nil {
			fmt.Fprintf(o.stderr, "desk: the viewer exited: %v\n", err)
		}
		return nil
	case <-ctx.Done():
		cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			cmd.Process.Kill()
			<-done
		}
		return nil
	}
}

func findViewer(path string) (string, error) {
	if path != "" {
		p, err := exec.LookPath(path)
		if err != nil {
			return "", fmt.Errorf("viewer: %w", err)
		}
		return p, nil
	}
	if p, err := exec.LookPath("vncviewer"); err == nil {
		return p, nil
	}
	apps := append([]string{}, viewerApps...)
	if home, err := os.UserHomeDir(); err == nil {
		apps = append(apps, filepath.Join(home, "Applications", "TigerVNC.app"))
	}
	for _, app := range apps {
		p := filepath.Join(app, "Contents", "MacOS", "vncviewer")
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", errors.New("no VNC viewer found: install TigerVNC or pass -viewer")
}
