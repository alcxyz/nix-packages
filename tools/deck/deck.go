package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	factsInterval    = 60 * time.Second
	factsFreshFor    = 15 * time.Second
	probeMaxAge      = 30 * time.Minute
	probeConcurrency = 4
	headMessages     = 3
	headChars        = 1500
	tailMessages     = 30
	tailChars        = 500
	retryFirst       = 15 * time.Minute
	retryMax         = 6 * time.Hour
)

// modelTimeout bounds each model entry's call on its own, so a hung
// primary leaves time for the backups.
var modelTimeout = 5 * time.Minute

var (
	errBudget    = errors.New("model call budget for this hour is used up")
	errNotQuiet  = errors.New("thread is active again")
	errAttempted = errors.New("this revision was already attempted")
	errInterval  = errors.New("thread was attempted within the summary interval")
)

// instanceFacts is the last good view of one T3 instance plus its last error.
type instanceFacts struct {
	inventory Inventory
	status    map[string]StatusThread
	statusRaw []byte
	grouped   *Grouped
	// summaries are the cache entries the grouping's summary text came
	// from, so the page dates and flags exactly the text it shows.
	summaries map[string]CacheEntry
	asOf      time.Time
	// readAt is when the published inventory was read: changes after it
	// may be missing from the facts.
	readAt time.Time
	err    string
	errAt  time.Time
}

// probe is a thread's message revision as last checked, and the inventory
// updatedAt it was checked at. An empty revision means no messages. seq
// orders the observations: it is taken before the messages command runs, so
// a slow read cannot replace one that started after it. A probe seeded from
// the cache has seq zero.
type probe struct {
	updatedAt      string
	revision       string
	lastActivityAt string
	checkedAt      time.Time
	seq            uint64
}

func probeOf(updatedAt string, m Messages, at time.Time, seq uint64) probe {
	p := probe{updatedAt: updatedAt, lastActivityAt: m.LastActivityAt, checkedAt: at, seq: seq}
	if m.MessageCount > 0 {
		p.revision = m.Revision
	}
	return p
}

// Deck holds the facts of every instance, the summary queue and its budget.
type Deck struct {
	cfg      Config
	cmds     Commands
	cache    *SummaryCache
	models   map[string][]ModelConfig
	roleErr  error
	runModel modelRunner
	log      *slog.Logger
	now      func() time.Time

	mu       sync.Mutex
	facts    map[string]*instanceFacts
	probes   map[string]probe
	probeSeq uint64
	queue    []string
	manual   map[string]string
	inFlight map[string]bool
	calls    []time.Time

	// refreshLock serialises an instance's refreshes and regroups; it is a
	// channel so a waiter can give up when its context ends.
	refreshLock map[string]chan struct{}
	probing     map[string]bool
	probeWG     sync.WaitGroup
	// work counts the facts loops, the dispatcher and summary jobs, so
	// shutdown can wait until their commands have been killed.
	work sync.WaitGroup
	// base is the service context: actions run on it, detached from their
	// request but stopped at shutdown.
	base       context.Context
	refreshCh  map[string]chan struct{}
	refreshAll map[string]bool
	wake       chan struct{}
}

func newDeck(cfg Config, cache *SummaryCache, models map[string][]ModelConfig, roleErr error, log *slog.Logger) *Deck {
	d := &Deck{
		cfg:         cfg,
		cmds:        Commands{cfg: cfg.Commands, timeout: 90 * time.Second},
		cache:       cache,
		models:      models,
		roleErr:     roleErr,
		runModel:    runModelCLI,
		log:         log,
		now:         time.Now,
		facts:       map[string]*instanceFacts{},
		probes:      map[string]probe{},
		manual:      map[string]string{},
		inFlight:    map[string]bool{},
		refreshLock: map[string]chan struct{}{},
		probing:     map[string]bool{},
		refreshCh:   map[string]chan struct{}{},
		refreshAll:  map[string]bool{},
		wake:        make(chan struct{}, 1),
		base:        context.Background(),
	}
	for _, in := range cfg.Threads.Instances {
		d.facts[in.Name] = &instanceFacts{}
		d.refreshLock[in.Name] = make(chan struct{}, 1)
		d.refreshCh[in.Name] = make(chan struct{}, 1)
	}
	return d
}

