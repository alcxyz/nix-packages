// Command revkit runs agent reviews: repo-review reviews a whole repository
// at rest through configured lenses, with read-only reviewers of both
// clients, each in a home that holds only its login (#557).
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

// version is set by the package build (-X main.version); see CHANGELOG.md.
var version = "dev"

const usage = `Usage: revkit repo-review [OPTIONS] [PATH]
       revkit findings list|show|dismiss|reopen|file [OPTIONS] [ID...]
       revkit --version

Subcommands:
  repo-review  Review the repository at PATH (default: the current directory)
               as a whole, one job per configured lens and reviewer. Each
               reviewer's findings are verified by the next reviewer, and
               the result is written to a local report; nothing is filed.
               Distinct problems go to the repository's findings store,
               matched by meaning with what earlier runs reported.
  findings     List, show, dismiss and reopen the stored findings, and file
               the confirmed ones you pick as issues.

The configuration comes from --config FILE or REVKIT_CONFIG; see
docs/adr/0011-revkit-agent-review-toolkit.md for its fields.

Exit status: 0 when every review and verification completed, 1 when any
reviewer failed (a missing review, not one without findings), 2 for usage
and configuration errors.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	switch args[0] {
	case "--version", "version":
		fmt.Printf("revkit %s\n", version)
		return 0
	case "-h", "--help", "help":
		fmt.Print(usage)
		return 0
	case "repo-review":
		return repoReview(ctx, args[1:])
	case "findings":
		return findingsCommand(ctx, args[1:], os.Stdout)
	}
	fmt.Fprintf(os.Stderr, "revkit: unknown subcommand %q\n\n%s", args[0], usage)
	return 2
}
