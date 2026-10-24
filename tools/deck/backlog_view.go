package main

import (
	"fmt"
	"net/url"
	"sort"
	"time"
)

// BacklogPageView is everything the backlog page shows.
type BacklogPageView struct {
	BasePath    string
	GeneratedAt time.Time
	HasRun      bool
	AsOf        time.Time
	Running     bool
	Err         string
	ErrAt       time.Time
	User        string
	Sources     []string
	Missing     int
	Issues      int
	PRs         int
	MaybeDone   int
	Stale       int
	Ready       int
	Sections    []BacklogSectionView
	Empty       bool
	// Verdicts: why they are off, or the queue and the shared budget.
	VerdictsOff  string
	Queued       int
	Judging      bool
	BudgetLeft   int
	CallsPerHour int
	// SeenUpTo is when the shown facts were read, for "new since your
	// last visit".
	SeenUpTo time.Time
}

type BacklogSectionView struct {
	Key   string
	Title string
	Note  string
	Count int
	Subs  []BacklogSubView
}

type BacklogSubView struct {
	Title     string
	Collapsed bool
	Rows      []BacklogRowView
}

// LinkView is a reference shown as a link when its URL is safe to link.
type LinkView struct {
	Text string
	URL  string
}

type BacklogRowView struct {
	Key        string
	Kind       string
	Ref        string
	Title      string
	URL        string
	Repo       string
	Lane       string
	LaneLabel  string
	Flags      []string
	IdleDays   int
	Idle       string
	UpdatedAt  string
	Author     string
	Checks     string
	Next       string
	Owner      string
	Thread     string
	Links      []LinkView
	Related    []string
	Suggestion string

	// The verdict of a shortlisted issue, its age and the refresh form.
	HasVerdict      bool
	Verdict         string
	VerdictReason   string
	Criteria        string
	VerdictOutdated bool
	VerdictModel    string
	VerdictRole     string
	VerdictAt       time.Time
	VerdictError    string
	VerdictState    string
	CanJudge        bool
	Roles           []string
	DefaultRole     string
}

var verdictSuggestions = map[string]string{
	"close":    "Close",
	"drop":     "Close as not planned",
	"dispatch": "Dispatch",
	"refresh":  "Refresh the status",
	"keep":     "Keep",
	"unclear":  "Look at it",
}

// withVerdict adds a shortlisted issue's cached verdict and its refresh
// form; a verdict suggests the action in place of the facts' guess.
func (b *Backlog) withVerdict(row BacklogRowView, issue BacklogIssue, queued map[string]bool) BacklogRowView {
	if !b.verdictsEnabled() {
		return row
	}
	row.CanJudge = true
	for _, role := range b.deck.cfg.Threads.SummaryRoles {
		if len(b.deck.models[role]) > 0 {
			row.Roles = append(row.Roles, role)
		}
	}
	row.DefaultRole = b.cfg.VerdictRole
	switch {
	case b.judging == issue.Ref:
		row.VerdictState = "being judged"
	case queued[issue.Ref]:
		row.VerdictState = "queued"
	}
	e, ok := b.cache.get(issue.Ref)
	if !ok {
		return row
	}
	if e.Verdict != nil {
		row.HasVerdict = true
		row.Verdict = e.Verdict.Verdict
		row.VerdictReason = e.Verdict.Reason
		row.Criteria = deref(e.Verdict.Criteria)
		row.VerdictOutdated = e.Basis != verdictBasis(issue)
		row.VerdictModel, row.VerdictRole, row.VerdictAt = e.Model, e.Role, e.MadeAt
		if s, ok := verdictSuggestions[e.Verdict.Verdict]; ok && !row.VerdictOutdated {
			row.Suggestion = s
		}
	}
	if e.Error != "" && e.AttemptedAt.After(e.MadeAt) {
		row.VerdictError = e.Error
	}
	return row
}

var issueLaneLabels = map[string]string{
	"close":     "Close",
	"inflight":  "In flight",
	"stale":     "Stale",
	"active":    "Active",
	"dashboard": "Dashboard",
}

var prLaneLabels = map[string]string{
	"ready":    "Ready to merge",
	"review":   "Needs your review",
	"blocked":  "Blocked",
	"waiting":  "Waiting",
	"stale":    "Stale",
	"inflight": "In flight",
	"open":     "Open",
	"renovate": "Renovate",
}

