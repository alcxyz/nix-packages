package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"time"
)

// PageView is everything the page template shows.
type PageView struct {
	BasePath     string
	GeneratedAt  time.Time
	Counts       GroupSummary
	HasAvg       bool
	AvgPct       int
	Instances    []InstanceView
	Multi        bool
	QueueLen     int
	Running      int
	BudgetLeft   int
	CallsPerHour int
	RoleErr      string
	Sections     []SectionView
	Unblockers   []Unblocker
	Empty        bool
	// SeenUpTo is the oldest time up to which the page's facts are
	// complete, across instances; zero while an instance has none. A
	// visit marks changes up to it as seen, never later ones.
	SeenUpTo time.Time
}

type InstanceView struct {
	Label  string
	AsOf   time.Time
	HasRun bool
	Err    string
	ErrAt  time.Time
}

type SectionView struct {
	Key   string
	Title string
	Count int
	Subs  []SubView
}

type SubView struct {
	Title     string
	Collapsed bool
	Rows      []RowView
}

type RowView struct {
	Key           string
	Instance      string
	InstanceLabel string
	ShowInstance  bool
	ThreadID      string
	Title         string
	Prefix        string
	Project       string
	Link          string
	Group         string
	Owner         string
	HasProgress   bool
	Progress      int
	Phase         string
	NextAction    string
	ActionKind    string
	Goal          string
	Done          []string
	Remaining     []string
	Blocker       string
	Flag          string
	Verdict       string
	VerdictReason string
	FactsVerdict  string
	SettleLine    string
	LeftForYou    []string
	StallReasons  []string
	WaitingOn     []WaitingOn
	PRs           []PR
	// UnwatchedChecks are open PRs whose checks run while the thread has
	// stopped with no PR watch armed.
	UnwatchedChecks []UnwatchedCheck
	HasIdle         bool
	IdleMinutes     int
	IdleText        string
	UpdatedAt       string
	SettledAt       string
	Acknowledged    bool
	CanAccept       bool
	CanRefresh      bool
	HasSummary      bool
	SummaryStale    bool
	// SummaryUnchecked marks a summary of a thread that changed after its
	// messages were last checked.
	SummaryUnchecked   bool
	SummaryModel       string
	SummaryRole        string
	SummaryRoles       []string
	DefaultSummaryRole string
	SummaryAt          time.Time
	SummaryError       string
	SummaryState       string
}

// UnwatchedCheck is one PR of a thread's unwatchedChecks; Number is empty
// when neither the status facts nor the URL give one.
type UnwatchedCheck struct {
	URL    string
	Number string
}

type Unblocker struct {
	URL     string
	Threads []RowView
}

var prefixRE = regexp.MustCompile(`^\[([^\]]{1,40})\]`)

var actionKindTitles = []struct{ kind, title string }{
	{"decide", "Decide or answer"},
	{"deploy", "Deploy or verify"},
	{"review", "Review or merge"},
	{"unblock", "Sign in or unblock"},
	{"other", "Other"},
}

// threadLink builds the T3 web app link of a thread, each path segment
// escaped.
func threadLink(in InstanceConfig, env, threadID string) string {
	if env == "" {
		return in.WebURL + "/"
	}
	return in.WebURL + "/" + url.PathEscape(env) + "/" + url.PathEscape(threadID)
}

// prNumber is a PR's number: from the status facts when they list the URL,
// otherwise the URL's last path segment when that is a number.
func prNumber(prURL string, prs []StatusPR) string {
	for _, pr := range prs {
		if pr.URL == prURL && pr.Number > 0 {
			return strconv.Itoa(pr.Number)
		}
	}
	u, err := url.Parse(prURL)
	if err != nil {
		return ""
	}
	n := path.Base(u.Path)
	if _, err := strconv.Atoi(n); err != nil {
		return ""
	}
	return n
}

// leftForYouList reads the Left-for-you steps, a list of strings or one
// string.
func leftForYouList(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		return list
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil && one != "" {
		return []string{one}
	}
	return nil
}