// run starts the facts loop of every instance and the summary dispatcher.
// Cancelling ctx stops them, their commands and any action still running;
// wait then waits for them.
func (d *Deck) run(ctx context.Context) {
	d.base = ctx
	for _, in := range d.cfg.Threads.Instances {
		d.work.Add(1)
		go func(in InstanceConfig) {
			defer d.work.Done()
			d.factsLoop(ctx, in)
		}(in)
	}
	d.work.Add(1)
	go func() {
		defer d.work.Done()
		d.dispatch(ctx)
	}()
}

// wait waits up to timeout for the background work to end after its
// context was cancelled, and reports whether it did.
func (d *Deck) wait(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		d.work.Wait()
		d.probeWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

func (d *Deck) factsLoop(ctx context.Context, in InstanceConfig) {
	ticker := time.NewTicker(factsInterval)
	defer ticker.Stop()
	for {
		d.refreshInstance(ctx, in)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-d.refreshCh[in.Name]:
		}
	}
}

// requestRefresh asks the facts loop of one instance for a refresh, without
// waiting for it.
func (d *Deck) requestRefresh(instance string) {
	select {
	case d.refreshCh[instance] <- struct{}{}:
	default:
	}
}

// requestRefreshAll asks every facts loop to refresh now, then queues each
// thread that needs a summary from those facts. Repeated requests coalesce
// through the existing buffered refresh channels.
func (d *Deck) requestRefreshAll() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, in := range d.cfg.Threads.Instances {
		d.refreshAll[in.Name] = true
		d.requestRefresh(in.Name)
	}
}

// requestFacts asks for a facts refresh of every instance whose facts are
// older than factsFreshFor, without waiting for it.
func (d *Deck) requestFacts() {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	for name, f := range d.facts {
		if now.Sub(f.asOf) < factsFreshFor {
			continue
		}
		select {
		case d.refreshCh[name] <- struct{}{}:
		default:
		}
	}
}

var errRefreshBusy = errors.New("another refresh of this instance is still running")

func (d *Deck) lockRefresh(ctx context.Context, instance string, wait time.Duration) error {
	var timeout <-chan time.Time
	if wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		timeout = timer.C
	}
	select {
	case d.refreshLock[instance] <- struct{}{}:
		return nil
	case <-timeout:
		return errRefreshBusy
	case <-ctx.Done():
		return errRefreshBusy
	}
}

func (d *Deck) unlockRefresh(instance string) {
	<-d.refreshLock[instance]
}

// refreshInstance refreshes one instance's facts and reschedules summaries,
// and starts probing changed threads in the background. A failure keeps the
// last good facts and records the error.
func (d *Deck) refreshInstance(ctx context.Context, in InstanceConfig) {
	d.mu.Lock()
	refreshAll := d.refreshAll[in.Name]
	delete(d.refreshAll, in.Name)
	d.mu.Unlock()
	err := d.refreshFacts(ctx, in, 0)
	if err != nil {
		d.factsError(in.Name, err)
	}
	if refreshAll {
		// On failure, refresh summaries of the last good facts instead.
		d.mu.Lock()
		if len(d.models[d.cfg.Threads.SummaryRole]) > 0 {
			var pending []string
			for _, key := range d.queue {
				if d.manual[key] != "" {
					pending = append(pending, key)
				}
			}
			f := d.facts[in.Name]
			for _, t := range f.inventory.Threads {
				key := cacheKey(in.Name, t.ThreadID)
				if needsSummaryByFacts(t, f.status[t.ThreadID]) && d.manual[key] == "" {
					// A bulk refresh must retain an explicit pending role,
					// including one requested while facts were being read.
					d.queueSummaryLocked(key, d.cfg.Threads.SummaryRole)
				}
			}
			// Keep already queued manual requests ahead of new bulk defaults,
			// in their original order.
			for i := len(pending) - 1; i >= 0; i-- {
				d.queueSummaryLocked(pending[i], d.manual[pending[i]])
			}
		}
		d.mu.Unlock()
	}
	d.schedule()
	if err == nil {
		d.startProbes(ctx, in)
	}
}

