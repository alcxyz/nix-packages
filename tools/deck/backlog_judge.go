package main

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// The verdict worker judges one shortlisted issue at a time: issues asked
// for by hand first, then automatic ones while more than a quarter of the
// hour's call budget is left, so the backlog cannot starve thread summaries.

var errNotShortlisted = errors.New("the issue is not on the verdict shortlist")

// verdictsEnabled reports whether verdicts can be made at all.
func (b *Backlog) verdictsEnabled() bool {
	return b.cfg.Commands.Item != "" && b.cache != nil && b.deck != nil
}

// verdictsOff says why there are no verdicts, or "" when there are.
func (b *Backlog) verdictsOff() string {
	switch {
	case b.cfg.Commands.Item == "":
		return "no item command is configured"
	case b.cache == nil || b.deck == nil:
		return "the verdict cache is unavailable"
	case len(b.deck.models[b.cfg.VerdictRole]) == 0:
		return fmt.Sprintf("role %q has no usable no-tools model", b.cfg.VerdictRole)
	}
	return ""
}

// modelsFor returns the role's entries, at verdictEffort for the verdict
// role; another role picked by hand keeps its own effort.
func (b *Backlog) modelsFor(role string) []ModelConfig {
	models := b.deck.models[role]
	if role != b.cfg.VerdictRole || *b.cfg.VerdictEffort == "" {
		return models
	}
	out := make([]ModelConfig, len(models))
	for i, m := range models {
		m.Effort = *b.cfg.VerdictEffort
		out[i] = m
	}
	return out
}

// shortlistedLocked finds a shortlisted issue in the current facts.
func (b *Backlog) shortlistedLocked(ref string) (BacklogIssue, bool) {
	if b.inv == nil {
		return BacklogIssue{}, false
	}
	maybeDone, stale := shortlist(b.inv.Issues)
	for _, issue := range append(maybeDone, stale...) {
		if issue.Ref == ref {
			return issue, true
		}
	}
	return BacklogIssue{}, false
}

// attempted reports whether the cache holds an attempt at basis that is not
// due for a retry: a verdict, a failure that is not retried, or one whose
// backoff still runs.
func attempted(e VerdictEntry, ok bool, basis string, now time.Time) bool {
	return ok && e.AttemptBasis == basis && (e.RetryAt.IsZero() || now.Before(e.RetryAt))
}

// waitsForInterval reports whether a changed issue must still wait for the
// verdict interval after its last attempt, failed or not and by hand or not.
func waitsForInterval(e VerdictEntry, ok bool, basis string, now time.Time, interval time.Duration) bool {
	return ok && e.AttemptBasis != basis && !e.AttemptedAt.IsZero() && now.Sub(e.AttemptedAt) < interval
}

// nextAutomaticLocked picks the first shortlisted issue due for a verdict.
func (b *Backlog) nextAutomaticLocked() (string, bool) {
	if b.inv == nil {
		return "", false
	}
	now := b.now()
	interval := time.Duration(*b.cfg.VerdictIntervalMinutes) * time.Minute
	maybeDone, stale := shortlist(b.inv.Issues)
	for _, issue := range append(maybeDone, stale...) {
		basis := verdictBasis(issue)
		e, ok := b.cache.get(issue.Ref)
		if attempted(e, ok, basis, now) || waitsForInterval(e, ok, basis, now, interval) {
			continue
		}
		return issue.Ref, true
	}
	return "", false
}

// queueVerdictLocked puts an issue at the front of the queue with a role;
// the latest request for an issue replaces a pending one.
func (b *Backlog) queueVerdictLocked(ref, role string) {
	b.manual[ref] = role
	q := []string{ref}
	for _, r := range b.queue {
		if r != ref {
			q = append(q, r)
		}
	}
	b.queue = q
}

