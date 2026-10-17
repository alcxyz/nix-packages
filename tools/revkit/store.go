package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Store statuses. A dismissed or filed finding keeps its status when a later
// run sees it again, so it is not reported as new.
const (
	storeOpen      = "open"
	storeDismissed = "dismissed"
	storeFiled     = "filed"
)

// storedFinding is one distinct problem, however many runs and reviewers
// reported it. The text is the latest sighting's; paths and reviewers
// accumulate.
type storedFinding struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	// Lens is the lens that first reported the problem; Lenses every lens
	// that has, since the matcher may join findings across lenses.
	Lens       string   `json:"lens"`
	Lenses     []string `json:"lenses"`
	Title      string   `json:"title"`
	Severity   string   `json:"severity"`
	Security   bool     `json:"security"`
	Paths      []string `json:"paths"`
	Claim      string   `json:"claim"`
	Evidence   string   `json:"evidence"`
	Suggestion string   `json:"suggestion"`
	// Verdict and Reason are the latest verification.
	Verdict   string   `json:"verdict"`
	Reason    string   `json:"reason,omitempty"`
	Reviewers []string `json:"reviewers"`
	FirstSeen sighting `json:"firstSeen"`
	LastSeen  sighting `json:"lastSeen"`
	// Reviews are the reviews that reported it; Sightings counts them.
	// DescribedBy is the review whose report the text and verdict are, at
	// DescribedCommit through DescribedLens.
	Reviews         []string `json:"reviews"`
	DescribedBy     string   `json:"describedBy"`
	DescribedCommit string   `json:"describedCommit"`
	DescribedLens   string   `json:"describedLens"`
	Sightings       int      `json:"sightings"`
	// Issue is the URL of the issue it was filed as; Note says why it was
	// dismissed.
	Issue string `json:"issue,omitempty"`
	Note  string `json:"note,omitempty"`
}

type sighting struct {
	Commit string    `json:"commit"`
	Run    string    `json:"run"`
	Time   time.Time `json:"time"`
}

type findingStore struct {
	Version int `json:"version"`
	// Store is a random ID chosen when the store is first saved; with a
	// finding's ID it names the issue revkit filed for that finding, since
	// every store numbers its findings from F1.
	Store    string          `json:"store,omitempty"`
	NextID   int             `json:"nextId"`
	Findings []storedFinding `json:"findings"`
}

// storePath is the findings store of one repository.
func storePath(stateDir, repository string) string {
	return filepath.Join(stateDir, "repo-review", repository, "findings.json")
}

// withStore runs change on the store at path under an exclusive lock and
// saves it if change returns nil, so concurrent runs and commands do not
// lose each other's updates. errUnchanged from change leaves the store as it
// was, but for an ID given to a store that had none.
func withStore(path string, change func(*findingStore) error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	s, err := readStore(path)
	if err != nil {
		return err
	}
	// Stores from 0.2 have no ID yet; one is saved even if change makes no
	// other, so the ID a filed issue names stays the store's.
	named := s.Store == ""
	if named {
		random := make([]byte, 8)
		if _, err := rand.Read(random); err != nil {
			return err
		}
		s.Store = hex.EncodeToString(random)
	}
	changed := change(s)
	if changed != nil && !(named && changed == errUnchanged) {
		return changed
	}
	encoded, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	staging := path + ".new"
	if err := os.WriteFile(staging, append(encoded, '\n'), 0o644); err != nil {
		return err
	}
	if err := os.Rename(staging, path); err != nil {
		return err
	}
	return changed
}

// readStore reads the store at path; a missing one is empty.
func readStore(path string) (*findingStore, error) {
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &findingStore{Version: 1, NextID: 1}, nil
	}
	if err != nil {
		return nil, err
	}
	var s findingStore
	if err := json.Unmarshal(content, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if s.Version != 1 {
		return nil, fmt.Errorf("%s: unknown store version %d", path, s.Version)
	}
	return &s, nil
}

func (s *findingStore) find(id string) *storedFinding {
	for i := range s.Findings {
		if strings.EqualFold(s.Findings[i].ID, id) {
			return &s.Findings[i]
		}
	}
	return nil
}

// add stores a new problem and returns its ID.
func (s *findingStore) add(f finding, seen sighting) string {
	id := "F" + strconv.Itoa(s.NextID)
	s.NextID++
	s.Findings = append(s.Findings, storedFinding{
		ID: id, Status: storeOpen, FirstSeen: seen, Sightings: 0,
	})
	s.Findings[len(s.Findings)-1].sight(f, seen)
	return id
}

// verdictStrength ranks verdicts for choosing among the findings of one
// review that describe the same problem: any verification beats none.
var verdictStrength = map[string]int{"confirmed": 3, "partly": 2, "rejected": 1, "unverified": 0}

// sight records another report of a stored problem, keeping its status.
// Within the review that describes it, the best-verified report wins; a
// report from a later review replaces it, unless that report went
// unverified; an earlier review, resumed, only replaces an unverified one.
// Reviews are named by their first run, which starts with its UTC start
// time to the nanosecond, so names order them.
func (sf *storedFinding) sight(f finding, seen sighting) {
	sameReview := slices.Contains(sf.Reviews, seen.Run)
	var describe bool
	switch {
	case sf.DescribedBy == "" && len(sf.Reviews) == 0:
		describe = true
	case seen.Run == sf.DescribedBy:
		describe = verdictStrength[f.Verdict] > verdictStrength[sf.Verdict]
	case seen.Run > sf.DescribedBy:
		describe = f.Verdict != "unverified" || sf.Verdict == "unverified"
	default:
		// An earlier review, resumed: only to verify what went unverified.
		describe = sf.Verdict == "unverified" && f.Verdict != "unverified"
	}
	if describe {
		sf.Title, sf.Severity, sf.Claim, sf.Evidence, sf.Suggestion = f.Title, f.Severity, f.Claim, f.Evidence, f.Suggestion
		sf.Verdict, sf.Reason, sf.DescribedBy = f.Verdict, f.Reason, seen.Run
		sf.DescribedCommit, sf.DescribedLens = seen.Commit, f.Lens
	}
	if sf.Lens == "" {
		sf.Lens = f.Lens
	}
	if !slices.Contains(sf.Lenses, f.Lens) {
		sf.Lenses = append(sf.Lenses, f.Lens)
	}
	sf.Security = sf.Security || f.Security
	for _, path := range f.Paths {
		if !slices.Contains(sf.Paths, path) {
			sf.Paths = append(sf.Paths, path)
		}
	}
	if !slices.Contains(sf.Reviewers, f.Reviewer) {
		sf.Reviewers = append(sf.Reviewers, f.Reviewer)
		sort.Strings(sf.Reviewers)
	}
	if !sameReview {
		sf.Reviews = append(sf.Reviews, seen.Run)
	}
	sf.Sightings = len(sf.Reviews)
	if seen.Run >= sf.LastSeen.Run {
		// A resumed earlier review does not move the last sighting back.
		sf.LastSeen = seen
	}
}

// lensSet is every lens that reported the problem.
func (sf storedFinding) lensSet() []string {
	if len(sf.Lenses) == 0 {
		return []string{sf.Lens}
	}
	return sf.Lenses
}

// titleKey is the identity of a finding when no matcher compared meanings:
// its lens and title, ignoring case and spacing.
func titleKey(lens, title string) string {
	return lens + "\x00" + strings.Join(strings.Fields(strings.ToLower(title)), " ")
}
