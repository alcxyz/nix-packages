package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// The backlog view (ADR-0014) shows backlog-inventory's facts live. deck
// holds no forge credentials: the inventory command the consumer configures is
// its only view of the forges.

// Shortlist limits, as the backlog overview skill's: the issues that may be
// done, then the idlest stale ones.
const (
	shortlistMaybeDone = 24
	shortlistStale     = 12
)

// backlogRefRE is the inventory's item reference: OWNER/REPO#N on Forgejo,
// gh:OWNER/REPO#N on GitHub.
var backlogRefRE = regexp.MustCompile(`^(gh:)?[\w.-]+/[\w.-]+#[1-9][0-9]*$`)

// validBacklogRef refuses a leading dash as well, so a reference can never
// be read as an option.
func validBacklogRef(ref string) bool {
	return backlogRefRE.MatchString(ref) && !strings.HasPrefix(ref, "-")
}

// BacklogInventory is the part of backlog-inventory's output deck reads.
type BacklogInventory struct {
	GeneratedAt string            `json:"generatedAt"`
	User        string            `json:"user"`
	StaleDays   int               `json:"staleDays"`
	Scope       BacklogScope      `json:"scope"`
	Sources     map[string]string `json:"sources"`
	Issues      []BacklogIssue    `json:"issues"`
	PRs         []BacklogPR       `json:"prs"`
	Related     []BacklogRelated  `json:"related"`
}

type BacklogScope struct {
	Missing []string `json:"missing"`
}

type BacklogChecks struct {
	Done  int `json:"done"`
	Total int `json:"total"`
}

// BacklogStatus is an issue's status line, as the inventory parsed it.
type BacklogStatus struct {
	Date    string  `json:"date"`
	AgeDays *int    `json:"ageDays"`
	Owner   *string `json:"owner"`
	Next    *string `json:"next"`
}

type BacklogMergedPR struct {
	Ref      string `json:"ref"`
	URL      string `json:"url"`
	Closes   bool   `json:"closes"`
	MergedAt string `json:"mergedAt"`
}

type BacklogThread struct {
	ThreadID string `json:"threadId"`
	Title    string `json:"title"`
	Bucket   string `json:"bucket"`
	Status   string `json:"status"`
}

type BacklogIssue struct {
	Ref       string            `json:"ref"`
	Forge     string            `json:"forge"`
	Repo      string            `json:"repo"`
	Number    int               `json:"number"`
	URL       string            `json:"url"`
	Title     string            `json:"title"`
	Author    string            `json:"author"`
	AgeDays   int               `json:"ageDays"`
	IdleDays  int               `json:"idleDays"`
	UpdatedAt string            `json:"updatedAt"`
	Labels    []string          `json:"labels"`
	Milestone *string           `json:"milestone"`
	Checks    *BacklogChecks    `json:"checks"`
	Status    *BacklogStatus    `json:"status"`
	OpenPRs   []string          `json:"openPrs"`
	MergedPRs []BacklogMergedPR `json:"mergedPrs"`
	Thread    *BacklogThread    `json:"thread"`
	Lane      string            `json:"lane"`
	Flags     []string          `json:"flags"`
	Related   []string          `json:"related"`
}

type BacklogPR struct {
	Ref       string         `json:"ref"`
	Forge     string         `json:"forge"`
	Repo      string         `json:"repo"`
	Number    int            `json:"number"`
	URL       string         `json:"url"`
	Title     string         `json:"title"`
	Author    string         `json:"author"`
	Kind      string         `json:"kind"`
	Draft     bool           `json:"draft"`
	Base      string         `json:"base"`
	CI        string         `json:"ci"`
	Review    string         `json:"review"`
	Issues    []string       `json:"issues"`
	IdleDays  int            `json:"idleDays"`
	UpdatedAt string         `json:"updatedAt"`
	Thread    *BacklogThread `json:"thread"`
	Lane      string         `json:"lane"`
	Flags     []string       `json:"flags"`
}

type BacklogRelated struct {
	Ref   string `json:"ref"`
	Kind  string `json:"kind"`
	State string `json:"state"`
	Title string `json:"title"`
}

// parseBacklogInventory checks what deck relies on: every row has a valid
// reference, and links are http or https, so no row can carry a script URL
// into the page.
func parseBacklogInventory(data []byte) (BacklogInventory, error) {
	var inv BacklogInventory
	if err := json.Unmarshal(data, &inv); err != nil {
		return BacklogInventory{}, fmt.Errorf("parse backlog inventory: %s", jsonErrorKind(err))
	}
	for i, issue := range inv.Issues {
		if !validBacklogRef(issue.Ref) {
			return BacklogInventory{}, fmt.Errorf("parse backlog inventory: issue %d has an invalid ref", i+1)
		}
	}
	for i, pr := range inv.PRs {
		if !validBacklogRef(pr.Ref) {
			return BacklogInventory{}, fmt.Errorf("parse backlog inventory: pull request %d has an invalid ref", i+1)
		}
	}
	return inv, nil
}

