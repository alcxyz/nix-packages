package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"revkit/review"
)

const matchPrompt = `You are matching review findings about the repository checked out in the
current directory. Known findings were reported by earlier runs and have ids
like F12. New findings come from this run; several may describe the same
problem in other words or with other line numbers.

For each new finding, decide whether it describes the same problem as a known
finding or as an earlier new finding in the list: the same underlying defect,
however it is worded and whichever lines it cites. Different problems in the
same file are not the same. Read the cited files when the text alone does not
settle it.

Reply with ONLY a JSON object, no prose before or after, one entry per new
finding:
{"matches": [{"id": "<new finding id>", "same": "<known id, earlier new id, or null>"}]}
`

// knownLimit bounds how many known findings the matcher is shown.
const knownLimit = 200

type matchEntry struct {
	ID   string  `json:"id"`
	Same *string `json:"same"`
}

// remember matches this run's findings with each other and with the
// repository's store, then records them there: a new problem gets an ID, a
// known one another sighting with its status kept. The findings get their
// store IDs only once the store is saved.
func (r *reviewRun) remember(ctx context.Context, lenses []lens) error {
	if len(r.report.Findings) == 0 {
		return nil
	}
	known, err := readStore(r.storePath)
	if err != nil {
		return err
	}
	inRun := map[string]bool{}
	for _, l := range lenses {
		inRun[l.Name] = true
	}
	var candidates []storedFinding
	for _, sf := range known.Findings {
		for _, name := range sf.lensSet() {
			if inRun[name] {
				candidates = append(candidates, sf)
				break
			}
		}
	}
	// Only the matcher's prompt is bounded; title matching below sees the
	// whole store.
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].LastSeen.Time.After(candidates[j].LastSeen.Time)
	})
	if len(candidates) > knownLimit {
		candidates = candidates[:knownLimit]
	}
	targets, mode := r.matchByMeaning(ctx, candidates)
	r.report.Matching = mode
	return r.store(targets, candidates)
}