// queueOutdatedLocked queues, for Refresh now, every shortlisted issue
// without a verdict for its current basis, behind the ones already asked
// for by hand.
func (b *Backlog) queueOutdatedLocked() {
	if !b.verdictsEnabled() || b.inv == nil || len(b.deck.models[b.cfg.VerdictRole]) == 0 {
		return
	}
	maybeDone, stale := shortlist(b.inv.Issues)
	for _, issue := range append(maybeDone, stale...) {
		if _, queued := b.manual[issue.Ref]; queued {
			continue
		}
		// The running verdict covers its issue unless the issue has
		// changed since it started.
		if issue.Ref == b.judging && verdictBasis(issue) == b.judgingBasis {
			continue
		}
		if e, ok := b.cache.get(issue.Ref); ok && e.Verdict != nil && e.Basis == verdictBasis(issue) {
			continue
		}
		b.manual[issue.Ref] = b.cfg.VerdictRole
		b.queue = append(b.queue, issue.Ref)
	}
}

// requestVerdict queues a shortlisted issue for Refresh verdict. It skips
// the reserve, the interval and the backoff, not the budget.
func (b *Backlog) requestVerdict(ref, role string) error {
	if !b.verdictsEnabled() {
		return errors.New("verdicts are off: " + b.verdictsOff())
	}
	if len(b.deck.models[role]) == 0 {
		return fmt.Errorf("unknown or unusable role %q", role)
	}
	b.mu.Lock()
	if _, ok := b.shortlistedLocked(ref); !ok {
		b.mu.Unlock()
		return errNotShortlisted
	}
	b.queueVerdictLocked(ref, role)
	b.mu.Unlock()
	b.signal()
	return nil
}

// requestRefreshAll reads the facts now, then queues every shortlisted
// issue whose verdict is missing or outdated.
func (b *Backlog) requestRefreshAll() {
	b.mu.Lock()
	b.refreshVerdicts = true
	b.mu.Unlock()
	b.requestRefresh()
}

// verdictLoop runs verdicts until ctx ends, woken by refreshes and
// requests, and once a minute for budget that came free and backoffs that
// ran out.
func (b *Backlog) verdictLoop(ctx context.Context) {
	if !b.verdictsEnabled() {
		return
	}
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		for ctx.Err() == nil && b.judgeNext(ctx) {
		}
		select {
		case <-ctx.Done():
			return
		case <-b.wake:
		case <-ticker.C:
		}
	}
}

// reserve is the part of the hour's budget automatic verdicts leave to
// summaries.
func (b *Backlog) reserve() int {
	return b.deck.cfg.Threads.CallsPerHour / 4
}

// judgeNext judges the next issue, if one may run now, and reports whether
// it did.
func (b *Backlog) judgeNext(ctx context.Context) bool {
	if !b.verdictsEnabled() {
		return false
	}
	b.mu.Lock()
	ref, role, manual := "", "", false
	if len(b.queue) > 0 && b.deck.budgetLeft() > 0 {
		// A request by hand waits for budget before its item is read.
		ref, b.queue = b.queue[0], b.queue[1:]
		role, manual = b.manual[ref], true
		delete(b.manual, ref)
	} else if len(b.queue) == 0 && len(b.deck.models[b.cfg.VerdictRole]) > 0 && b.deck.budgetLeft() > b.reserve() {
		ref, _ = b.nextAutomaticLocked()
		role = b.cfg.VerdictRole
	}
	if ref == "" {
		b.mu.Unlock()
		return false
	}
	// Take the issue, and record the basis it is judged against, under the
	// lock that picked it: judge works on this snapshot, so a Refresh now
	// sees exactly the basis being judged.
	issue, ok := b.shortlistedLocked(ref)
	if !ok {
		b.mu.Unlock()
		return true // left the shortlist since it was queued; try the next
	}
	related := map[string]BacklogRelated{}
	for _, r := range b.inv.Related {
		related[r.Ref] = r
	}
	b.judging, b.judgingBasis = ref, verdictBasis(issue)
	b.mu.Unlock()

	err := b.judge(ctx, issue, related, role, manual)

	b.mu.Lock()
	defer b.mu.Unlock()
	b.judging, b.judgingBasis = "", ""
	if errors.Is(err, errBudget) {
		// Wait for budget; a request by hand keeps its place.
		if manual {
			if _, again := b.manual[ref]; !again {
				b.queueVerdictLocked(ref, role)
			}
		}
		return false
	}
	if err != nil && ctx.Err() == nil {
		b.log.Warn("verdict failed", "issue", ref, "err", err.Error())
	} else if err == nil {
		b.log.Info("verdict updated", "issue", ref, "role", role)
	}
	return ctx.Err() == nil
}

