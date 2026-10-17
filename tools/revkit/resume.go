package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"revkit/review"
)

func promptSHA(prompt string) string {
	sum := sha256.Sum256([]byte(prompt))
	return hex.EncodeToString(sum[:])
}

// latestRun returns the earlier run under runs that started last and left
// results, and its directory name. Directory names carry the start second
// only, so the recorded start time decides.
func latestRun(runs string) (*runReport, string, error) {
	entries, err := os.ReadDir(runs)
	if err != nil {
		return nil, "", err
	}
	var latest *runReport
	var name string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		content, err := os.ReadFile(filepath.Join(runs, entry.Name(), "results.json"))
		if err != nil {
			continue
		}
		var report runReport
		if json.Unmarshal(content, &report) != nil {
			continue
		}
		if latest == nil || report.Started.After(latest.Started) {
			latest, name = &report, entry.Name()
		}
	}
	if latest == nil {
		return nil, "", fmt.Errorf("no earlier run on this commit left results")
	}
	return latest, name, nil
}

// findingNumber is the position of a finding in its reviewer's reply, the
// last part of its ID.
func findingNumber(id string) int {
	n, _ := strconv.Atoi(id[strings.LastIndex(id, ".")+1:])
	return n
}

func sameReviewer(job jobRecord, r review.Reviewer) bool {
	return job.Reviewer == r.Name && job.Client == r.Client && job.Model == r.Model && job.Effort == r.Effort
}

// reuseFind returns the earlier run's completed find job with the same
// reviewer and prompt, and its findings, to be verified again.
func (r *reviewRun) reuseFind(reviewer review.Reviewer, lensName, sha string) (jobRecord, []finding, bool) {
	if r.previous == nil {
		return jobRecord{}, nil, false
	}
	for _, job := range r.previous.Jobs {
		if job.Kind != "find" || job.Lens != lensName || job.PromptSHA != sha || job.Status != review.StatusOK || !sameReviewer(job, reviewer) {
			continue
		}
		job.ReusedFrom = r.previousName
		var findings []finding
		for _, f := range r.previous.Findings {
			if f.Reviewer == reviewer.Name && f.Lens == lensName {
				// The store ID is carried: it ties the finding to the
				// problem it joined in the first part of the review.
				f.carried = f.StoreID
				f.Verdict, f.Verifier, f.Reason, f.StoreID, f.StoreStatus = "unverified", "", "", "", ""
				findings = append(findings, f)
			}
		}
		if len(findings) != job.Findings {
			return jobRecord{}, nil, false
		}
		// The report sorts findings by verdict; the reply's order is what
		// the verification prompt, and its hash, were built from.
		sort.SliceStable(findings, func(i, j int) bool {
			return findingNumber(findings[i].ID) < findingNumber(findings[j].ID)
		})
		return job, findings, true
	}
	return jobRecord{}, nil, false
}

// reuseVerify returns the earlier run's completed verification by the same
// checker of the same findings, and the verdicts it gave.
func (r *reviewRun) reuseVerify(checker review.Reviewer, author, sha string) (jobRecord, []verdict, bool) {
	if r.previous == nil {
		return jobRecord{}, nil, false
	}
	for _, job := range r.previous.Jobs {
		if job.Kind != "verify" || job.Author != author || job.PromptSHA != sha || job.Status != review.StatusOK || !sameReviewer(job, checker) {
			continue
		}
		job.ReusedFrom = r.previousName
		var verdicts []verdict
		for _, f := range r.previous.Findings {
			if f.Reviewer == author && f.Verifier == checker.Name {
				verdicts = append(verdicts, verdict{ID: f.ID, Verdict: f.Verdict, Reason: f.Reason})
			}
		}
		return job, verdicts, true
	}
	return jobRecord{}, nil, false
}