// startProbes probes changed threads without holding up the facts loop: a
// slow messages command delays summaries, never facts. One probe pass runs
// per instance at a time.
func (d *Deck) startProbes(ctx context.Context, in InstanceConfig) {
	d.mu.Lock()
	if d.probing[in.Name] {
		d.mu.Unlock()
		return
	}
	d.probing[in.Name] = true
	d.mu.Unlock()
	d.probeWG.Add(1)
	go func() {
		defer d.probeWG.Done()
		changed := d.probeChanged(ctx, in)
		d.mu.Lock()
		d.probing[in.Name] = false
		d.mu.Unlock()
		// A revision the grouping did not know may withhold a verdict.
		if changed {
			if err := d.regroup(ctx, in); err != nil && ctx.Err() == nil {
				d.factsError(in.Name, err)
			}
		}
		d.schedule()
	}()
}

// refreshFacts runs inventory, status and the grouping, and publishes them
// together only once all three have succeeded, so the page never pairs new
// facts with an old grouping. It holds the instance's refresh lock, which
// also serialises regroup, so groupings publish in the order they read
// their facts.
//
// lockWait bounds the wait for the lock, separately from ctx, which bounds
// the commands; zero waits as long as ctx allows.
func (d *Deck) refreshFacts(ctx context.Context, in InstanceConfig, lockWait time.Duration) error {
	if err := d.lockRefresh(ctx, in.Name, lockWait); err != nil {
		return err
	}
	defer d.unlockRefresh(in.Name)

	readAt := d.now()
	inv, err := d.cmds.inventory(ctx, in, d.cfg.Threads.WindowDays)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(inv.Threads))
	for _, t := range inv.Threads {
		ids = append(ids, t.ThreadID)
	}
	st, statusRaw, err := d.cmds.status(ctx, in, ids)
	if err != nil {
		return err
	}
	g, snap, err := d.group(ctx, in, inv, statusRaw)
	if err != nil {
		return err
	}
	byID := map[string]StatusThread{}
	for _, s := range st.Threads {
		byID[s.ThreadID] = s
	}

	d.mu.Lock()
	f := d.facts[in.Name]
	f.inventory = inv
	f.status = byID
	f.statusRaw = statusRaw
	f.grouped = &g
	f.summaries = snap
	f.asOf = d.now()
	f.readAt = readAt
	f.err = ""
	d.mu.Unlock()
	d.prune(in, inv)
	return nil
}

// prune forgets probes of threads that left the instance's inventory, and
// cached summaries of those not touched for longer than the window, so
// neither grows without bound.
func (d *Deck) prune(in InstanceConfig, inv Inventory) {
	present := map[string]bool{}
	for _, t := range inv.Threads {
		present[cacheKey(in.Name, t.ThreadID)] = true
	}
	prefix := in.Name + "/"
	d.mu.Lock()
	for k := range d.probes {
		if strings.HasPrefix(k, prefix) && !present[k] {
			delete(d.probes, k)
		}
	}
	d.mu.Unlock()
	// Keep an absent thread's entry for the summary interval too, so a
	// thread that returns cannot skip it.
	keep := max(time.Duration(d.cfg.Threads.WindowDays+1)*24*time.Hour,
		time.Duration(*d.cfg.Threads.SummaryIntervalMinutes)*time.Minute)
	cutoff := d.now().Add(-keep)
	err := d.cache.prune(func(k string, e CacheEntry) bool {
		return !strings.HasPrefix(k, prefix) || present[k] || e.AttemptedAt.After(cutoff) || e.MadeAt.After(cutoff)
	})
	if err != nil {
		d.log.Warn("prune summary cache failed", "err", err.Error())
	}
}

func (d *Deck) factsError(instance string, err error) {
	d.log.Warn("facts refresh failed", "instance", instance, "err", err.Error())
	d.mu.Lock()
	defer d.mu.Unlock()
	f := d.facts[instance]
	f.err = err.Error()
	f.errAt = d.now()
}