// judge makes one verdict and records the outcome in the cache. On failure
// the previous verdict stays.
func (b *Backlog) judge(ctx context.Context, issue BacklogIssue, related map[string]BacklogRelated, role string, manual bool) error {
	ref := issue.Ref
	basis := verdictBasis(issue)
	now := b.now()
	e, cached := b.cache.get(ref)
	if !manual && (attempted(e, cached, basis, now) ||
		waitsForInterval(e, cached, basis, now, time.Duration(*b.cfg.VerdictIntervalMinutes)*time.Minute)) {
		return nil
	}
	models := b.modelsFor(role)
	if len(models) == 0 {
		return b.recordFailure(ctx, ref, basis, fmt.Errorf("role %q is unavailable", role), false)
	}
	item, err := b.readItem(ctx, issue)
	if err != nil {
		return b.recordFailure(ctx, ref, basis, err, true)
	}
	prompt, err := buildVerdictPrompt(issue, item, related)
	if err != nil {
		return b.recordFailure(ctx, ref, basis, err, false)
	}
	allowCall := b.deck.takeCall
	if !manual {
		// Each automatic call, a backup's included, keeps the reserve.
		allowCall = func() bool { return b.deck.takeCallAbove(b.reserve()) }
	}
	out, model, err := callModel(ctx, b.deck.runModel, models, prompt, allowCall)
	if errors.Is(err, errBudget) {
		return err
	}
	if err != nil {
		return b.recordFailure(ctx, ref, basis, err, true)
	}
	v, err := parseVerdict(out)
	if err != nil {
		return b.recordFailure(ctx, ref, basis, fmt.Errorf("%s: %w", model, err), false)
	}
	at := b.now()
	return b.cache.put(ref, VerdictEntry{
		Verdict: &v, Basis: basis, Model: model, Role: role, MadeAt: at,
		AttemptBasis: basis, AttemptedAt: at,
	})
}

// recordFailure keeps the previous verdict and notes the attempt. A
// transient failure (the item command or a model call failed) is retried
// with a growing backoff; invalid output or a missing role waits until the
// issue changes or a verdict is asked for by hand.
func (b *Backlog) recordFailure(ctx context.Context, ref, basis string, cause error, transient bool) error {
	if ctx.Err() != nil {
		return fmt.Errorf("%v: %w", cause, ctx.Err())
	}
	e, _ := b.cache.get(ref)
	if e.AttemptBasis != basis {
		e.Failures = 0
	}
	e.AttemptBasis = basis
	e.RetryAt = time.Time{}
	if transient {
		e.Failures++
		e.RetryAt = b.now().Add(retryBackoff(e.Failures))
	}
	e.AttemptedAt = b.now()
	e.Error = cause.Error()
	if err := b.cache.put(ref, e); err != nil {
		return fmt.Errorf("%v; save verdict cache: %w", cause, err)
	}
	return cause
}

// pruneVerdicts drops the verdicts of issues that left the inventory, once
// the interval has passed, so an issue that returns cannot skip it.
func (b *Backlog) pruneVerdicts(inv BacklogInventory) {
	if b.cache == nil {
		return
	}
	present := map[string]bool{}
	for _, issue := range inv.Issues {
		present[issue.Ref] = true
	}
	cutoff := b.now().Add(-time.Duration(*b.cfg.VerdictIntervalMinutes) * time.Minute)
	err := b.cache.prune(func(ref string, e VerdictEntry) bool {
		return present[ref] || e.AttemptedAt.After(cutoff) || e.MadeAt.After(cutoff)
	})
	if err != nil {
		b.log.Warn("prune verdict cache failed", "err", err.Error())
	}
}
