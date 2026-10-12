package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// sshCheck runs a short non-interactive command on an endpoint.
func (d *deployer) sshCheck(endpoint, remoteCommand string) (string, int) {
	argv := []string{"ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=" + d.settings.sshCheckTimeout, "-o", "ConnectionAttempts=1"}
	argv = append(argv, d.sshArgs...)
	argv = append(argv, endpoint, remoteCommand)
	return capture(0, argv...)
}

func (d *deployer) checkEndpoint(endpoint string) (string, bool) {
	output, status := d.sshCheck(endpoint, "true")
	message := oneLine(output)
	if message == "" {
		message = fmt.Sprintf("ssh exited with status %d", status)
	}
	return message, status == 0
}

// remoteFreeKiB returns the free KiB on the target filesystem backing /nix,
// or the error output.
func (d *deployer) remoteFreeKiB(endpoint string) (int64, string, bool) {
	output, status := d.sshCheck(endpoint, "df -Pk /nix | awk 'NR == 2 { print $4 }'")
	free := strings.TrimSpace(lastLine(output))
	if status != 0 || !digits.MatchString(free) {
		return 0, oneLine(output), false
	}
	kib, err := strconv.ParseInt(free, 10, 64)
	return kib, oneLine(output), err == nil
}

// remoteGC runs the target's own scheduled garbage collection once and waits
// at most DEPLOY_REMOTE_GC_TIMEOUT for it; the remote unit may keep running
// after the wait ends. Hosts without nix-gc.timer are left alone, so this
// frees only what the host's own schedule would.
func (d *deployer) remoteGC(endpoint string) (string, bool) {
	argv := []string{"ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=" + d.settings.sshCheckTimeout, "-o", "ConnectionAttempts=1"}
	argv = append(argv, d.sshArgs...)
	argv = append(argv, endpoint, "systemctl cat nix-gc.timer >/dev/null 2>&1 || exit 3; systemctl start nix-gc.service")
	output, status := capture(d.settings.remoteGCTimeout, argv...)
	switch status {
	case 0:
		return "", true
	case 3:
		return "no nix-gc.timer on the host", false
	case exitTimedOut, 137:
		return fmt.Sprintf("nix-gc.service did not finish within %ds; it may still be running on the host", int(d.settings.remoteGCTimeout.Seconds())), false
	default:
		return "nix-gc.service failed: " + lastLine(output), false
	}
}

func (d *deployer) checkCapacity(host string) bool {
	minimum := d.settings.remoteMinFreeKiB
	if minimum == 0 {
		return true
	}
	endpoint := d.inv.systemEndpoint(host)
	label := d.ui.phase("check") + " " + d.ui.host(host)

	free, output, ok := d.remoteFreeKiB(endpoint)
	if !ok {
		fmt.Fprintf(os.Stderr, "%s cannot check free space on %s: %s\n", label, endpoint, output)
		return false
	}

	// Only root can start the unit without an interactive sudo prompt. A dry
	// run changes nothing on the host, so it assumes the collection a real run
	// would start frees enough space.
	collects := free < minimum && d.settings.remoteGC && d.inv.systemUser(host) == "root"
	if collects && d.dryRun {
		fmt.Printf("%s only %s free on /nix filesystem; dry run: not starting nix-gc.service, assuming a real run frees enough\n", label, formatGiB(free))
		return true
	}
	if collects {
		fmt.Printf("%s only %s free on /nix filesystem; running nix-gc.service on the host\n", label, formatGiB(free))
		if message, ok := d.remoteGC(endpoint); !ok {
			fmt.Fprintf(os.Stderr, "%s %s\n", label, message)
		}
		// A failed or interrupted collection may still have freed space.
		free, output, ok = d.remoteFreeKiB(endpoint)
		if !ok {
			fmt.Fprintf(os.Stderr, "%s cannot check free space on %s: %s\n", label, endpoint, output)
			return false
		}
	}

	if free < minimum {
		fmt.Fprintf(os.Stderr, "%s only %s free on /nix filesystem; need at least %s. Clean old generations or set DEPLOY_REMOTE_MIN_FREE_KIB=0 to override.\n",
			label, formatGiB(free), formatGiB(minimum))
		return false
	}
	fmt.Printf("%s free on /nix filesystem: %s\n", label, formatGiB(free))
	return true
}

// preflightHost checks the endpoints a phase needs. mode is "nixos", "hm" or
// "full".
func (d *deployer) preflightHost(host, mode string) bool {
	if host == d.self || !d.preflight {
		return true
	}
	var endpoints []string
	if mode == "nixos" || mode == "full" {
		endpoints = append(endpoints, d.inv.systemEndpoint(host))
	}
	if mode == "hm" || mode == "full" {
		endpoint := d.inv.homeEndpoint(host)
		if !contains(endpoints, endpoint) {
			endpoints = append(endpoints, endpoint)
		}
	}

	for _, endpoint := range endpoints {
		if message, ok := d.checkEndpoint(endpoint); !ok {
			fmt.Fprintf(os.Stderr, "%s %s unavailable via %s: %s\n", d.ui.phase("check"), d.ui.host(host), endpoint, message)
			return false
		}
	}
	fmt.Printf("%s %s SSH available: %s\n", d.ui.phase("check"), d.ui.host(host), strings.Join(endpoints, " "))

	if mode == "nixos" || mode == "full" {
		return d.checkCapacity(host)
	}
	return true
}

// filterReachable splits hosts by preflight. It returns false when the phase
// cannot proceed: a host failed and --fail-unreachable is set, or none is
// reachable.
func (d *deployer) filterReachable(mode string, hosts []string) (available, skipped []string, ok bool) {
	if !d.preflight {
		return hosts, nil, true
	}
	d.ui.section("preflight")
	for _, host := range hosts {
		if host == d.self {
			fmt.Printf("%s %s local host\n", d.ui.phase("check"), d.ui.host(host))
			available = append(available, host)
			continue
		}
		if d.preflightHost(host, mode) {
			available = append(available, host)
		} else {
			skipped = append(skipped, host)
		}
	}

	if len(skipped) > 0 {
		if d.failUnreachable {
			fmt.Fprintf(os.Stderr, "deploy: hosts failed preflight: %s\n", joinHosts(skipped))
			return available, skipped, false
		}
		fmt.Fprintf(os.Stderr, "deploy: skipping hosts that failed preflight: %s\n", joinHosts(skipped))
	}
	if len(available) == 0 {
		fmt.Fprintln(os.Stderr, "deploy: no reachable hosts to deploy")
		return available, skipped, false
	}
	return available, skipped, true
}