// regroup regroups the published facts after a summary changed. It takes
// the refresh lock, so it cannot publish over a newer refresh.
func (d *Deck) regroup(ctx context.Context, in InstanceConfig) error {
	if err := d.lockRefresh(ctx, in.Name, 0); err != nil {
		return err
	}
	defer d.unlockRefresh(in.Name)

	d.mu.Lock()
	f := d.facts[in.Name]
	inv := f.inventory
	statusRaw := f.statusRaw
	ready := f.grouped != nil
	d.mu.Unlock()
	if !ready {
		return nil
	}
	g, snap, err := d.group(ctx, in, inv, statusRaw)
	if err != nil {
		return err
	}
	d.mu.Lock()
	f.grouped = &g
	f.summaries = snap
	d.mu.Unlock()
	return nil
}

// group builds the overview report from facts and cached summaries and runs
// the group command on it.
func (d *Deck) group(ctx context.Context, in InstanceConfig, inv Inventory, statusRaw []byte) (Grouped, map[string]CacheEntry, error) {
	report, snap, err := d.buildReport(in, inv)
	if err != nil {
		return Grouped{}, nil, err
	}
	g, err := d.cmds.group(ctx, report, statusRaw)
	if err != nil {
		return Grouped{}, nil, err
	}
	return g, snap, nil
}

func (d *Deck) environmentID(in InstanceConfig, inv Inventory) string {
	if in.EnvironmentID != "" {
		return in.EnvironmentID
	}
	return inv.EnvironmentID
}

// buildReport passes each inventory row on unchanged and adds the thread's
// cached summary fields, even when the thread has moved on since: an older
// summary is better than none, and the page marks it. It also returns the
// cache entries it used.
func (d *Deck) buildReport(in InstanceConfig, inv Inventory) ([]byte, map[string]CacheEntry, error) {
	snap := map[string]CacheEntry{}
	rows := make([]map[string]json.RawMessage, 0, len(inv.Threads))
	for _, t := range inv.Threads {
		row := map[string]json.RawMessage{}
		for k, v := range t.raw {
			row[k] = v
		}
		if e, ok := d.cache.get(cacheKey(in.Name, t.ThreadID)); ok && e.Summary != nil {
			snap[cacheKey(in.Name, t.ThreadID)] = e
			data, err := json.Marshal(e.Summary)
			if err != nil {
				return nil, nil, err
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(data, &fields); err != nil {
				return nil, nil, err
			}
			// A verdict judged another settlement than the thread's current
			// one, or none, or messages the thread has since moved past: it
			// must not decide the grouping.
			d.mu.Lock()
			p, probed := d.probes[cacheKey(in.Name, t.ThreadID)]
			d.mu.Unlock()
			movedOn := probed && p.revision != "" && p.revision != e.Basis.Revision
			if e.Basis.Bucket != t.Bucket || e.Basis.SettledAt != t.SettledAt || movedOn {
				fields["verdict"] = json.RawMessage("null")
				fields["verdictReason"] = json.RawMessage("null")
			}
			for k, v := range fields {
				row[k] = v
			}
		}
		rows = append(rows, row)
	}
	report := map[string]any{
		"generatedAt": d.now().UTC().Format(time.RFC3339),
		"windowDays":  d.cfg.Threads.WindowDays,
		"threads":     rows,
	}
	if env := d.environmentID(in, inv); env != "" {
		report["environmentId"] = env
	}
	data, err := json.Marshal(report)
	if err != nil {
		return nil, nil, err
	}
	return data, snap, nil
}

// needsSummaryByFacts is false for threads the grouping fills in itself:
// acknowledged ones and settled ones the facts call done or left for you.
func needsSummaryByFacts(t InventoryThread, st StatusThread) bool {
	if t.Acknowledged {
		return false
	}
	if t.settled() || st.State == "settled" {
		return st.Verdict != "done" && st.Verdict != "left-for-you"
	}
	return true
}

func isSettled(t InventoryThread, st StatusThread) bool {
	return t.settled() || st.State == "settled"
}

// knownProbe returns the probe of a thread, seeded from its cache entry when
// deck has not checked the thread since it started.
func (d *Deck) knownProbeLocked(key string) (probe, bool) {
	if p, ok := d.probes[key]; ok {
		return p, true
	}
	e, ok := d.cache.get(key)
	// An attempt without a revision may have failed before reading the
	// messages, so it says nothing about them: probe the thread.
	if !ok || e.AttemptedAt.IsZero() || e.AttemptBasis.Revision == "" || e.ThreadUpdatedAt == "" {
		return probe{}, false
	}
	p := probe{updatedAt: e.ThreadUpdatedAt, revision: e.AttemptBasis.Revision, lastActivityAt: e.LastActivityAt, checkedAt: e.AttemptedAt}
	d.probes[key] = p
	return p, true
}

// nextObservation numbers a messages read before it starts.
func (d *Deck) nextObservation() uint64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.probeSeq++
	return d.probeSeq
}

