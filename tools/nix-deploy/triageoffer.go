package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

// triageGuard is set in the environment of the agent triage launches, so a
// deploy the agent runs while it repairs never offers triage again.
const triageGuard = "NIXFLEET_TRIAGE"

// maxAnswerBytes bounds the answer to the triage offer, newline included.
const maxAnswerBytes = 64

// offerTriage asks, after a failed run, whether to start nixfleet triage on
// the run's record (ADR-0016 phase 3), and on yes runs triage on that run.
// The offer needs a record, stdin and stdout on a terminal (terminals), an
// agentCommand in the inventory's triage object and no NIXFLEET_TRIAGE in the
// environment; otherwise nothing is printed. The deploy's status is returned
// whatever triage returns, so scripts still see the deploy failure. The run
// record must be finished, its run.lock released, before this is called.
func (d *deployer) offerTriage(status int, terminals bool, in io.Reader, out io.Writer, triage func([]string) int) int {
	if status == 0 || d.record == nil || d.record.dir == "" || !terminals {
		return status
	}
	if _, set := os.LookupEnv(triageGuard); set {
		return status
	}
	cfg, _, err := loadTriageConfig(d.configFile)
	if err != nil || cfg == nil || len(cfg.AgentCommand) == 0 {
		return status
	}
	fmt.Fprint(out, "deploy: start nixfleet triage on this run? [y/N] ")
	// isTerminal accepts any character device but the null device, so the
	// answer is bounded: a device such as /dev/zero must not hang the read.
	line, err := bufio.NewReader(io.LimitReader(in, maxAnswerBytes)).ReadString('\n')
	if err != nil {
		// End of input, a read error or an answer without a newline within
		// the limit declines; end the prompt's line.
		fmt.Fprintln(out)
		return status
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		triage([]string{"--config", d.configFile, d.record.dir})
	}
	return status
}
