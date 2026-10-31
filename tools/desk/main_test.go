package main

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeSSH records each call as one line in $DESK_LOG. Names in
// $FAKE_DEFINED have a host name of their own, connections to names in
// $FAKE_DOWN fail as ssh does (255), and remote-desk actions in $FAKE_FAIL
// fail with remote-desk's own status.
const fakeSSH = `#!/bin/sh
echo "ssh $*" >>"$DESK_LOG"
if [ "$1" = -G ]; then
	case " $FAKE_DEFINED " in
	*" $2 "*) echo "hostname 192.0.2.1" ;;
	*) echo "hostname $2" ;;
	esac
	exit 0
fi
route=
action=
prev=
for a in "$@"; do
	[ "$a" = remote-desk ] && route=$prev
	[ "$prev" = remote-desk ] && action=$a
	prev=$a
done
[ -z "$route" ] && route=$prev
case " $* " in *" -O exit "*) exit 0 ;; esac
# remote-desk over route: "<action>@<route>" in $FAKE_STALL hangs, in
# $FAKE_LOST drops the connection.
if [ -n "$action" ]; then
	case " $FAKE_STALL " in *" $action@$route "*) exec sleep 30 ;; esac
	case " $FAKE_LOST " in *" $action@$route "*) exit 255 ;; esac
fi
case " $FAKE_DOWN " in *" $route "*) [ -n "$route" ] && exit 255 ;; esac
case " $* " in *" -f "*)
	# The master's control socket.
	prev=
	for a in "$@"; do [ "$prev" = -S ] && touch "$a"; prev=$a; done
	exit 0
	;;
esac
case " $FAKE_FAIL " in *" $action "*) exit 1 ;; esac
exit 0
`

const fakeViewer = `#!/bin/sh
echo "viewer $*" >>"$DESK_LOG"
[ -n "$FAKE_VIEWER_SLEEP" ] && exec sleep "$FAKE_VIEWER_SLEEP"
exit 0
`

type fixture struct {
	t      *testing.T
	log    string
	viewer string
	port   int
}