// publishProbeLocked records an observation unless one that started later
// is already recorded, and reports whether the known revision changed.
func (d *Deck) publishProbeLocked(key string, p probe) bool {
	old, ok := d.probes[key]
	if ok && old.seq > p.seq {
		return false
	}
	d.probes[key] = p
	return !ok || old.revision != p.revision
}

// attemptedLocked reports whether the cache already holds an attempt at
// basis that is not due for a retry: a summary, a failure that is not
// retried, or one whose backoff still runs.
func (d *Deck) attemptedLocked(key string, basis Basis, now time.Time) bool {
	e, ok := d.cache.get(key)
	return ok && e.AttemptBasis == basis && (e.RetryAt.IsZero() || now.Before(e.RetryAt))
}

// probeChanged asks for the message revision of each thread that may need a
// summary and whose inventory updatedAt moved, or whose last check is old.
// It reports whether a revision became known or changed, including one
// seeded from the cache.
func (d *Deck) probeChanged(ctx context.Context, in InstanceConfig) bool {
	changed := false
	type target struct {
		id, updatedAt string
	}
	var targets []target
	d.mu.Lock()
	f := d.facts[in.Name]
	now := d.now()
	for _, t := range f.inventory.Threads {
		if !needsSummaryByFacts(t, f.status[t.ThreadID]) {
			continue
		}
		_, had := d.probes[cacheKey(in.Name, t.ThreadID)]
		p, ok := d.knownProbeLocked(cacheKey(in.Name, t.ThreadID))
		if ok && !had {
			changed = true
		}
		if ok && p.updatedAt == t.UpdatedAt && now.Sub(p.checkedAt) < probeMaxAge {
			continue
		}
		targets = append(targets, target{t.ThreadID, t.UpdatedAt})
	}
	d.mu.Unlock()

	sem := make(chan struct{}, probeConcurrency)
	var wg sync.WaitGroup
	for _, tg := range targets {
		wg.Add(1)
		sem <- struct{}{}
		go func(tg target) {
			defer wg.Done()
			defer func() { <-sem }()
			seq := d.nextObservation()
			m, err := d.cmds.messages(ctx, in, tg.id, 0, 0, 0)
			if err != nil {
				d.log.Warn("message revision check failed", "instance", in.Name, "thread", tg.id, "err", err.Error())
				return
			}
			d.mu.Lock()
			if d.publishProbeLocked(cacheKey(in.Name, tg.id), probeOf(tg.updatedAt, m, d.now(), seq)) {
				changed = true
			}
			d.mu.Unlock()
		}(tg)
	}
	wg.Wait()
	return changed
}

// waitsForIntervalLocked reports whether a changed thread must still wait
// for the summary interval after its last attempt, failed or not and by hand
// or not; a retry of the same basis follows its backoff instead.
func (d *Deck) waitsForIntervalLocked(key string, basis Basis, now time.Time) bool {
	interval := time.Duration(*d.cfg.Threads.SummaryIntervalMinutes) * time.Minute
	e, ok := d.cache.get(key)
	return ok && e.AttemptBasis != basis && !e.AttemptedAt.IsZero() && now.Sub(e.AttemptedAt) < interval
}

// candidate is a thread that needs a summary and may get one now.
type candidate struct {
	key     string
	changed time.Time
}

