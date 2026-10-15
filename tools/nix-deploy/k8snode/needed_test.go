package k8snode

import (
	"regexp"
	"strings"
	"testing"

	"nix-deploy/bootcheck"
)

// reboot --needed reads the host's systems first and skips a reboot it does
// not need before any cluster or host change.

// bootReport is the boot check's output for a host whose current system
// uses the given kernel; "-" leaves its initrd unresolved.
func bootReport(currentKernel, currentInitrd string) string {
	lines := []string{"os=Linux"}
	for _, side := range []string{"booted", "current", "profile"} {
		kernel, initrd := "k1", "i1"
		if side != "booted" {
			kernel, initrd = currentKernel, currentInitrd
		}
		if initrd != "-" {
			initrd = "/nix/store/" + initrd + "-initrd/initrd"
		}
		lines = append(lines,
			side+"=/nix/store/"+side+"-system",
			side+"-kernel=/nix/store/"+kernel+"-linux/bzImage",
			side+"-initrd="+initrd,
			side+"-kernel-modules=/nix/store/m1-linux-modules",
			side+"-kernel-params=p:loglevel=4",
		)
	}
	return strings.Join(lines, "\n") + "\n"
}

var bootCheckCall = regexp.QuoteMeta(bootcheck.RemoteCommand(bootcheck.Script))

// answerBootCheck serves the boot check with the given output and status.
func answerBootCheck(f *fakeCluster, output string, status int) {
	f.respond = func(c Command, line string) (Result, bool) {
		if c.Argv[0] == "ssh" && c.Argv[len(c.Argv)-1] == bootcheck.RemoteCommand(bootcheck.Script) {
			return Result{Stdout: output, Status: status}, true
		}
		return Result{}, false
	}
}

func TestNeededSkipsUnneededReboot(t *testing.T) {
	f := newHealthyCluster(t)
	answerBootCheck(f, bootReport("k1", "i1"), 0)
	output := run(t, f, "reboot", "--needed")
	// The boot check and the cordon read are the only commands: no other
	// cluster read, cordon or power action.
	if len(f.calls) != 2 || !regexp.MustCompile(`^ssh -o BatchMode=yes -o ConnectTimeout=10 root@server-a `+bootCheckCall+`$`).MatchString(f.calls[0].line) ||
		f.calls[1].line != "kubectl get node server-a -o json" {
		t.Fatalf("calls:\n%s", f.callLog())
	}
	mustContain(t, output, "server-a does not need a reboot")
	mustContain(t, output, "nothing was changed")
}

func TestNeededSkipsBeforeCheckOnlyGates(t *testing.T) {
	f := newHealthyCluster(t)
	answerBootCheck(f, bootReport("k1", "i1"), 0)
	run(t, f, "reboot", "--needed", "--check-only")
	if len(f.calls) != 2 {
		t.Fatalf("calls:\n%s", f.callLog())
	}
}

// A cordoned node is in unfinished maintenance: --needed does not skip it,
// so the normal gates report it instead of a silent success.
func TestNeededDoesNotSkipCordonedNode(t *testing.T) {
	f := newHealthyCluster(t)
	set(f.targetNode(), true, "spec", "unschedulable")
	answerBootCheck(f, bootReport("k1", "i1"), 0)
	opts := options(t, "reboot", "--needed", "server-a")
	o, output := newOperation(f, opts)
	expectRunFailure(t, o, output, "server-a is cordoned; not skipping it", "already cordoned; use --resume-maintenance")
	mustNotCall(t, f, regexp.QuoteMeta(rebootCommand))
}

// Changed kernel parameters take effect only at boot.
func TestNeededRebootsForKernelParams(t *testing.T) {
	f := newHealthyCluster(t)
	answerBootCheck(f, strings.Replace(bootReport("k1", "i1"), "current-kernel-params=p:loglevel=4", "current-kernel-params=p:loglevel=7", 1), 0)
	output := run(t, f, "reboot", "--needed")
	mustContain(t, output, "server-a needs a reboot: kernel-params differs")
	mustCall(t, f, regexp.QuoteMeta(rebootCommand))
}

func TestNeededRebootsWhenNeeded(t *testing.T) {
	f := newHealthyCluster(t)
	answerBootCheck(f, bootReport("k2", "i1"), 0)
	output := run(t, f, "reboot", "--needed")
	requireOrder(t, f, bootCheckCall, `^kubectl get node server-a$`, `^kubectl cordon server-a$`, regexp.QuoteMeta(rebootCommand), `^kubectl uncordon server-a$`)
	mustContain(t, output, "server-a needs a reboot: kernel differs between the booted system and the current or next-boot system")
}

// Fail-safe: a verdict that cannot be drawn never skips the reboot.
func TestNeededRebootsWhenUnknown(t *testing.T) {
	cases := []struct {
		name   string
		output string
		status int
		want   string
	}{
		{"missing initrd", bootReport("k1", "-"), 0, "cannot tell whether server-a needs a reboot (cannot compare initrd"},
		{"unreadable", "", 255, "cannot read whether root@server-a needs a reboot (ssh exited with status 255); rebooting"},
		{"no output", "", 0, "cannot tell whether server-a needs a reboot (the host did not report its systems)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newHealthyCluster(t)
			answerBootCheck(f, c.output, c.status)
			output := run(t, f, "reboot", "--needed")
			requireOrder(t, f, bootCheckCall, `^kubectl cordon server-a$`, regexp.QuoteMeta(rebootCommand))
			mustContain(t, output, c.want)
		})
	}
}

func TestRebootWithoutNeededSkipsTheCheck(t *testing.T) {
	f := newHealthyCluster(t)
	run(t, f, "reboot")
	mustNotCall(t, f, bootCheckCall)
}
