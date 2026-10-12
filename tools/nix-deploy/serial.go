package main

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// precheckBudget bounds the one-attempt readiness check of serial members
// before a switch.
const precheckBudget = 60 * time.Second

// readyCommand substitutes the host into the consumer-authored ready
// command. Host names are validated as plain identifiers, so substitution
// cannot add shell syntax to it.
func (d *deployer) readyCommand(host string) string {
	return strings.ReplaceAll(d.inv.SerialReadyCommand, "{host}", host)
}

// runReady runs one ready-command attempt in the foreground, so Ctrl-C
// reaches it, killed at the deadline.
func (d *deployer) runReady(host string, deadline time.Duration) bool {
	return command{argv: []string{"bash", "-c", d.readyCommand(host)}, timeout: deadline}.run() == 0
}

// waitReady runs the serial ready gate for one host until it passes or
// serialReadyTimeoutSeconds elapse. Each attempt is killed at the remaining
// deadline, a success reported after the deadline is rejected, and the gate
// gives up instead of pausing when the pause would leave no budget for
// another attempt. Elapsed time is the larger of wall-clock time and the
// requested pauses.
func (d *deployer) waitReady(host string) bool {
	if d.inv.SerialReadyCommand == "" {
		return true
	}
	timeout := time.Duration(d.inv.SerialReadyTimeout) * time.Second
	interval := d.settings.readyInterval
	label := d.ui.phase("system") + " " + d.ui.host(host)
	fmt.Printf("%s ready gate: %s\n", label, d.readyCommand(host))

	start := time.Now()
	var clock time.Duration
	advance := func() {
		if elapsed := time.Since(start); elapsed > clock {
			clock = elapsed
		}
	}
	attempts := 0
	for {
		advance()
		remaining := timeout - clock
		if remaining <= 0 {
			break
		}
		attempts++
		ok := d.runReady(host, remaining)
		advance()
		if ok && clock <= timeout {
			fmt.Printf("%s ready gate passed after %d attempt(s)\n", label, attempts)
			return true
		}
		if timeout-clock <= interval {
			break
		}
		time.Sleep(interval)
		clock += interval
	}
	fmt.Fprintf(os.Stderr, "%s ready gate did not pass within %ds (%d attempt(s)): %s\n",
		label, d.inv.SerialReadyTimeout, attempts, d.readyCommand(host))
	return false
}

// precheckSerial checks each named serial host once before a serial switch
// and returns the first host that is not ready.
func (d *deployer) precheckSerial(hosts []string) (string, bool) {
	if d.inv.SerialReadyCommand == "" {
		return "", true
	}
	budget := precheckBudget
	if timeout := time.Duration(d.inv.SerialReadyTimeout) * time.Second; timeout < budget {
		budget = timeout
	}
	for _, host := range hosts {
		fmt.Printf("%s %s ready pre-check: %s\n", d.ui.phase("check"), d.ui.host(host), d.readyCommand(host))
		if !d.runReady(host, budget) {
			return host, false
		}
	}
	return "", true
}

// otherSerialHosts lists the serial group without host, in inventory order.
func (d *deployer) otherSerialHosts(host string) []string {
	var others []string
	for _, member := range d.inv.SerialSystemHosts {
		if member != host {
			others = append(others, member)
		}
	}
	return others
}