func setup(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	ssh := filepath.Join(dir, "ssh")
	viewer := filepath.Join(dir, "viewer")
	for path, body := range map[string]string{ssh: fakeSSH, viewer: fakeViewer} {
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f := &fixture{t: t, log: filepath.Join(dir, "log"), viewer: viewer, port: freePort(t)}
	t.Setenv("DESK_LOG", f.log)
	t.Setenv("FAKE_DEFINED", "")
	t.Setenv("FAKE_DOWN", "")
	t.Setenv("FAKE_FAIL", "")
	t.Setenv("FAKE_VIEWER_SLEEP", "")
	t.Setenv("FAKE_STALL", "")
	t.Setenv("FAKE_LOST", "")
	oldSSH := sshPath
	sshPath = ssh
	t.Cleanup(func() { sshPath = oldSSH })
	return f
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func (f *fixture) run(args ...string) (int, string) {
	f.t.Helper()
	var out, errOut bytes.Buffer
	full := append([]string{"-viewer", f.viewer, "-port", strconv.Itoa(f.port)}, args...)
	code := run(full, &out, &errOut)
	return code, errOut.String()
}

// calls returns the recorded calls with the control socket path replaced
// by CTL and the forward by FWD.
func (f *fixture) calls() []string {
	f.t.Helper()
	data, err := os.ReadFile(f.log)
	if err != nil && !os.IsNotExist(err) {
		f.t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		for i := range fields {
			if i > 0 && fields[i-1] == "-S" {
				fields[i] = "CTL"
			}
			if strings.HasPrefix(fields[i], "127.0.0.1::") {
				fields[i] = "127.0.0.1::PORT"
			} else if strings.HasPrefix(fields[i], "127.0.0.1:") {
				fields[i] = "FWD"
			}
		}
		out = append(out, strings.Join(fields, " "))
	}
	return out
}

func (f *fixture) want(want ...string) {
	f.t.Helper()
	got := f.calls()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		f.t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

const (
	master   = "ssh -f -N -M -S CTL -o ControlPersist=no -o ExitOnForwardFailure=yes -L FWD"
	probe    = "-o ConnectTimeout=2 -o StrictHostKeyChecking=yes"
	direct   = "ssh -o ControlPath=none"
	closeCtl = "ssh -S CTL -O exit xyz"
)

func TestSessionOverLAN(t *testing.T) {
	f := setup(t)
	t.Setenv("FAKE_DEFINED", "xyz-lan")
	code, stderr := f.run("xyz", "--", "-FullScreen")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	f.want(
		"ssh -G xyz-lan",
		master+" "+probe+" xyz-lan",
		direct+" "+probe+" xyz-lan remote-desk on",
		"viewer -FullScreen 127.0.0.1::PORT",
		direct+" "+probe+" xyz-lan remote-desk off",
		closeCtl,
	)
}

func TestSessionFallsBackWhenLANDoesNotAnswer(t *testing.T) {
	f := setup(t)
	t.Setenv("FAKE_DEFINED", "xyz-lan")
	t.Setenv("FAKE_DOWN", "xyz-lan")
	code, stderr := f.run("xyz")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	// on and off go through the chosen route.
	f.want(
		"ssh -G xyz-lan",
		master+" "+probe+" xyz-lan",
		master+" xyz",
		direct+" xyz remote-desk on",
		"viewer 127.0.0.1::PORT",
		direct+" xyz remote-desk off",
		closeCtl,
	)
}

func TestSessionWithoutLANName(t *testing.T) {
	f := setup(t)
	code, stderr := f.run("xyz")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	f.want(
		"ssh -G xyz-lan",
		master+" xyz",
		direct+" xyz remote-desk on",
		"viewer 127.0.0.1::PORT",
		direct+" xyz remote-desk off",
		closeCtl,
	)
}

func TestFailedOnStillRunsOffAndSkipsViewer(t *testing.T) {
	f := setup(t)
	t.Setenv("FAKE_FAIL", "on")
	code, stderr := f.run("xyz")
	if code != 1 || !strings.Contains(stderr, "remote-desk on") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	// A remote-desk failure is not retried over another route.
	f.want(
		"ssh -G xyz-lan",
		master+" xyz",
		direct+" xyz remote-desk on",
		direct+" xyz remote-desk off",
		closeCtl,
	)
}

func TestNoRouteAnswers(t *testing.T) {
	f := setup(t)
	t.Setenv("FAKE_DOWN", "a b")
	code, stderr := f.run("-via", "a,b", "xyz")
	if code != 1 || !strings.Contains(stderr, "no route to xyz answered") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	// Nothing ran on the host.
	f.want(
		master+" "+probe+" a",
		master+" b",
	)
}

func TestBusyPortFailsBeforeConnecting(t *testing.T) {
	f := setup(t)
	l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(f.port))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	code, stderr := f.run("xyz")
	if code != 1 || !strings.Contains(stderr, "is in use") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	// Only the configuration was read.
	f.want("ssh -G xyz-lan")
}

func TestSignalEndsViewerAndRunsOff(t *testing.T) {
	f := setup(t)
	t.Setenv("FAKE_VIEWER_SLEEP", "30")
	go func() {
		// Wait for the viewer to start, then interrupt.
		for i := 0; i < 200; i++ {
			for _, c := range f.calls() {
				if strings.HasPrefix(c, "viewer") {
					syscall.Kill(os.Getpid(), syscall.SIGTERM)
					return
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	start := time.Now()
	code, stderr := f.run("xyz")
	if code != 1 || !strings.Contains(stderr, "interrupted") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if time.Since(start) > 20*time.Second {
		t.Fatal("the viewer was not ended")
	}
	f.want(
		"ssh -G xyz-lan",
		master+" xyz",
		direct+" xyz remote-desk on",
		"viewer 127.0.0.1::PORT",
		direct+" xyz remote-desk off",
		closeCtl,
	)
}

func TestOffMovesOnFromStalledRoute(t *testing.T) {
	f := setup(t)
	old := attemptTimeout
	attemptTimeout = 300 * time.Millisecond
	t.Cleanup(func() { attemptTimeout = old })
	t.Setenv("FAKE_DEFINED", "xyz-lan")
	t.Setenv("FAKE_STALL", "off@xyz-lan")
	code, stderr := f.run("xyz")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	// The stalled attempt gets its own deadline; then the next route.
	f.want(
		"ssh -G xyz-lan",
		master+" "+probe+" xyz-lan",
		direct+" "+probe+" xyz-lan remote-desk on",
		"viewer 127.0.0.1::PORT",
		direct+" "+probe+" xyz-lan remote-desk off",
		direct+" xyz remote-desk off",
		closeCtl,
	)
}

func TestOnIsNotRetriedOnAnotherRoute(t *testing.T) {
	f := setup(t)
	t.Setenv("FAKE_DEFINED", "xyz-lan")
	t.Setenv("FAKE_LOST", "on@xyz-lan off@xyz-lan")
	code, stderr := f.run("xyz")
	if code != 1 || !strings.Contains(stderr, "remote-desk on") || !strings.Contains(stderr, "run desk off xyz") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	// off moves on to the next route; on does not.
	f.want(
		"ssh -G xyz-lan",
		master+" "+probe+" xyz-lan",
		direct+" "+probe+" xyz-lan remote-desk on",
		direct+" "+probe+" xyz-lan remote-desk off",
		direct+" xyz remote-desk off",
		closeCtl,
	)
}

func TestSignalEndsStalledOn(t *testing.T) {
	f := setup(t)
	t.Setenv("FAKE_STALL", "on@xyz")
	go func() {
		for i := 0; i < 200; i++ {
			for _, c := range f.calls() {
				if strings.HasSuffix(c, "remote-desk on") {
					time.Sleep(100 * time.Millisecond)
					syscall.Kill(os.Getpid(), syscall.SIGTERM)
					return
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	start := time.Now()
	code, stderr := f.run("xyz")
	if code != 1 || !strings.Contains(stderr, "run desk off xyz") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if time.Since(start) > 20*time.Second {
		t.Fatal("on was not ended")
	}
	f.want(
		"ssh -G xyz-lan",
		master+" xyz",
		direct+" xyz remote-desk on",
		direct+" xyz remote-desk off",
		closeCtl,
	)
}

func TestOffAndStatusAlone(t *testing.T) {
	f := setup(t)
	t.Setenv("FAKE_DEFINED", "xyz-lan")
	t.Setenv("FAKE_DOWN", "xyz-lan")
	for _, action := range []string{"off", "status"} {
		if code, stderr := f.run(action, "xyz"); code != 0 {
			t.Fatalf("%s: exit %d: %s", action, code, stderr)
		}
	}
	f.want(
		"ssh -G xyz-lan",
		direct+" "+probe+" xyz-lan remote-desk off",
		direct+" xyz remote-desk off",
		"ssh -G xyz-lan",
		direct+" "+probe+" xyz-lan remote-desk status",
		direct+" xyz remote-desk status",
	)
}

func TestUsage(t *testing.T) {
	f := setup(t)
	for _, args := range [][]string{{}, {"a", "b"}, {"off"}, {"off", "xyz", "--", "-x"}, {"-port", "0", "xyz"}, {"-via", " , ", "xyz"}} {
		if code, _ := f.run(args...); code != 2 {
			t.Errorf("%q: exit %d, want 2", args, code)
		}
	}
	var out bytes.Buffer
	if code := run([]string{"-version"}, &out, &out); code != 0 || out.String() != "desk dev\n" {
		t.Errorf("-version: %d %q", code, out.String())
	}
}

func TestMissingViewerFailsBeforeConnecting(t *testing.T) {
	f := setup(t)
	f.viewer = filepath.Join(t.TempDir(), "missing")
	code, stderr := f.run("xyz")
	if code != 1 || !strings.Contains(stderr, "viewer") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	f.want("ssh -G xyz-lan")
}

func TestFindViewer(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	t.Setenv("HOME", dir)
	app := filepath.Join(dir, "TigerVNC.app")
	old := viewerApps
	viewerApps = []string{app}
	t.Cleanup(func() { viewerApps = old })
	if _, err := findViewer(""); err == nil {
		t.Fatal("found a viewer in an empty directory")
	}
	bin := filepath.Join(app, "Contents", "MacOS", "vncviewer")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := findViewer(""); err != nil || got != bin {
		t.Fatalf("got %q, %v", got, err)
	}
	onPath := filepath.Join(dir, "vncviewer")
	if err := os.WriteFile(onPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := findViewer(""); err != nil || got != onPath {
		t.Fatalf("got %q, %v", got, err)
	}
}