// prLaneOrder lists PR lanes most actionable first; the last ones are
// collapsed.
var prLaneOrder = []string{"ready", "review", "blocked", "waiting", "stale", "inflight", "open", "renovate"}

var issueRestOrder = []string{"close", "stale", "active", "inflight"}

var prSuggestions = map[string]string{
	"ready":   "Merge",
	"review":  "Review",
	"blocked": "Unblock",
	"stale":   "Close or revive",
}

// safeURL keeps only http and https links, so a row cannot carry a script
// URL into the page.
func safeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return ""
	}
	return raw
}

func laneLabel(labels map[string]string, lane string) string {
	if l, ok := labels[lane]; ok {
		return l
	}
	return lane
}

func issueRow(issue BacklogIssue, related map[string]BacklogRelated, suggestion string) BacklogRowView {
	row := BacklogRowView{
		Key:        "issue:" + issue.Ref,
		Kind:       "issue",
		Ref:        issue.Ref,
		Title:      issue.Title,
		URL:        safeURL(issue.URL),
		Repo:       issue.Repo,
		Lane:       issue.Lane,
		LaneLabel:  laneLabel(issueLaneLabels, issue.Lane),
		Flags:      issue.Flags,
		IdleDays:   issue.IdleDays,
		Idle:       daysText(issue.IdleDays),
		UpdatedAt:  issue.UpdatedAt,
		Author:     issue.Author,
		Suggestion: suggestion,
	}
	if issue.Checks != nil && issue.Checks.Total > 0 {
		row.Checks = fmt.Sprintf("%d/%d checked", issue.Checks.Done, issue.Checks.Total)
	}
	if issue.Status != nil {
		if issue.Status.Next != nil {
			row.Next = *issue.Status.Next
		}
		if issue.Status.Owner != nil {
			row.Owner = *issue.Status.Owner
		}
	}
	if issue.Thread != nil {
		row.Thread = issue.Thread.Title
	}
	for _, pr := range issue.MergedPRs {
		row.Links = append(row.Links, LinkView{Text: "merged " + pr.Ref, URL: safeURL(pr.URL)})
	}
	for _, ref := range issue.OpenPRs {
		row.Links = append(row.Links, LinkView{Text: "open " + ref})
	}
	for _, ref := range issue.Related {
		if r, ok := related[ref]; ok {
			row.Related = append(row.Related, fmt.Sprintf("%s (%s): %s", r.Ref, r.State, r.Title))
		}
	}
	return row
}

func prRow(pr BacklogPR) BacklogRowView {
	row := BacklogRowView{
		Key:        "pr:" + pr.Ref,
		Kind:       "pr",
		Ref:        pr.Ref,
		Title:      pr.Title,
		URL:        safeURL(pr.URL),
		Repo:       pr.Repo,
		Lane:       pr.Lane,
		LaneLabel:  laneLabel(prLaneLabels, pr.Lane),
		Flags:      pr.Flags,
		IdleDays:   pr.IdleDays,
		Idle:       daysText(pr.IdleDays),
		UpdatedAt:  pr.UpdatedAt,
		Author:     pr.Author,
		Suggestion: prSuggestions[pr.Lane],
	}
	if pr.CI != "" && pr.CI != "none" {
		row.Checks = "CI " + pr.CI
	}
	if pr.Thread != nil {
		row.Thread = pr.Thread.Title
	}
	for _, ref := range pr.Issues {
		row.Links = append(row.Links, LinkView{Text: "for " + ref})
	}
	return row
}

func daysText(days int) string {
	switch days {
	case 0:
		return "today"
	case 1:
		return "1 day"
	}
	return fmt.Sprintf("%d days", days)
}