// candidatesLocked lists the threads that need a (new) summary and are
// eligible for one, oldest change first. A thread attempted less than
// summaryIntervalMinutes ago waits, settled or not; a refresh by hand does
// not.
func (d *Deck) candidatesLocked() []candidate {
	if d.roleErr != nil {
		return nil // automatic summaries need the default role
	}
	now := d.now()
	quiet := time.Duration(*d.cfg.Threads.QuietMinutes) * time.Minute
	var out []candidate
	for _, in := range d.cfg.Threads.Instances {
		f := d.facts[in.Name]
		for _, t := range f.inventory.Threads {
			st := f.status[t.ThreadID]
			if !needsSummaryByFacts(t, st) {
				continue
			}
			key := cacheKey(in.Name, t.ThreadID)
			p, probed := d.probes[key]
			if !probed {
				continue
			}
			if p.revision == "" {
				continue // no finished messages to summarise
			}
			basis := basisOf(p.revision, t)
			if d.attemptedLocked(key, basis, now) {
				continue
			}
			if d.waitsForIntervalLocked(key, basis, now) {
				continue
			}
			changed := lastChange(p.lastActivityAt, t.UpdatedAt)
			if !isSettled(t, st) && !quietSince(changed, now, quiet) {
				continue
			}
			out = append(out, candidate{key: key, changed: changed})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].changed.Before(out[j].changed) })
	return out
}

// lastChange is the thread's last message activity, or its inventory
// updatedAt when that is unknown.
func lastChange(lastActivityAt, updatedAt string) time.Time {
	if t, err := time.Parse(time.RFC3339, lastActivityAt); err == nil {
		return t
	}
	t, _ := time.Parse(time.RFC3339, updatedAt)
	return t
}

func quietSince(changed, now time.Time, quiet time.Duration) bool {
	return !changed.IsZero() && now.Sub(changed) >= quiet
}

// schedule rebuilds the queue: threads queued by hand first, then eligible
// threads, oldest change first.
func (d *Deck) schedule() {
	d.mu.Lock()
	var q []string
	for _, key := range d.queue {
		if d.manual[key] != "" && !d.inFlight[key] {
			q = append(q, key)
		}
	}
	for _, c := range d.candidatesLocked() {
		if d.manual[c.key] == "" && !d.inFlight[c.key] {
			q = append(q, c.key)
		}
	}
	d.queue = q
	d.mu.Unlock()
	d.signal()
}

func (d *Deck) signal() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// budgetLeftLocked drops calls older than an hour and returns how many
// calls remain in the rolling hour.
func (d *Deck) budgetLeftLocked() int {
	cutoff := d.now().Add(-time.Hour)
	kept := d.calls[:0]
	for _, t := range d.calls {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	d.calls = kept
	return d.cfg.Threads.CallsPerHour - len(d.calls)
}

// takeCall spends one model call of the budget, if any is left.
func (d *Deck) takeCall() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.budgetLeftLocked() <= 0 {
		return false
	}
	d.calls = append(d.calls, d.now())
	return true
}

// requestSummary queues a thread ahead of the others, for the refresh
// button. It still waits for budget.
func (d *Deck) requestSummary(instance, threadID, role string) error {
	if len(d.models[role]) == 0 {
		return fmt.Errorf("unknown or unusable summary role %q", role)
	}
	key := cacheKey(instance, threadID)
	d.mu.Lock()
	f, ok := d.facts[instance]
	found := false
	if ok {
		for _, t := range f.inventory.Threads {
			if t.ThreadID == threadID {
				found = true
				break
			}
		}
	}
	if !found {
		d.mu.Unlock()
		return errors.New("unknown thread")
	}
	d.queueSummaryLocked(key, role)
	d.mu.Unlock()
	d.signal()
	return nil
}

// queueSummaryLocked records the one-shot role even if a job is already
// running. The latest request for a thread replaces any pending request.
func (d *Deck) queueSummaryLocked(key, role string) {
	d.manual[key] = role
	if d.inFlight[key] {
		return // the running job queues this request when it ends
	}
	q := []string{key}
	for _, k := range d.queue {
		if k != key {
			q = append(q, k)
		}
	}
	d.queue = q
}

func (d *Deck) dispatch(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		d.startJobs(ctx)
		select {
		case <-ctx.Done():
			return
		case <-d.wake:
		case <-ticker.C:
		}
	}
}

