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
func (g *serialGroup) readyCommand(host string) string {
	return strings.ReplaceAll(g.ReadyCommand, "{host}", host)
}

// others lists the group without host, in inventory order.
func (g *serialGroup) others(host string) []string {
	var others []string
	for _, member := range g.Hosts {
		if member != host {
			others = append(others, member)
		}
	}
	return others
}

// runReady runs one ready-command attempt in the foreground, so Ctrl-C
// reaches it, killed at the deadline.
func (g *serialGroup) runReady(host string, deadline time.Duration) bool {
	return command{argv: []string{"bash", "-c", g.readyCommand(host)}, timeout: deadline}.run() == 0
}

// waitReady runs the group's ready gate for one host until it passes or the
// group's timeout elapses. Each attempt is killed at the remaining deadline,
// a success reported after the deadline is rejected, and the gate gives up
// instead of pausing when the pause would leave no budget for another
// attempt. Elapsed time is the larger of wall-clock time and the requested
// pauses.
func (d *deployer) waitReady(g *serialGroup, host string) bool {
	if g.ReadyCommand == "" {
		return true
	}
	timeout := time.Duration(g.ReadyTimeout) * time.Second
	interval := d.settings.readyInterval
	label := d.ui.phase("system") + " " + d.ui.host(host)
	fmt.Printf("%s ready gate: %s\n", label, g.readyCommand(host))

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
		ok := g.runReady(host, remaining)
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
		label, g.ReadyTimeout, attempts, g.readyCommand(host))
	return false
}

// precheck checks each named member once before a switch and returns the
// first one that is not ready.
func (d *deployer) precheck(g *serialGroup, hosts []string) (string, bool) {
	if g.ReadyCommand == "" {
		return "", true
	}
	budget := precheckBudget
	if timeout := time.Duration(g.ReadyTimeout) * time.Second; timeout < budget {
		budget = timeout
	}
	for _, host := range hosts {
		fmt.Printf("%s %s ready pre-check: %s\n", d.ui.phase("check"), d.ui.host(host), g.readyCommand(host))
		if !g.runReady(host, budget) {
			return host, false
		}
	}
	return "", true
}
