package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

// failedTargets returns the targets the report records as failed, which
// before activation are those that failed to evaluate or build.
func failedTargets(targets []target, r *report) []target {
	var failed []target
	for _, t := range targets {
		if r.result(t.phase, t.host) == "failed" {
			failed = append(failed, t)
		}
	}
	return failed
}

// activateAfterFailures applies the --on-failure choice after the build and
// before any activation step, when some selected targets failed to evaluate
// or build (failed) and others built (ready). It returns whether to activate
// the ready targets; when not, it records them as skipped, so the run fails
// with nothing activated. With nothing failed or nothing ready there is
// nothing to decide.
func (d *deployer) activateAfterFailures(failed, ready []target, r *report) bool {
	return d.decideAfterFailures(failed, ready, r, os.Stdin, os.Stdout)
}

func (d *deployer) decideAfterFailures(failed, ready []target, r *report, in io.Reader, out io.Writer) bool {
	if len(failed) == 0 || len(ready) == 0 {
		return true
	}
	activate := false
	switch d.onFailure {
	case onFailureContinue:
		activate = true
	case onFailureAsk:
		activate = d.askActivate(failed, len(ready), in, out)
	}
	if activate {
		return true
	}
	for _, t := range ready {
		r.record(t.phase, t.host, "skipped")
	}
	r.failed = true
	reason := "--on-failure stop"
	if d.onFailure == onFailureAsk {
		reason = "declined"
	}
	fmt.Fprintf(os.Stderr, "deploy: %s: nothing was activated; the %s that built %s skipped\n",
		reason, countTargets(len(ready)), plural(len(ready), "is", "are"))
	return false
}

// askActivate lists the failed targets and asks whether to activate the ones
// that built. Only y or yes activates; anything else, the end of input or an
// interrupt declines.
func (d *deployer) askActivate(failed []target, ready int, in io.Reader, out io.Writer) bool {
	fmt.Fprintf(out, "deploy: %s failed to evaluate or build:\n", countTargets(len(failed)))
	for _, t := range failed {
		fmt.Fprintf(out, "  %s\n", d.label(t))
	}
	fmt.Fprintf(out, "deploy: activate the %s that built? [y/N] ", countTargets(ready))
	// As for the triage offer, the answer is bounded so a character device
	// such as /dev/zero cannot hang the read.
	line, err := readPromptLine(runContext, bufio.NewReader(io.LimitReader(in, maxAnswerBytes)))
	if err != nil {
		fmt.Fprintln(out)
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}

func countTargets(n int) string {
	return fmt.Sprintf("%d %s", n, plural(n, "target", "targets"))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
