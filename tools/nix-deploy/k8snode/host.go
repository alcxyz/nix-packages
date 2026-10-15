package k8snode

import (
	"fmt"
	"strings"
)

// The host side of the power action: root access, the power command, and
// the SSH and boot-ID evidence that it took effect.

const privilegeCheckCommand = `
    if [[ $(id -u) -eq 0 ]]; then
      exit 0
    fi
    exec sudo -n true
  `

const rebootCommand = `
    if [[ $(id -u) -eq 0 ]]; then
      exec systemctl reboot --no-block
    fi
    exec sudo -n systemctl reboot --no-block
  `

const powerOffCommand = `
    if [[ $(id -u) -eq 0 ]]; then
      exec systemctl poweroff
    fi
    exec sudo -n systemctl poweroff
  `

// sshDownAttempts bounds the wait for SSH to drop after the power command.
const sshDownAttempts = 30

func (o *Operation) verifyRemotePrivilege() error {
	target := o.opts.SSHTarget
	o.log("checking privileged SSH access on %s", target)
	r := o.ssh(Command{Argv: []string{"-o", "BatchMode=yes", target, privilegeCheckCommand}, Passthrough: true})
	if r.Status != 0 {
		return fmt.Errorf("%s does not provide noninteractive root access; no cluster state was changed", target)
	}
	return nil
}

// readBootID reads the target's boot ID; it is "" when it cannot be read.
func (o *Operation) readBootID() string {
	r := o.ssh(Command{Argv: []string{"-o", "BatchMode=yes", o.opts.SSHTarget, "cat", "/proc/sys/kernel/random/boot_id"}})
	if r.Status != 0 {
		return ""
	}
	return strings.TrimRight(r.Stdout, "\n")
}

// rebootHost records the boot ID and starts the reboot. ssh's status 255
// is accepted: the connection may drop as the host goes down.
func (o *Operation) rebootHost() error {
	target := o.opts.SSHTarget
	o.bootID = o.readBootID()
	if o.bootID == "" {
		return fmt.Errorf("could not read the current boot ID from %s", target)
	}
	o.log("rebooting %s", target)
	r := o.ssh(Command{Argv: []string{"-T", target, rebootCommand}, Passthrough: true})
	if r.Status != 0 && r.Status != 255 {
		return fmt.Errorf("remote reboot command failed before reboot started; %s remains cordoned", o.opts.Node)
	}
	return nil
}

func (o *Operation) verifyNewBoot() error {
	target := o.opts.SSHTarget
	current := o.readBootID()
	if current == "" {
		return fmt.Errorf("could not read the boot ID after %s returned", target)
	}
	if current == o.bootID {
		return fmt.Errorf("%s returned without rebooting; %s remains cordoned", target, o.opts.Node)
	}
	return nil
}

func (o *Operation) powerOffHost() error {
	target := o.opts.SSHTarget
	o.log("powering off %s", target)
	r := o.ssh(Command{Argv: []string{"-T", target, powerOffCommand}, Passthrough: true})
	if r.Status != 0 && r.Status != 255 {
		return fmt.Errorf("remote poweroff command failed before shutdown started")
	}
	return nil
}

// waitForSSHDown verifies that the power action took effect.
func (o *Operation) waitForSSHDown() error {
	o.log("waiting for SSH on %s to drop", o.opts.SSHTarget)
	for range sshDownAttempts {
		r := o.ssh(Command{Argv: []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=2", o.opts.SSHTarget, "true"}, Quiet: true})
		// A probe refused or killed by an interrupt is no evidence of a drop,
		// and must not clear the stopped-agent warning.
		if r.Status == statusNotStarted || o.ctx.Err() != nil {
			return errInterrupted
		}
		if r.Status != 0 {
			return nil
		}
		if err := o.pause(); err != nil {
			return err
		}
	}
	return fmt.Errorf("SSH did not drop; the power action was not verified and %s remains cordoned", o.opts.Node)
}

// waitForSSHUp waits for the host to accept SSH again. The timeout counts
// poll intervals, as in the Bash tool, but the host is probed at least once,
// so a zero timeout does not fail without looking.
func (o *Operation) waitForSSHUp() error {
	o.log("waiting for SSH on %s to return", o.opts.SSHTarget)
	step := max(int(o.poll.Seconds()), 1)
	for elapsed := 0; ; elapsed += step {
		r := o.ssh(Command{Argv: []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=5", o.opts.SSHTarget, "true"}, Quiet: true})
		if r.Status == 0 {
			return nil
		}
		if elapsed+step >= o.opts.SSHTimeoutSeconds {
			break
		}
		if err := o.pause(); err != nil {
			return err
		}
	}
	return fmt.Errorf("SSH did not return within %ds", o.opts.SSHTimeoutSeconds)
}
