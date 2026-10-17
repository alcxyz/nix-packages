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
	"slices"
	"strings"
	"sync"
	"time"

	"revkit/review"
)

const findPrompt = `You are reviewing the repository checked out in the current directory as a
whole, at rest, not a change. Only report problems you have confirmed by
reading the files: quote or cite exact paths and line numbers. Prefer a few
well-evidenced findings over many guesses; report nothing rather than
speculation. Do not report style preferences. Material outside this checkout
is not available: do not report a reference as broken only because its
target is outside this tree.

Reply with ONLY a JSON object, no prose before or after, in this shape:
{"findings": [{"title": "...", "severity": "high|medium|low",
  "paths": ["path:line", ...], "claim": "what is wrong, one or two sentences",
  "evidence": "what you read that shows it", "suggestion": "smallest fix"}]}
Use {"findings": []} when you find nothing that meets the bar. If you cannot
read the files, reply {"error": "what stopped you"} instead.
`

const verifyPrompt = `You are verifying findings that another reviewer reported about the repository
checked out in the current directory. For each finding, read the cited files
and decide whether the claim is true as stated. Be strict: a finding that is
partly true, exaggerated, or depends on something outside this tree is not
"confirmed".

Reply with ONLY a JSON object, no prose before or after:
{"verdicts": [{"id": "<finding id>", "verdict": "confirmed|partly|rejected",
  "reason": "one or two sentences citing what you read"}]}
If you cannot read the files, reply {"error": "what stopped you"} instead.

Findings:
`

// Statuses beyond the runner's: a reviewer that replied with something
// other than the asked-for JSON did not review either.
const statusUnparsed = "unparsed"