// view builds the backlog page from the last good inventory.
func (b *Backlog) view(basePath string) BacklogPageView {
	b.mu.Lock()
	defer b.mu.Unlock()
	v := BacklogPageView{
		BasePath:    basePath,
		GeneratedAt: b.now(),
		HasRun:      b.inv != nil,
		AsOf:        b.asOf,
		Running:     b.running,
		Err:         b.err,
		ErrAt:       b.errAt,
		SeenUpTo:    b.asOf,
	}
	v.VerdictsOff = b.verdictsOff()
	if b.verdictsEnabled() {
		v.Queued, v.Judging = len(b.queue), b.judging != ""
		v.BudgetLeft, v.CallsPerHour = b.deck.budgetLeft(), b.deck.cfg.Threads.CallsPerHour
	}
	queued := map[string]bool{}
	for _, ref := range b.queue {
		queued[ref] = true
	}
	if b.inv == nil {
		v.Empty = true
		return v
	}
	inv := b.inv
	v.User = inv.User
	v.Missing = len(inv.Scope.Missing)
	for _, name := range []string{"forgejo", "github", "threads", "related"} {
		state, ok := inv.Sources[name]
		if ok && name != "forgejo" && state != "ok" {
			v.Sources = append(v.Sources, name+": "+state)
		}
	}
	related := map[string]BacklogRelated{}
	for _, r := range inv.Related {
		related[r.Ref] = r
	}

	maybeDone, stale := shortlist(inv.Issues)
	shortlisted := map[string]bool{}
	var doneRows, staleRows []BacklogRowView
	for _, issue := range maybeDone {
		shortlisted[issue.Ref] = true
		suggestion := "Check whether it is done"
		if issue.Lane == "close" {
			suggestion = "Close?"
		}
		doneRows = append(doneRows, b.withVerdict(issueRow(issue, related, suggestion), issue, queued))
	}
	for _, issue := range stale {
		shortlisted[issue.Ref] = true
		staleRows = append(staleRows, b.withVerdict(issueRow(issue, related, "Drop, refresh or dispatch?"), issue, queued))
	}
	v.MaybeDone, v.Stale = len(doneRows), len(staleRows)
	if len(doneRows) > 0 {
		v.Sections = append(v.Sections, BacklogSectionView{
			Key: "maybe-done", Title: "Maybe done", Count: len(doneRows),
			Note: "In the close lane, or untouched since work naming them merged, or every related item closed.",
			Subs: []BacklogSubView{{Rows: doneRows}},
		})
	}
	if len(staleRows) > 0 {
		v.Sections = append(v.Sections, BacklogSectionView{
			Key: "stale", Title: "Stale", Count: len(staleRows),
			Note: fmt.Sprintf("The idlest issues untouched for %d days or more.", inv.StaleDays),
			Subs: []BacklogSubView{{Rows: staleRows}},
		})
	}

	byLane := map[string][]BacklogRowView{}
	for _, pr := range inv.PRs {
		byLane[pr.Lane] = append(byLane[pr.Lane], prRow(pr))
		if pr.Lane == "ready" {
			v.Ready++
		}
	}
	prs := BacklogSectionView{Key: "prs", Title: "Pull requests", Count: len(inv.PRs)}
	for _, lane := range orderedLanes(prLaneOrder, byLane) {
		prs.Subs = append(prs.Subs, BacklogSubView{
			Title:     laneLabel(prLaneLabels, lane),
			Collapsed: lane == "renovate" || lane == "inflight" || lane == "open",
			Rows:      byLane[lane],
		})
	}
	if prs.Count > 0 {
		v.Sections = append(v.Sections, prs)
	}

	rest := map[string][]BacklogRowView{}
	count := 0
	for _, issue := range inv.Issues {
		if shortlisted[issue.Ref] || issue.Lane == "dashboard" {
			continue
		}
		rest[issue.Lane] = append(rest[issue.Lane], issueRow(issue, related, ""))
		count++
	}
	other := BacklogSectionView{Key: "issues", Title: "Other issues", Count: count}
	for _, lane := range orderedLanes(issueRestOrder, rest) {
		other.Subs = append(other.Subs, BacklogSubView{Title: laneLabel(issueLaneLabels, lane), Collapsed: true, Rows: rest[lane]})
	}
	if count > 0 {
		v.Sections = append(v.Sections, other)
	}
	v.Issues, v.PRs = len(inv.Issues), len(inv.PRs)
	v.Empty = v.Issues == 0 && v.PRs == 0
	return v
}

// orderedLanes lists the lanes present in rows, the known ones in order and
// any others after them, sorted.
func orderedLanes(order []string, rows map[string][]BacklogRowView) []string {
	var lanes []string
	for _, lane := range order {
		if len(rows[lane]) > 0 {
			lanes = append(lanes, lane)
		}
	}
	var unknown []string
	for lane := range rows {
		if !oneOf(lane, order) {
			unknown = append(unknown, lane)
		}
	}
	sort.Strings(unknown)
	return append(lanes, unknown...)
}
