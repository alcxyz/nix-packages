package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"sort"
	"strings"
)

// NavItem is one link in the shell every page shares (ADR-0015): a view, the
// approvals page or a collection. Count and Tone flag what waits there.
type NavItem struct {
	Label string `json:"label"`
	URL   string `json:"url"`
	Count int    `json:"count,omitempty"`
	// Tone colours the count: "attn" waits on the owner.
	Tone string `json:"tone,omitempty"`
	// Collection marks a page collection, which the shell sets apart from
	// the views.
	Collection bool `json:"collection,omitempty"`
	Current    bool `json:"-"`
}

// NavView is the shell's links, with the one for the current page marked.
type NavView struct {
	Items []NavItem `json:"items"`
}

// navPath is where bundles' shell.js reads the links (ADR-0013). It is no
// collection slug, so it cannot shadow one.
const navPath = "/nav.json"

// navItems lists the overview, the views and the collections that have a
// page. Counts come from the in-process views only, so the pages-only mode
// links its views without counts.
func (s *Server) navItems() []NavItem {
	cfg := s.deck.cfg
	var items []NavItem
	// With the dashboard at the root there is no overview; the root is the
	// thread dashboard.
	if cfg.BasePath != "/" {
		items = append(items, NavItem{Label: "Overview", URL: "/"})
	}
	threads := NavItem{Label: "Threads", URL: cfg.BasePath}
	if counts, ok := s.deck.threadCounts(); ok && counts.User > 0 {
		threads.Count, threads.Tone = counts.User, "attn"
	}
	items = append(items, threads)
	if cfg.Backlog != nil {
		items = append(items, NavItem{Label: "Backlog", URL: cfg.Backlog.BasePath})
	}
	if cfg.Consent != nil {
		approvals := NavItem{Label: "Approvals", URL: consentBase}
		if n := len(s.pendingConsent()); n > 0 {
			approvals.Count, approvals.Tone = n, "attn"
		}
		items = append(items, approvals)
	}
	for _, name := range s.collectionNames() {
		items = append(items, NavItem{Label: CollectionView{Name: name}.Title(), URL: "/" + name + "/", Collection: true})
	}
	return items
}

// nav marks the item for the page at current: the overview only on the root,
// any other item for its path and everything below it.
func (s *Server) nav(current string) NavView {
	items := s.navItems()
	for i := range items {
		if items[i].URL == "/" {
			items[i].Current = current == "/"
		} else {
			items[i].Current = strings.HasPrefix(current, items[i].URL)
		}
	}
	return NavView{Items: items}
}

func (s *Server) navJSON(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(NavView{Items: s.navItems()})
}

// collectionNames lists the collections that hold at least one valid page,
// as the root index does (ADR-0013), sorted by name.
func (s *Server) collectionNames() []string {
	if !*s.deck.cfg.Collections {
		return nil
	}
	root, err := os.OpenRoot(s.deck.cfg.ContentDir)
	if err != nil {
		return nil
	}
	defer root.Close()
	entries, err := osReadContentDir(root)
	if err != nil {
		return nil
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() || !s.contentCollectionAvailable(entry.Name()) {
			continue
		}
		if collection, err := s.collection(root, entry.Name()); err == nil && len(collection.Pages) > 0 {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names
}

// threadCounts sums the instances' summaries without building the rows; ok
// is false until every instance has grouped facts, so a partial sum never
// passes for the whole, and in the pages-only mode.
func (d *Deck) threadCounts() (GroupSummary, bool) {
	if d.cfg.pagesOnly() {
		return GroupSummary{}, false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var sum GroupSummary
	for _, in := range d.cfg.Threads.Instances {
		f := d.facts[in.Name]
		if f == nil || f.grouped == nil {
			return GroupSummary{}, false
		}
		s := f.grouped.Summary
		sum.Unsettled += s.Unsettled
		sum.User += s.User
		sum.Agent += s.Agent
		sum.Stalled += s.Stalled
		sum.Settle += s.Settle
		sum.SettledOpen += s.SettledOpen
		sum.UnwatchedChecks += s.UnwatchedChecks
	}
	return sum, len(d.cfg.Threads.Instances) > 0
}

// pendingConsent lists the unexpired requests in the spool, newest first.
func (s *Server) pendingConsent() []consentRequest {
	if s.deck.cfg.Consent == nil {
		return nil
	}
	entries, err := os.ReadDir(s.deck.cfg.Consent.SpoolDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		s.log.Error("consent: cannot read the spool", "err", err.Error())
	}
	var requests []consentRequest
	for _, entry := range entries {
		id, ok := strings.CutSuffix(entry.Name(), ".request.json")
		if !ok {
			continue
		}
		if _, request, ok := s.openConsentRequest(id); ok {
			requests = append(requests, *request)
		}
	}
	sort.Slice(requests, func(i, j int) bool { return requests[i].CreatedAt > requests[j].CreatedAt })
	return requests
}

// needs is the overview's "Needs you" row: what waits on the owner in the
// threads, approvals and backlog this process runs. Each links to its view
// filtered to that count (deck.js reads #card=).
func (s *Server) needs() []NeedView {
	cfg := s.deck.cfg
	if cfg.pagesOnly() {
		return nil
	}
	counts, known := s.deck.threadCounts()
	base := cfg.BasePath
	needs := []NeedView{
		{Count: counts.User, Label: plural(counts.User, "thread waits on you", "threads wait on you"), URL: base + "#card=you", Tone: "attn", Known: known},
		{Count: counts.Stalled, Label: plural(counts.Stalled, "thread stalled", "threads stalled"), URL: base + "#card=stalled", Tone: "danger", Known: known},
		{Count: counts.Settle, Label: "ready to settle", URL: base + "#card=settle", Tone: "ok", Known: known},
	}
	if cfg.Consent != nil {
		n := len(s.pendingConsent())
		needs = append(needs, NeedView{Count: n, Label: plural(n, "approval waits", "approvals wait"), URL: consentBase, Tone: "attn", Known: true})
	}
	if s.deck.backlog != nil {
		b := s.deck.backlog.view(cfg.Backlog.BasePath)
		backlog := cfg.Backlog.BasePath
		needs = append(needs,
			NeedView{Count: b.Ready, Label: plural(b.Ready, "pull request ready", "pull requests ready"), URL: backlog + "#card=ready", Tone: "ok", Known: b.HasRun},
			NeedView{Count: b.MaybeDone, Label: plural(b.MaybeDone, "issue maybe done", "issues maybe done"), URL: backlog + "#card=maybe", Tone: "attn", Known: b.HasRun},
		)
	}
	return needs
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
