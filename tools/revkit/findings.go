package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"revkit/review"
)

const findingsUsage = `Usage: revkit findings list [--all] [--json] [OPTIONS]
       revkit findings show [OPTIONS] ID...
       revkit findings dismiss --reason TEXT [OPTIONS] ID...
       revkit findings reopen [OPTIONS] ID...
       revkit findings file [--dry-run] [--new] [OPTIONS] ID...

The findings store of a repository holds every distinct problem its reviews
reported, by ID (F1, F2, ...). list shows the open ones, except those the
verifier rejected; --all shows every one. A dismissed problem stays dismissed
when later runs report it again; reopen undoes that, and also unties a
filed finding from its issue, which is left as it is.

file files the given findings as issues in the repository's configured
tracker, or links an open issue with the same title, and marks them filed.
Only open, confirmed findings that are not security-relevant can be filed,
at most the tracker's limit per command. An open issue with the same title
is linked instead of filing a duplicate; --new files a new issue for each
finding and links none (after reopen undid a wrong link) but the one revkit
filed for that finding, and --dry-run says what it would file or link.

Options: --config FILE (default $REVKIT_CONFIG) and --repository NAME
(default: the directory name of the git checkout around the current
directory).
`

var severityRank = map[string]int{"high": 0, "medium": 1, "low": 2}