// startJobs starts queued jobs while there is concurrency and budget left.
func (d *Deck) startJobs(ctx context.Context) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for len(d.inFlight) < d.cfg.Threads.RefreshConcurrency && len(d.queue) > 0 && d.budgetLeftLocked() > 0 {
		key := d.queue[0]
		d.queue = d.queue[1:]
		role := d.manual[key]
		delete(d.manual, key)
		d.inFlight[key] = true
		d.work.Add(1)
		go func(key, role string) {
			defer d.work.Done()
			d.summarize(ctx, key, role)
		}(key, role)
	}
}

// findThreadLocked looks a cache key up in the current facts.
func (d *Deck) findThreadLocked(key string) (InstanceConfig, InventoryThread, StatusThread, bool) {
	for _, in := range d.cfg.Threads.Instances {
		f := d.facts[in.Name]
		for _, t := range f.inventory.Threads {
			if cacheKey(in.Name, t.ThreadID) == key {
				return in, t, f.status[t.ThreadID], true
			}
		}
	}
	return InstanceConfig{}, InventoryThread{}, StatusThread{}, false
}

func (d *Deck) summarize(ctx context.Context, key, role string) {
	d.mu.Lock()
	in, t, st, ok := d.findThreadLocked(key)
	d.mu.Unlock()
	// A job stopped by the budget, or one a refresh by hand was asked for
	// while it ran (startJobs cleared the mark when it started), goes back
	// to the front of the queue in the same step that ends it, so schedule
	// never sees it both running and queued.
	requeue := false
	defer func() {
		d.mu.Lock()
		delete(d.inFlight, key)
		if requeue || d.manual[key] != "" {
			d.queue = append([]string{key}, d.queue...)
			if d.manual[key] == "" && role != "" {
				d.manual[key] = role
			}
		}
		d.mu.Unlock()
		d.signal()
	}()
	if !ok || !needsSummaryByFacts(t, st) {
		return // gone, or settled or accepted since it was queued
	}

	err := d.summarizeThread(ctx, in, t, st, role)
	if errors.Is(err, errNotQuiet) {
		return // rescheduled once the thread has been quiet long enough
	}
	if errors.Is(err, errInterval) {
		return // the thread changed since it was queued; rescheduled after the interval
	}
	if errors.Is(err, errAttempted) {
		return // queued on an older revision than the one read
	}
	if ctx.Err() != nil {
		return // deck is shutting down; nothing was recorded
	}
	if errors.Is(err, errBudget) {
		requeue = true
		return
	}
	if err != nil {
		d.log.Warn("summary failed", "instance", in.Name, "thread", t.ThreadID, "err", err.Error())
	} else {
		d.log.Info("summary updated", "instance", in.Name, "thread", t.ThreadID)
	}
	if err := d.regroup(ctx, in); err != nil {
		d.factsError(in.Name, err)
	}
}