// store records the run's findings in the store, given the matcher's
// targets (nil without an answer) and the known problems it was shown.
func (r *reviewRun) store(targets map[string]string, candidates []storedFinding) error {
	// A resumed review is one review: its findings share the first run's key,
	// so resuming adds no sighting and their problems still count as new.
	seen := sighting{Commit: r.report.Commit, Run: r.report.Review, Time: time.Now().UTC()}
	type assignment struct{ id, status string }
	assigned := make([]assignment, len(r.report.Findings))
	// When the matcher answered, its "no match" stands for every problem it
	// was shown; titles only decide against problems it did not see: older
	// ones beyond its prompt, or ones another run added meanwhile.
	judged := map[string]bool{}
	if targets != nil {
		for _, sf := range candidates {
			judged[sf.ID] = true
		}
	}
	err := withStore(r.storePath, func(s *findingStore) error {
		// Under the lock, findings without a match are matched by lens and
		// title against the store as it is now.
		byTitle := map[string]string{}
		for _, sf := range s.Findings {
			if judged[sf.ID] {
				continue
			}
			for _, name := range sf.lensSet() {
				if key := titleKey(name, sf.Title); byTitle[key] == "" {
					byTitle[key] = sf.ID
				}
			}
		}
		findings := r.report.Findings
		carried := func(f finding) string {
			if sf := s.find(f.carried); f.carried != "" && sf != nil {
				return sf.ID
			}
			return ""
		}
		// claimed holds the problems already taken when the matcher
		// answered: kept by a reused finding (reserved before any group is
		// placed) or joined by title. Findings the matcher told apart cannot
		// share one through a title.
		claimed := map[string]bool{}
		if targets != nil {
			for _, f := range findings {
				if id := carried(f); id != "" {
					claimed[id] = true
				}
			}
		}
		place := func(i int, id string) string {
			f := findings[i]
			if id == "" {
				id = s.add(f, seen)
			} else {
				s.find(id).sight(f, seen)
			}
			if key := titleKey(f.Lens, f.Title); byTitle[key] == "" && targets == nil {
				byTitle[key] = id
			}
			sf := s.find(id)
			assigned[i] = assignment{id: id, status: sf.Status}
			if sf.FirstSeen.Run == seen.Run && sf.Status == storeOpen {
				// First reported in this review, and not dismissed since.
				assigned[i].status = "new"
			}
			return id
		}
		for _, group := range matchGroups(findings, targets) {
			// The group's problem: the first one a member keeps, else the
			// known problem the matcher named, else an unclaimed title
			// match, else a new one, made by the first member placed.
			anchor := ""
			for _, i := range group {
				if anchor = carried(findings[i]); anchor != "" {
					break
				}
			}
			for _, i := range group {
				if anchor != "" {
					break
				}
				if sf := s.find(targets[findings[i].ID]); targets[findings[i].ID] != "" && sf != nil {
					anchor = sf.ID
				}
			}
			for _, i := range group {
				if anchor != "" {
					break
				}
				if id := byTitle[titleKey(findings[i].Lens, findings[i].Title)]; id != "" && !claimed[id] {
					anchor = id
					if targets != nil {
						claimed[id] = true
					}
				}
			}
			for _, i := range group {
				// A reused finding keeps its problem whatever the matcher
				// says now.
				if id := carried(findings[i]); id != "" {
					place(i, id)
					continue
				}
				anchor = place(i, anchor)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for i := range r.report.Findings {
		r.report.Findings[i].StoreID, r.report.Findings[i].StoreStatus = assigned[i].id, assigned[i].status
	}
	return nil
}

// matchByMeaning asks the configured matcher which findings describe the
// same problem. It returns, for each finding, the ID of a known finding or of
// an earlier finding of the run it repeats (none for a new problem), and how
// the run matched; with no answer the targets are nil and titles decide.
func (r *reviewRun) matchByMeaning(ctx context.Context, known []storedFinding) (map[string]string, string) {
	switch {
	case r.cfg.Matcher == nil:
		return nil, "by lens and title: no matcher is configured"
	case ctx.Err() != nil:
		return nil, "by lens and title: the run was interrupted"
	case len(known) == 0 && len(r.report.Findings) < 2:
		return nil, "nothing to match"
	}
	type entry struct {
		ID    string   `json:"id"`
		Lens  string   `json:"lens"`
		Title string   `json:"title"`
		Paths []string `json:"paths"`
		Claim string   `json:"claim"`
	}
	knownEntries := make([]entry, len(known))
	for i, sf := range known {
		knownEntries[i] = entry{ID: sf.ID, Lens: strings.Join(sf.lensSet(), ", "), Title: sf.Title, Paths: sf.Paths, Claim: sf.Claim}
	}
	newEntries := make([]entry, len(r.report.Findings))
	for i, f := range r.report.Findings {
		newEntries[i] = entry{ID: f.ID, Lens: f.Lens, Title: f.Title, Paths: f.Paths, Claim: f.Claim}
	}
	encodedKnown, _ := json.MarshalIndent(knownEntries, "", "  ")
	encodedNew, _ := json.MarshalIndent(newEntries, "", "  ")
	prompt := matchPrompt + "\nKnown findings:\n" + string(encodedKnown) + "\n\nNew findings:\n" + string(encodedNew) + "\n"

	matcher := *r.cfg.Matcher
	r.logf("%s matches %d findings with %d known: started", matcher.Name, len(newEntries), len(knownEntries))
	result := review.Run(ctx, r.job(matcher, "match."+matcher.Name, prompt))
	record := newRecord("match", matcher, result)
	record.Findings = len(newEntries)
	var targets map[string]string
	if result.Status == review.StatusOK {
		var problem string
		targets, problem = parseMatches(result.Text, r.report.Findings, known)
		if problem != "" {
			record.Status, record.Problem = statusUnparsed, problem
		}
	}
	mode := "by meaning"
	if record.Status != review.StatusOK {
		targets = nil
		mode = "by lens and title: the matcher failed (" + record.Problem + ")"
	}
	r.mu.Lock()
	r.report.Jobs = append(r.report.Jobs, record)
	r.mu.Unlock()
	r.logf("%s matches: %s, %ds%s", matcher.Name, record.Status, record.Seconds, problemSuffix(record))
	return targets, mode
}

// parseMatches reads the matcher's reply. A target must be a known ID or a
// new finding listed before the one it repeats, so answers cannot form a
// cycle; anything else counts as no match. Every new finding needs an entry.
func parseMatches(text string, findings []finding, known []storedFinding) (map[string]string, string) {
	var reply struct {
		Matches []json.RawMessage `json:"matches"`
		Error   string            `json:"error"`
	}
	if problem := decodeReply(text, &reply, &reply.Error); problem != "" {
		return nil, problem
	}
	order := map[string]int{}
	for i, f := range findings {
		order[f.ID] = i
	}
	knownIDs := map[string]bool{}
	for _, sf := range known {
		knownIDs[strings.ToUpper(sf.ID)] = true
	}
	targets := map[string]string{}
	answered := map[string]bool{}
	for _, raw := range reply.Matches {
		var m matchEntry
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		position, ok := order[m.ID]
		if !ok {
			continue
		}
		answered[m.ID] = true
		if m.Same == nil {
			continue
		}
		same := strings.TrimSpace(*m.Same)
		switch {
		case knownIDs[strings.ToUpper(same)]:
			targets[m.ID] = strings.ToUpper(same)
		default:
			if earlier, ok := order[same]; ok && earlier < position {
				targets[m.ID] = same
			}
		}
	}
	if len(answered) < len(findings) {
		return nil, fmt.Sprintf("%d of %d findings got no match entry", len(findings)-len(answered), len(findings))
	}
	return targets, ""
}

// runName names this run: its directory under the commit.
func (r *reviewRun) runName() string {
	return filepath.Base(r.dir)
}

// matchGroups groups the run's findings the matcher said repeat each other,
// following every link between findings of the run (links to known problems
// do not group). Groups and their members are in report order.
func matchGroups(findings []finding, targets map[string]string) [][]int {
	index := map[string]int{}
	for i, f := range findings {
		index[f.ID] = i
	}
	parent := make([]int, len(findings))
	for i := range parent {
		parent[i] = i
	}
	var root func(int) int
	root = func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	for i, f := range findings {
		if j, ok := index[targets[f.ID]]; ok && targets[f.ID] != "" {
			parent[root(i)] = root(j)
		}
	}
	byRoot := map[int]int{}
	var groups [][]int
	for i := range findings {
		r := root(i)
		g, ok := byRoot[r]
		if !ok {
			g = len(groups)
			byRoot[r] = g
			groups = append(groups, nil)
		}
		groups[g] = append(groups[g], i)
	}
	return groups
}