func findingsCommand(ctx context.Context, args []string, stdout io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(os.Stderr, findingsUsage)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	action := args[0]
	flags := flag.NewFlagSet("findings "+action, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.Usage = func() { fmt.Fprint(os.Stderr, findingsUsage) }
	configPath := flags.String("config", os.Getenv("REVKIT_CONFIG"), "configuration `FILE`")
	repoName := flags.String("repository", "", "configured repository `NAME`")
	all := flags.Bool("all", false, "list every finding, whatever its status and verdict")
	asJSON := flags.Bool("json", false, "print the findings as JSON")
	reason := flags.String("reason", "", "why the findings are dismissed")
	dryRun := flags.Bool("dry-run", false, "print the issues file would create")
	fresh := flags.Bool("new", false, "file a new issue for each finding, linking only its own")
	ids, err := parseInterspersed(flags, args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(os.Stderr, "revkit findings: "+format+"\n", a...)
		return 2
	}
	if *configPath == "" {
		return fail("no configuration: pass --config FILE or set REVKIT_CONFIG")
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		return fail("%v", err)
	}
	if *repoName == "" {
		top, err := review.Git(".", "rev-parse", "--show-toplevel")
		if err != nil {
			return fail("not in a git checkout; pass --repository NAME")
		}
		*repoName = filepath.Base(strings.TrimSpace(top))
	}
	if (*dryRun || *fresh) && action != "file" {
		return fail("--dry-run and --new are only for file")
	}
	repo, ok := cfg.repository(*repoName)
	if !ok {
		return fail("repository %s is not configured; pass --repository NAME", *repoName)
	}
	stateDir, err := cfg.stateDir()
	if err != nil {
		return fail("%v", err)
	}
	path := storePath(stateDir, *repoName)

	switch action {
	case "list":
		if len(ids) > 0 {
			return fail("list takes no IDs")
		}
		s, err := readStore(path)
		if err != nil {
			return fail("%v", err)
		}
		listFindings(stdout, s.Findings, *all, *asJSON)
		return 0
	case "show":
		if len(ids) == 0 {
			return fail("show needs finding IDs")
		}
		s, err := readStore(path)
		if err != nil {
			return fail("%v", err)
		}
		for i, id := range ids {
			sf := s.find(id)
			if sf == nil {
				return fail("%s has no finding %s", *repoName, id)
			}
			if i > 0 {
				fmt.Fprintln(stdout)
			}
			showFinding(stdout, *sf)
		}
		return 0
	case "file":
		if len(ids) == 0 {
			return fail("file needs finding IDs")
		}
		if err := fileFindings(ctx, stdout, path, repo, ids, *dryRun, *fresh); err != nil {
			return fail("%v", err)
		}
		return 0
	case "dismiss", "reopen":
		if len(ids) == 0 {
			return fail("%s needs finding IDs", action)
		}
		if action == "dismiss" && strings.TrimSpace(*reason) == "" {
			return fail("dismiss needs --reason, so later readers know why")
		}
		// Messages wait until the store is saved, so none claims a change
		// a later error in the same command undid.
		var untied []string
		err := withStore(path, func(s *findingStore) error {
			for _, id := range ids {
				sf := s.find(id)
				switch {
				case sf == nil:
					return fmt.Errorf("%s has no finding %s", *repoName, id)
				case action == "dismiss" && sf.Status == storeFiled:
					return fmt.Errorf("%s is filed as %s; close the issue, or reopen the finding first", sf.ID, sf.Issue)
				case action == "dismiss":
					sf.Status, sf.Note = storeDismissed, strings.TrimSpace(*reason)
				case sf.Status == storeFiled:
					// Undoes a link, for example to an unrelated issue with
					// the same title; the issue itself is left alone.
					untied = append(untied, fmt.Sprintf("%s: no longer tied to %s, which is left as it is; file --new files it on its own", sf.ID, sf.Issue))
					sf.Status, sf.Issue = storeOpen, ""
				case sf.Status != storeDismissed:
					return fmt.Errorf("%s is neither dismissed nor filed", sf.ID)
				default:
					sf.Status, sf.Note = storeOpen, ""
				}
			}
			return nil
		})
		if err != nil {
			return fail("%v", err)
		}
		for _, line := range untied {
			fmt.Fprintln(stdout, line)
		}
		fmt.Fprintf(stdout, "%s: %s\n", action, strings.Join(ids, ", "))
		return 0
	}
	return fail("unknown action %q\n\n%s", action, findingsUsage)
}

// listed reports whether list shows a finding without --all.
func listed(sf storedFinding) bool {
	return sf.Status == storeOpen && sf.Verdict != "rejected"
}

func listFindings(out io.Writer, findings []storedFinding, all, asJSON bool) {
	var shown []storedFinding
	for _, sf := range findings {
		if all || listed(sf) {
			shown = append(shown, sf)
		}
	}
	sort.SliceStable(shown, func(i, j int) bool {
		if rank(severityRank, shown[i].Severity) != rank(severityRank, shown[j].Severity) {
			return rank(severityRank, shown[i].Severity) < rank(severityRank, shown[j].Severity)
		}
		return rank(verdictOrder, shown[i].Verdict) < rank(verdictOrder, shown[j].Verdict)
	})
	if asJSON {
		if shown == nil {
			shown = []storedFinding{}
		}
		encoded, _ := json.MarshalIndent(shown, "", "  ")
		fmt.Fprintln(out, string(encoded))
		return
	}
	if len(shown) == 0 {
		fmt.Fprintln(out, "no findings")
		return
	}
	table := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "ID\tSTATUS\tVERDICT\tSEVERITY\tLENS\tSEEN\tTITLE")
	for _, sf := range shown {
		title := oneLine(sf.Title)
		if sf.Security {
			title += " [security]"
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%d\t%s\n", sf.ID, sf.Status, sf.Verdict, sf.Severity, sf.Lens, sf.Sightings, title)
	}
	table.Flush()
}

func showFinding(out io.Writer, sf storedFinding) {
	fmt.Fprintf(out, "%s %s (%s, %s, %s)\n", sf.ID, oneLine(sf.Title), sf.Status, sf.Verdict, sf.Severity)
	fmt.Fprintf(out, "lens %s; seen %d times, first at %s, last at %s; reported by %s\n",
		sf.Lens, sf.Sightings, short(sf.FirstSeen.Commit), short(sf.LastSeen.Commit), strings.Join(sf.Reviewers, ", "))
	if sf.Security {
		fmt.Fprintln(out, "security-relevant: never filed")
	}
	if sf.Issue != "" {
		fmt.Fprintf(out, "issue: %s\n", sf.Issue)
	}
	if sf.Note != "" {
		fmt.Fprintf(out, "dismissed: %s\n", sf.Note)
	}
	fmt.Fprintf(out, "paths: %s\n\nclaim: %s\n\nevidence: %s\n\nsuggestion: %s\n", strings.Join(sf.Paths, ", "),
		sf.Claim, sf.Evidence, sf.Suggestion)
	if sf.Reason != "" {
		fmt.Fprintf(out, "\nverifier: %s\n", sf.Reason)
	}
}

func short(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}

// parseInterspersed parses flags that may come before, between or after the
// positional arguments, as in `dismiss F3 --reason ...`, and returns those.
func parseInterspersed(flags *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := flags.Parse(args); err != nil {
			return nil, err
		}
		args = flags.Args()
		if len(args) == 0 {
			return positional, nil
		}
		positional = append(positional, args[0])
		args = args[1:]
	}
}