// summarizeThread makes one summary and records the outcome in the cache.
// On failure the previous summary stays.
func (d *Deck) summarizeThread(ctx context.Context, in InstanceConfig, t InventoryThread, st StatusThread, role string) error {
	manual := role != ""
	if !manual {
		role = d.cfg.Threads.SummaryRole
	}
	key := cacheKey(in.Name, t.ThreadID)
	// Until the messages are read, the last check names the revision this
	// attempt is about.
	d.mu.Lock()
	seen := d.probes[key]
	d.mu.Unlock()
	if len(d.models[role]) == 0 {
		return d.recordFailure(ctx, key, t, seen, fmt.Errorf("summary role %q is unavailable", role), false)
	}
	seq := d.nextObservation()
	m, err := d.cmds.messages(ctx, in, t.ThreadID, headMessages, tailMessages, headChars)
	if err != nil {
		return d.recordFailure(ctx, key, t, seen, err, true)
	}
	seen = probeOf(t.UpdatedAt, m, d.now(), seq)
	// The job was queued on the revision last checked; the cache and its
	// retry backoff apply to the revision just read. A refresh by hand
	// tries again regardless.
	d.mu.Lock()
	d.publishProbeLocked(key, seen)
	attempted := d.attemptedLocked(key, basisOf(seen.revision, t), d.now())
	waits := d.waitsForIntervalLocked(key, basisOf(seen.revision, t), d.now())
	d.mu.Unlock()
	if !manual && attempted {
		return errAttempted
	}
	if !manual && waits {
		return errInterval
	}
	if seen.revision == "" {
		return d.recordFailure(ctx, key, t, seen, errors.New("thread has no finished messages"), false)
	}

	settled := isSettled(t, st)
	// The thread may have moved on since it was queued; a queued job waits
	// for the quiet period like any other, a refresh by hand does not.
	quiet := time.Duration(*d.cfg.Threads.QuietMinutes) * time.Minute
	if !manual && !settled && !quietSince(lastChange(seen.lastActivityAt, t.UpdatedAt), d.now(), quiet) {
		return errNotQuiet
	}
	facts := promptFacts{
		Title:     t.Title,
		Project:   t.Project,
		Settled:   settled,
		RunStatus: t.Status,
		Verdict:   st.Verdict,
		OpenPRs:   []string{},
		WaitingOn: t.WaitingOn,
	}
	if facts.WaitingOn == nil {
		facts.WaitingOn = []WaitingOn{}
	}
	if st.Settle != nil {
		facts.SettleLine = st.Settle.Line
	}
	for _, pr := range t.PRs {
		if isOpenPR(pr.State) {
			facts.OpenPRs = append(facts.OpenPRs, pr.URL)
		}
	}
	prompt, err := buildPrompt(facts, mergeMessages(m, headMessages, tailChars))
	if err != nil {
		return d.recordFailure(ctx, key, t, seen, err, false)
	}

	out, model, err := callModel(ctx, d.runModel, d.models[role], prompt, d.takeCall)
	if errors.Is(err, errBudget) {
		return err
	}
	if err != nil {
		return d.recordFailure(ctx, key, t, seen, err, true)
	}
	s, err := parseSummary(out, settled)
	if err != nil {
		return d.recordFailure(ctx, key, t, seen, fmt.Errorf("%s: %w", model, err), false)
	}
	now := d.now()
	return d.cache.put(key, CacheEntry{
		Summary:         &s,
		Basis:           basisOf(seen.revision, t),
		LastActivityAt:  seen.lastActivityAt,
		ThreadUpdatedAt: t.UpdatedAt,
		Model:           model,
		Role:            role,
		MadeAt:          now,
		AttemptBasis:    basisOf(seen.revision, t),
		AttemptedAt:     now,
	})
}

// recordFailure keeps the previous summary and notes the attempt. A
// transient failure (a command or model call that failed) is retried with
// a growing backoff; invalid output, a thread without messages or a missing
// role is not retried until the thread changes or is refreshed by hand.
func (d *Deck) recordFailure(ctx context.Context, key string, t InventoryThread, seen probe, cause error, transient bool) error {
	// A call cut short by shutdown says nothing about the thread: record
	// nothing, so the next start tries again at once.
	if ctx.Err() != nil {
		return fmt.Errorf("%v: %w", cause, ctx.Err())
	}
	e, _ := d.cache.get(key)
	basis := basisOf(seen.revision, t)
	if e.AttemptBasis != basis {
		e.Failures = 0
	}
	e.AttemptBasis = basis
	e.RetryAt = time.Time{}
	if transient {
		e.Failures++
		e.RetryAt = d.now().Add(retryBackoff(e.Failures))
	}
	if e.Summary == nil {
		e.LastActivityAt = seen.lastActivityAt
	}
	// The revision attempted is the one checked at seen.updatedAt, which
	// may be older than the thread's current updatedAt.
	e.ThreadUpdatedAt = seen.updatedAt
	e.AttemptedAt = d.now()
	e.Error = cause.Error()
	if err := d.cache.put(key, e); err != nil {
		return fmt.Errorf("%v; save summary cache: %w", cause, err)
	}
	return cause
}

// retryBackoff is 15 minutes after the first transient failure, doubling up
// to six hours.
func retryBackoff(failures int) time.Duration {
	backoff := retryFirst
	for i := 1; i < failures && backoff < retryMax; i++ {
		backoff *= 2
	}
	if backoff > retryMax {
		backoff = retryMax
	}
	return backoff
}

func isOpenPR(state string) bool {
	return state == "open" || state == "OPEN" || state == "Open"
}