type finding struct {
	ID         string   `json:"id"`
	Reviewer   string   `json:"reviewer"`
	Lens       string   `json:"lens"`
	Title      string   `json:"title"`
	Severity   string   `json:"severity"`
	Paths      []string `json:"paths"`
	Claim      string   `json:"claim"`
	Evidence   string   `json:"evidence"`
	Suggestion string   `json:"suggestion"`
	// Verdict is confirmed, partly, rejected or unverified.
	Verdict  string `json:"verdict"`
	Verifier string `json:"verifier,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

type jobRecord struct {
	Kind     string `json:"kind"` // find or verify
	Reviewer string `json:"reviewer"`
	Client   string `json:"client"`
	Model    string `json:"model"`
	Effort   string `json:"effort"`
	// Lens is set for find jobs; Author, for verify jobs, names the
	// reviewer whose findings were checked.
	Lens     string `json:"lens,omitempty"`
	Author   string `json:"author,omitempty"`
	Findings int    `json:"findings"`
	review.Result
}

type runReport struct {
	Version    string      `json:"revkitVersion"`
	Repository string      `json:"repository"`
	Commit     string      `json:"commit"`
	Started    time.Time   `json:"started"`
	Finished   time.Time   `json:"finished"`
	Complete   bool        `json:"complete"`
	Jobs       []jobRecord `json:"jobs"`
	Findings   []finding   `json:"findings"`
}

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func repoReview(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("repo-review", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	configPath := flags.String("config", os.Getenv("REVKIT_CONFIG"), "configuration `FILE` (default $REVKIT_CONFIG)")
	repoName := flags.String("repository", "", "configured repository `NAME` (default: the checkout's directory name)")
	ref := flags.String("ref", "HEAD", "commit to review")
	dryRun := flags.Bool("dry-run", false, "print the plan and stop")
	var lensNames, reviewerNames stringList
	flags.Var(&lensNames, "lens", "run only this lens (repeatable)")
	flags.Var(&reviewerNames, "reviewer", "run only this reviewer (repeatable)")
	flags.Usage = func() {
		fmt.Fprint(os.Stderr, "Usage: revkit repo-review [OPTIONS] [PATH]\n\n")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() > 1 {
		flags.Usage()
		return 2
	}
	path := "."
	if flags.NArg() == 1 {
		path = flags.Arg(0)
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(os.Stderr, "revkit repo-review: "+format+"\n", a...)
		return 2
	}

	if *configPath == "" {
		return fail("no configuration: pass --config FILE or set REVKIT_CONFIG")
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		return fail("%v", err)
	}
	top, err := review.Git(path, "rev-parse", "--show-toplevel")
	if err != nil {
		return fail("%s is not a git checkout: %v", path, err)
	}
	top = strings.TrimSpace(top)
	if *repoName == "" {
		*repoName = filepath.Base(top)
	}
	repo, ok := cfg.repository(*repoName)
	if !ok {
		return fail("repository %s is not configured; pass --repository NAME", *repoName)
	}
	lenses, err := selectLenses(repo.Lenses, lensNames)
	if err != nil {
		return fail("%v", err)
	}
	reviewers, err := selectReviewers(cfg.Reviewers, reviewerNames)
	if err != nil {
		return fail("%v", err)
	}
	commit, err := review.ResolveCommit(top, *ref)
	if err != nil {
		return fail("cannot resolve %s: %v", *ref, err)
	}
	stateDir, err := cfg.stateDir()
	if err != nil {
		return fail("%v", err)
	}
	commitDir := filepath.Join(stateDir, "repo-review", repo.Name, commit)
	tree := filepath.Join(commitDir, "tree")

	fmt.Printf("repo-review %s at %s (%s)\n", repo.Name, commit[:12], *ref)
	for i, r := range reviewers {
		fmt.Printf("  %s (%s %s, %s): %s", r.Name, r.Client, r.Model, r.Effort, lensList(lenses))
		if len(reviewers) > 1 {
			fmt.Printf("; verifies %s", reviewers[(i+len(reviewers)-1)%len(reviewers)].Name)
		}
		fmt.Println()
	}
	if len(reviewers) == 1 {
		fmt.Println("  one reviewer: findings stay unverified")
	}
	fmt.Printf("  %d reviewer runs of up to %s each, then verification\n",
		len(reviewers)*len(lenses), time.Duration(cfg.TimeoutSeconds)*time.Second)
	if *dryRun {
		return 0
	}

	if _, err := os.Stat(tree); err != nil {
		if err := review.WriteTree(top, commit, tree); err != nil {
			if _, statErr := os.Stat(tree); statErr != nil {
				return fail("cannot write the tree: %v", err)
			}
		}
	}
	runs := filepath.Join(commitDir, "runs")
	if err := os.MkdirAll(runs, 0o755); err != nil {
		return fail("%v", err)
	}
	// Each run gets a directory of its own, even when two start in the same
	// second on the same commit.
	runDir, err := os.MkdirTemp(runs, time.Now().UTC().Format("20060102T150405Z")+"-")
	if err != nil {
		return fail("%v", err)
	}
	fmt.Printf("  run directory: %s\n", runDir)

	r := &reviewRun{
		cfg: cfg, repo: repo, tree: tree, dir: runDir, environ: os.Environ(), out: os.Stdout,
		report: runReport{Version: version, Repository: repo.Name, Commit: commit, Started: time.Now().UTC()},
	}
	r.find(ctx, reviewers, lenses)
	r.verify(ctx, reviewers)
	r.report.Finished = time.Now().UTC()
	r.report.Complete = ctx.Err() == nil && r.complete(len(reviewers)*len(lenses))
	if err := r.write(); err != nil {
		return fail("cannot write the report: %v", err)
	}
	r.summary()
	if !r.report.Complete {
		return 1
	}
	return 0
}

func selectLenses(all []lens, names []string) ([]lens, error) {
	if len(names) == 0 {
		return all, nil
	}
	var selected []lens
	for i, name := range names {
		if slices.Contains(names[:i], name) {
			return nil, fmt.Errorf("lens %s is selected twice", name)
		}
		found := false
		for _, l := range all {
			if l.Name == name {
				selected, found = append(selected, l), true
			}
		}
		if !found {
			return nil, fmt.Errorf("lens %s is not configured for this repository", name)
		}
	}
	return selected, nil
}

func selectReviewers(all []review.Reviewer, names []string) ([]review.Reviewer, error) {
	if len(names) == 0 {
		return all, nil
	}
	var selected []review.Reviewer
	for i, name := range names {
		if slices.Contains(names[:i], name) {
			return nil, fmt.Errorf("reviewer %s is selected twice", name)
		}
		found := false
		for _, r := range all {
			if r.Name == name {
				selected, found = append(selected, r), true
			}
		}
		if !found {
			return nil, fmt.Errorf("reviewer %s is not configured", name)
		}
	}
	return selected, nil
}

func lensList(lenses []lens) string {
	names := make([]string, len(lenses))
	for i, l := range lenses {
		names[i] = l.Name
	}
	return strings.Join(names, ", ")
}

type reviewRun struct {
	cfg     *config
	repo    *repository
	tree    string
	dir     string
	environ []string
	out     io.Writer

	mu     sync.Mutex
	report runReport
}

func (r *reviewRun) job(reviewer review.Reviewer, name, prompt string) review.Job {
	return review.Job{
		Reviewer: reviewer, Prompt: prompt, Tree: r.tree, Dir: r.dir, Name: name,
		Timeout: time.Duration(r.cfg.TimeoutSeconds) * time.Second,
		Environ: r.environ, Extra: r.cfg.Environment, HomeLinks: r.cfg.HomeLinks, Readable: r.cfg.ReadablePaths,
	}
}

func (r *reviewRun) logf(format string, a ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fmt.Fprintf(r.out, time.Now().Format("15:04:05")+" "+format+"\n", a...)
}

// find runs every lens with every reviewer: reviewers run in parallel, and
// each runs its lenses one after another.
func (r *reviewRun) find(ctx context.Context, reviewers []review.Reviewer, lenses []lens) {
	var wg sync.WaitGroup
	for _, reviewer := range reviewers {
		wg.Add(1)
		go func(reviewer review.Reviewer) {
			defer wg.Done()
			for _, l := range lenses {
				if ctx.Err() != nil {
					// Record what an interruption skipped, so the run
					// cannot look complete without it.
					record := newRecord("find", reviewer, review.Result{Status: review.StatusInterrupted, Problem: "not started"})
					record.Lens = l.Name
					r.mu.Lock()
					r.report.Jobs = append(r.report.Jobs, record)
					r.mu.Unlock()
					continue
				}
				r.logf("%s %s: started", reviewer.Name, l.Name)
				prompt := findPrompt
				if about := strings.TrimSpace(r.repo.Context); about != "" {
					prompt += "\nRepository:\n" + about + "\n"
				}
				prompt += "\nLens and scope:\n" + strings.TrimSpace(l.Prompt) + "\n"
				result := review.Run(ctx, r.job(reviewer, "find."+reviewer.Name+"."+l.Name, prompt))
				record := newRecord("find", reviewer, result)
				record.Lens = l.Name
				var findings []finding
				if result.Status == review.StatusOK {
					var dropped int
					var problem string
					findings, dropped, problem = parseFindings(result.Text)
					checkReply(&record, problem)
					if record.Status != review.StatusOK {
						findings = nil
					}
					if dropped > 0 {
						record.Notes = append(record.Notes, fmt.Sprintf("dropped %d malformed findings", dropped))
					}
				}
				for i := range findings {
					findings[i].ID = fmt.Sprintf("%s.%s.%d", reviewer.Name, l.Name, i+1)
					findings[i].Reviewer, findings[i].Lens, findings[i].Verdict = reviewer.Name, l.Name, "unverified"
				}
				record.Findings = len(findings)
				r.mu.Lock()
				r.report.Jobs = append(r.report.Jobs, record)
				r.report.Findings = append(r.report.Findings, findings...)
				r.mu.Unlock()
				r.logf("%s %s: %s, %d findings, %ds%s", reviewer.Name, l.Name, record.Status, len(findings),
					record.Seconds, problemSuffix(record))
			}
		}(reviewer)
	}
	wg.Wait()
}

// verify has each reviewer's findings checked by the next reviewer in the
// configuration, so each model checks another model's work.
func (r *reviewRun) verify(ctx context.Context, reviewers []review.Reviewer) {
	if len(reviewers) < 2 || ctx.Err() != nil {
		return
	}
	// Every batch is copied before any verifier starts, since verifiers
	// write their verdicts into the findings.
	batches := make([][]finding, len(reviewers))
	for i, author := range reviewers {
		for _, f := range r.report.Findings {
			if f.Reviewer == author.Name {
				batches[i] = append(batches[i], f)
			}
		}
	}
	var wg sync.WaitGroup
	for i, author := range reviewers {
		checker := reviewers[(i+1)%len(reviewers)]
		batch := batches[i]
		if len(batch) == 0 {
			continue
		}
		wg.Add(1)
		go func(author, checker review.Reviewer, batch []finding) {
			defer wg.Done()
			r.logf("%s verifies %d findings of %s: started", checker.Name, len(batch), author.Name)
			type claim struct {
				ID       string   `json:"id"`
				Title    string   `json:"title"`
				Severity string   `json:"severity"`
				Paths    []string `json:"paths"`
				Claim    string   `json:"claim"`
				Evidence string   `json:"evidence"`
			}
			claims := make([]claim, len(batch))
			for i, f := range batch {
				claims[i] = claim{ID: f.ID, Title: f.Title, Severity: f.Severity, Paths: f.Paths, Claim: f.Claim, Evidence: f.Evidence}
			}
			encoded, _ := json.MarshalIndent(claims, "", "  ")
			result := review.Run(ctx, r.job(checker, "verify."+author.Name+".by."+checker.Name, verifyPrompt+string(encoded)+"\n"))
			record := newRecord("verify", checker, result)
			record.Author = author.Name
			record.Findings = len(batch)
			var verdicts []verdict
			if result.Status == review.StatusOK {
				var problem string
				verdicts, problem = parseVerdicts(result.Text)
				checkReply(&record, problem)
				if record.Status != review.StatusOK {
					verdicts = nil
				}
			}
			applied := 0
			r.mu.Lock()
			for _, v := range verdicts {
				for i := range r.report.Findings {
					f := &r.report.Findings[i]
					if f.ID == v.ID && f.Reviewer == author.Name && f.Verifier == "" {
						f.Verdict, f.Verifier, f.Reason = v.Verdict, checker.Name, v.Reason
						applied++
					}
				}
			}
			if record.Status == review.StatusOK && applied < len(batch) {
				// A verifier that leaves findings without a verdict did
				// not finish its review, however it replied.
				record.Status, record.Problem = statusUnparsed,
					fmt.Sprintf("%d of %d findings got no verdict", len(batch)-applied, len(batch))
			}
			r.report.Jobs = append(r.report.Jobs, record)
			r.mu.Unlock()
			r.logf("%s verifies %s: %s, %d of %d verdicts, %ds%s", checker.Name, author.Name, record.Status,
				applied, len(batch), record.Seconds, problemSuffix(record))
		}(author, checker, batch)
	}
	wg.Wait()
}

// checkReply fails a job whose reply could not be read, that reported an
// error, or whose reviewer made no successful tool call: none of these
// reviewed the tree, whatever the reply says. One successful call does not
// prove the reviewer read what it needed; it rules out a reviewer that was
// blocked or could not start its tools, the failure seen in practice.
func checkReply(record *jobRecord, problem string) {
	switch {
	case problem != "":
		record.Status, record.Problem = statusUnparsed, problem
	case record.Reads == 0:
		record.Status, record.Problem = review.StatusFailed, "the reviewer made no successful tool call"
	}
}

func newRecord(kind string, reviewer review.Reviewer, result review.Result) jobRecord {
	return jobRecord{Kind: kind, Reviewer: reviewer.Name, Client: reviewer.Client, Model: reviewer.Model,
		Effort: reviewer.Effort, Result: result}
}

func problemSuffix(record jobRecord) string {
	if record.Status == review.StatusOK {
		return ""
	}
	return fmt.Sprintf(" (%s; see %s)", record.Problem, record.Log)
}

// complete reports whether all planned find jobs ran and reviewed, every
// verification reviewed and, in a run with more than one reviewer, every
// finding got a verdict.
func (r *reviewRun) complete(planned int) bool {
	reviewers := map[string]bool{}
	found := 0
	for _, job := range r.report.Jobs {
		if job.Status != review.StatusOK {
			return false
		}
		if job.Kind == "find" {
			reviewers[job.Reviewer] = true
			found++
		}
	}
	if planned == 0 || found != planned {
		return false
	}
	if len(reviewers) < 2 {
		return true
	}
	for _, f := range r.report.Findings {
		if f.Verdict == "unverified" {
			return false
		}
	}
	return true
}

// parseFindings reads {"findings": [...]} from a reply and counts entries
// it could not read. A reply without that list, or with an error, returns
// the problem instead.
func parseFindings(text string) ([]finding, int, string) {
	var reply struct {
		Findings []json.RawMessage `json:"findings"`
		Error    string            `json:"error"`
	}
	if problem := decodeReply(text, &reply, &reply.Error); problem != "" {
		return nil, 0, problem
	}
	if reply.Findings == nil {
		return nil, 0, "the reply is not the asked-for JSON"
	}
	findings := []finding{}
	dropped := 0
	for _, raw := range reply.Findings {
		var f finding
		if json.Unmarshal(raw, &f) != nil || (strings.TrimSpace(f.Claim) == "" && strings.TrimSpace(f.Title) == "") {
			dropped++
			continue
		}
		f.Severity = strings.ToLower(strings.TrimSpace(f.Severity))
		f.ID, f.Reviewer, f.Lens, f.Verdict, f.Verifier, f.Reason = "", "", "", "", "", ""
		findings = append(findings, f)
	}
	if len(findings) == 0 && dropped > 0 {
		return nil, dropped, "no finding in the reply is in the asked-for shape"
	}
	return findings, dropped, ""
}

type verdict struct {
	ID      string `json:"id"`
	Verdict string `json:"verdict"`
	Reason  string `json:"reason"`
}

// parseVerdicts reads {"verdicts": [...]} from a reply, keeping verdicts of
// the three known kinds, or returns the problem.
func parseVerdicts(text string) ([]verdict, string) {
	var reply struct {
		Verdicts []json.RawMessage `json:"verdicts"`
		Error    string            `json:"error"`
	}
	if problem := decodeReply(text, &reply, &reply.Error); problem != "" {
		return nil, problem
	}
	if reply.Verdicts == nil {
		return nil, "the reply is not the asked-for JSON"
	}
	verdicts := []verdict{}
	for _, raw := range reply.Verdicts {
		var v verdict
		if json.Unmarshal(raw, &v) != nil {
			continue
		}
		v.Verdict = strings.ToLower(strings.TrimSpace(v.Verdict))
		if v.Verdict == "confirmed" || v.Verdict == "partly" || v.Verdict == "rejected" {
			verdicts = append(verdicts, v)
		}
	}
	return verdicts, ""
}

// decodeReply decodes a reply into value and returns a problem when it is
// not JSON or reports an error in *reported.
func decodeReply(text string, value any, reported *string) string {
	if !decodeObject(text, value) {
		return "the reply is not the asked-for JSON"
	}
	if message := strings.TrimSpace(*reported); message != "" {
		return "the reviewer reported: " + oneLine(message)
	}
	return ""
}

// decodeObject decodes the outermost JSON object of text, which may sit in
// a code fence or between short remarks.
func decodeObject(text string, value any) bool {
	start, end := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if start < 0 || end < start {
		return false
	}
	return json.Unmarshal([]byte(text[start:end+1]), value) == nil
}