func idleText(minutes int) string {
	switch {
	case minutes < 60:
		return fmt.Sprintf("%dm", minutes)
	case minutes < 48*60:
		return fmt.Sprintf("%dh %dm", minutes/60, minutes%60)
	default:
		return fmt.Sprintf("%dd %dh", minutes/(24*60), (minutes/60)%24)
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// view builds the page from the current facts, without running anything.
func (d *Deck) view() PageView {
	d.mu.Lock()
	defer d.mu.Unlock()

	v := PageView{
		BasePath:     d.cfg.BasePath,
		GeneratedAt:  d.now(),
		Multi:        len(d.cfg.Threads.Instances) > 1,
		QueueLen:     len(d.queue),
		Running:      len(d.inFlight),
		BudgetLeft:   d.budgetLeftLocked(),
		CallsPerHour: d.cfg.Threads.CallsPerHour,
		Counts:       GroupSummary{Phases: map[string]int{}},
	}
	if d.roleErr != nil {
		v.RoleErr = d.roleErr.Error()
	}
	queued := map[string]bool{}
	for _, k := range d.queue {
		queued[k] = true
	}

	sectionIndex := map[string]int{}
	var sections []SectionView
	rowsBySection := map[string][]RowView{}
	pctSum, pctN := 0.0, 0
	prThreads := map[string][]RowView{}
	var prOrder []string

	complete := true
	for _, in := range d.cfg.Threads.Instances {
		f := d.facts[in.Name]
		v.Instances = append(v.Instances, InstanceView{
			Label: in.Label, AsOf: f.asOf, HasRun: !f.asOf.IsZero(), Err: f.err, ErrAt: f.errAt,
		})
		if f.grouped == nil {
			complete = false
			continue
		}
		if v.SeenUpTo.IsZero() || f.readAt.Before(v.SeenUpTo) {
			v.SeenUpTo = f.readAt
		}
		s := f.grouped.Summary
		v.Counts.Unsettled += s.Unsettled
		v.Counts.User += s.User
		v.Counts.Agent += s.Agent
		v.Counts.Stalled += s.Stalled
		v.Counts.Settle += s.Settle
		v.Counts.SettledOpen += s.SettledOpen
		v.Counts.UnwatchedChecks += s.UnwatchedChecks
		for p, n := range s.Phases {
			v.Counts.Phases[p] += n
		}

		env := d.environmentID(in, f.inventory)
		inv := map[string]InventoryThread{}
		for _, t := range f.inventory.Threads {
			inv[t.ThreadID] = t
		}
		for _, sec := range f.grouped.Sections {
			if _, ok := sectionIndex[sec.Key]; !ok {
				sectionIndex[sec.Key] = len(sections)
				sections = append(sections, SectionView{Key: sec.Key, Title: sec.Title})
			}
			for _, gr := range sec.Threads {
				row := d.rowLocked(in, env, gr, f.status[gr.ThreadID], f.summaries, queued)
				row.ShowInstance = v.Multi
				// The average is the group command's rule over every
				// instance's unsettled threads: progress, or 0 without one.
				if gr.Bucket != "recently-settled" {
					pctN++
					if gr.ProgressPct != nil {
						pctSum += float64(*gr.ProgressPct)
					}
				}
				rowsBySection[sec.Key] = append(rowsBySection[sec.Key], row)
				t := inv[gr.ThreadID]
				if t.Bucket != "unsettled" {
					continue
				}
				seen := map[string]bool{}
				for _, pr := range t.PRs {
					if !isOpenPR(pr.State) || pr.URL == "" || seen[pr.URL] {
						continue
					}
					seen[pr.URL] = true
					if _, ok := prThreads[pr.URL]; !ok {
						prOrder = append(prOrder, pr.URL)
					}
					prThreads[pr.URL] = append(prThreads[pr.URL], row)
				}
			}
		}
	}
	if pctN > 0 {
		v.HasAvg = true
		v.AvgPct = int(pctSum/float64(pctN) + 0.5)
	}

	for _, sec := range sections {
		rows := rowsBySection[sec.Key]
		sec.Count = len(rows)
		sec.Subs = subdivide(sec.Key, rows)
		v.Sections = append(v.Sections, sec)
	}
	v.Empty = len(rowsBySection) == 0

	for _, u := range prOrder {
		if len(prThreads[u]) >= 2 {
			v.Unblockers = append(v.Unblockers, Unblocker{URL: u, Threads: prThreads[u]})
		}
	}
	sort.SliceStable(v.Unblockers, func(i, j int) bool {
		return len(v.Unblockers[i].Threads) > len(v.Unblockers[j].Threads)
	})
	if !complete {
		v.SeenUpTo = time.Time{}
	}
	return v
}

// subdivide splits "waiting on you" by kind of action and the settled
// section into rows that may not be done and rows confirmed done.
func subdivide(key string, rows []RowView) []SubView {
	switch key {
	case "you":
		var subs []SubView
		for _, ak := range actionKindTitles {
			var sub []RowView
			for _, r := range rows {
				kind := r.ActionKind
				if !oneOf(kind, actionKinds) {
					kind = "other"
				}
				if kind == ak.kind {
					sub = append(sub, r)
				}
			}
			if len(sub) > 0 {
				subs = append(subs, SubView{Title: ak.title, Rows: sub})
			}
		}
		return subs
	case "settled":
		var open, done []RowView
		for _, r := range rows {
			if r.Group == "settled-done" {
				done = append(done, r)
			} else {
				open = append(open, r)
			}
		}
		var subs []SubView
		if len(open) > 0 {
			subs = append(subs, SubView{Title: "Maybe not done", Rows: open})
		}
		if len(done) > 0 {
			subs = append(subs, SubView{Title: "Confirmed done", Collapsed: true, Rows: done})
		}
		return subs
	default:
		return []SubView{{Rows: rows}}
	}
}

// rowLocked builds a row. shown holds the cache entries whose text the
// grouping carries: the row's date and staleness describe that text, while
// the error and queue state come from the latest attempt.
func (d *Deck) rowLocked(in InstanceConfig, env string, gr GroupRow, st StatusThread, shown map[string]CacheEntry, queued map[string]bool) RowView {
	key := cacheKey(in.Name, gr.ThreadID)
	r := RowView{
		Key:           key,
		Instance:      in.Name,
		InstanceLabel: in.Label,
		ThreadID:      gr.ThreadID,
		Title:         gr.Title,
		Project:       gr.Project,
		Link:          threadLink(in, env, gr.ThreadID),
		Group:         gr.Group,
		Owner:         gr.Owner,
		Phase:         gr.Phase,
		NextAction:    gr.NextAction,
		ActionKind:    gr.ActionKind,
		Goal:          gr.Goal,
		Done:          gr.Done,
		Remaining:     gr.Remaining,
		Blocker:       deref(gr.Blocker),
		Flag:          deref(gr.Flag),
		Verdict:       deref(gr.Verdict),
		VerdictReason: deref(gr.VerdictReason),
		FactsVerdict:  gr.Facts.Verdict,
		StallReasons:  gr.Facts.StallReasons,
		WaitingOn:     gr.WaitingOn,
		UpdatedAt:     gr.UpdatedAt,
		SettledAt:     gr.SettledAt,
		Acknowledged:  gr.Acknowledged,
	}
	for _, role := range d.cfg.Threads.SummaryRoles {
		if len(d.models[role]) > 0 {
			r.SummaryRoles = append(r.SummaryRoles, role)
		}
	}
	r.DefaultSummaryRole = d.cfg.Threads.SummaryRole
	r.CanRefresh = len(r.SummaryRoles) > 0 && needsSummaryByFacts(gr.InventoryThread, st)
	if r.Title == "" {
		r.Title = gr.ThreadID
	}
	if m := prefixRE.FindStringSubmatch(gr.Title); m != nil {
		r.Prefix = m[0]
	}
	if gr.ProgressPct != nil {
		r.HasProgress = true
		r.Progress = *gr.ProgressPct
	}
	settle := gr.Facts.Settle
	if settle == nil {
		settle = st.Settle
	}
	if settle != nil {
		r.SettleLine = settle.Line
		r.LeftForYou = leftForYouList(settle.LeftForYou)
	}
	if len(r.StallReasons) == 0 {
		r.StallReasons = st.StallReasons
	}
	if r.FactsVerdict == "" {
		r.FactsVerdict = st.Verdict
	}
	for _, pr := range gr.PRs {
		if isOpenPR(pr.State) {
			r.PRs = append(r.PRs, pr)
		}
	}
	for _, u := range gr.UnwatchedChecks {
		if u != "" {
			r.UnwatchedChecks = append(r.UnwatchedChecks, UnwatchedCheck{URL: u, Number: prNumber(u, st.PRs)})
		}
	}
	if st.IdleMinutes != nil {
		r.HasIdle = true
		r.IdleMinutes = int(*st.IdleMinutes)
		r.IdleText = idleText(r.IdleMinutes)
	}
	r.CanAccept = gr.Group == "settled-open" && !gr.Acknowledged && settledAtRE.MatchString(gr.SettledAt)

	p, probed := d.probes[key]
	current := basisOf(p.revision, gr.InventoryThread)
	if e, ok := shown[key]; ok && e.Summary != nil {
		r.HasSummary = true
		r.SummaryModel = e.Model
		r.SummaryRole = e.Role
		if r.SummaryRole == "" {
			r.SummaryRole = d.cfg.Threads.SummaryRole
		}
		r.SummaryAt = e.MadeAt
		if probed && p.revision != "" && e.Basis != current {
			r.SummaryStale = true
		}
		// A summary of another settlement is stale even without a probe:
		// settled threads the facts call done, or accepted ones, are not
		// probed.
		if e.Basis.Bucket != gr.Bucket || e.Basis.SettledAt != gr.SettledAt {
			r.SummaryStale = true
		}
		// The inventory moved past the last good check of a thread deck
		// still summarises, and no later check has caught up (yet, or
		// because it failed): the summary may be out of date.
		checked := e.ThreadUpdatedAt
		if probed {
			checked = p.updatedAt
		}
		if !r.SummaryStale && needsSummaryByFacts(gr.InventoryThread, st) && newerTime(gr.UpdatedAt, checked) {
			r.SummaryUnchecked = true
		}
	}
	if e, ok := d.cache.get(key); ok && e.Error != "" && (!probed || e.AttemptBasis == current) {
		r.SummaryError = e.Error
	}
	switch {
	case d.inFlight[key]:
		r.SummaryState = "summarising"
	case queued[key]:
		r.SummaryState = "queued"
	}
	return r
}

// newerTime reports whether RFC 3339 time a is after b. An unreadable time
// is never newer or older.
func newerTime(a, b string) bool {
	ta, err := time.Parse(time.RFC3339, a)
	if err != nil {
		return false
	}
	tb, err := time.Parse(time.RFC3339, b)
	if err != nil {
		return false
	}
	return ta.After(tb)
}