// shortlist picks the issues that get verdicts, as the skill's overview
// does: up to shortlistMaybeDone in the close lane or flagged as possibly
// done (unless in the inflight lane), in inventory order, then up to
// shortlistStale other stale issues, idlest first.
func shortlist(issues []BacklogIssue) (maybeDone, stale []BacklogIssue) {
	var rest []BacklogIssue
	for _, issue := range issues {
		flagged := oneOf("untouched-since-merge", issue.Flags) || oneOf("related-closed", issue.Flags)
		switch {
		case issue.Lane == "close" || (issue.Lane != "inflight" && flagged):
			if len(maybeDone) < shortlistMaybeDone {
				maybeDone = append(maybeDone, issue)
			}
		case issue.Lane == "stale":
			rest = append(rest, issue)
		}
	}
	sort.SliceStable(rest, func(i, j int) bool { return rest[i].IdleDays > rest[j].IdleDays })
	if len(rest) > shortlistStale {
		rest = rest[:shortlistStale]
	}
	return maybeDone, rest
}

// Backlog keeps the last good inventory and refreshes it on an interval,
// and judges the shortlisted issues one at a time.
type Backlog struct {
	cfg BacklogConfig
	log *slog.Logger
	now func() time.Time
	// deck lends its model roles, model runner and call budget; cache holds
	// the verdicts. Without either, or without an item command, the view
	// shows facts only.
	deck  *Deck
	cache *VerdictCache

	mu      sync.Mutex
	inv     *BacklogInventory
	asOf    time.Time
	err     string
	errAt   time.Time
	running bool
	// queue holds the issues asked for by hand, first come first; manual
	// has the role each was asked with. judging is the issue being judged,
	// and judgingBasis the basis it is judged against, once read.
	queue           []string
	manual          map[string]string
	judging         string
	judgingBasis    string
	refreshVerdicts bool

	refreshCh chan struct{}
	wake      chan struct{}
}

func newBacklog(cfg BacklogConfig, log *slog.Logger) *Backlog {
	return &Backlog{
		cfg: cfg, log: log, now: time.Now,
		manual:    map[string]string{},
		refreshCh: make(chan struct{}, 1),
		wake:      make(chan struct{}, 1),
	}
}

// loop refreshes the inventory every intervalMinutes and when asked, until
// ctx ends.
func (b *Backlog) loop(ctx context.Context) {
	ticker := time.NewTicker(time.Duration(b.cfg.IntervalMinutes) * time.Minute)
	defer ticker.Stop()
	for {
		b.refresh(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-b.refreshCh:
		}
	}
}

// requestRefresh asks the loop for a refresh without waiting for it;
// requests made while one runs coalesce into one more.
func (b *Backlog) requestRefresh() {
	select {
	case b.refreshCh <- struct{}{}:
	default:
	}
}

// refresh runs the inventory command under its deadline. A failure keeps
// the last good inventory and records the error. The facts are dated from
// when the read started, so a change made during a slow read is not marked
// as already seen. Then the verdict worker is woken for what changed.
func (b *Backlog) refresh(ctx context.Context) {
	b.mu.Lock()
	b.running = true
	started := b.now()
	// A Refresh now asked for during this read waits for the next one,
	// which reads what changed since this one started.
	refreshVerdicts := b.refreshVerdicts
	b.refreshVerdicts = false
	b.mu.Unlock()
	inv, err := b.readInventory(ctx)
	b.mu.Lock()
	b.running = false
	switch {
	case err != nil && ctx.Err() != nil:
		b.mu.Unlock()
		return // shutting down; nothing went wrong with the facts or the requests
	case err != nil:
		b.log.Warn("backlog inventory failed", "err", err.Error())
		b.err = err.Error()
		b.errAt = b.now()
	default:
		b.inv = &inv
		b.asOf = started
		b.err = ""
	}
	// On failure, Refresh now queues verdicts from the last good facts.
	if refreshVerdicts {
		b.queueOutdatedLocked()
	}
	b.mu.Unlock()
	if err == nil && inventoryComplete(inv) {
		b.pruneVerdicts(inv)
	}
	b.signal()
}

// inventoryComplete reports whether every source the inventory read
// answered, so an issue missing from it has really left: a failed GitHub
// read or a scoped repository not found must not prune good verdicts.
func inventoryComplete(inv BacklogInventory) bool {
	for _, state := range inv.Sources {
		if strings.HasPrefix(state, "error") {
			return false
		}
	}
	return len(inv.Scope.Missing) == 0
}

func (b *Backlog) signal() {
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

func (b *Backlog) readInventory(parent context.Context) (BacklogInventory, error) {
	timeout := time.Duration(b.cfg.TimeoutMinutes) * time.Minute
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	name := b.cfg.Commands.Inventory
	cmd := exec.CommandContext(ctx, name)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := runGroup(cmd); err != nil {
		if parent.Err() != nil {
			return BacklogInventory{}, fmt.Errorf("%s stopped: %w", name, parent.Err())
		}
		if ctx.Err() == context.DeadlineExceeded {
			return BacklogInventory{}, fmt.Errorf("%s timed out after %s", name, timeout)
		}
		return BacklogInventory{}, fmt.Errorf("%s: %w%s", name, err, stderrHint(stderr.Bytes()))
	}
	return parseBacklogInventory(stdout.Bytes())
}
