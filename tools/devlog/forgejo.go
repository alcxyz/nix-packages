package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// Forgejo is the primary activity source: most work is Forgejo-first, and
// GitHub search does not link forge squash merges or Forgejo PRs and issues
// to the account.

const (
	forgejoFeedPageSize = 50
	forgejoFeedMaxPages = 40
)

type forgejoClient struct {
	baseURL string
	user    string
	token   string
	http    *http.Client
}

// newForgejoClient reads the API token from FORGEJO_API_TOKEN_FILE, so the token
// never appears in arguments or the unit environment.
func newForgejoClient(baseURL, user string) (*forgejoClient, error) {
	path := os.Getenv("FORGEJO_API_TOKEN_FILE")
	if path == "" {
		return nil, fmt.Errorf("FORGEJO_API_TOKEN_FILE must be set when -forgejo-url is used")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read Forgejo token: %w", err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return nil, fmt.Errorf("Forgejo token file %s is empty", path)
	}
	return &forgejoClient{
		baseURL: strings.TrimRight(baseURL, "/") + "/api/v1",
		user:    user,
		token:   token,
		http:    &http.Client{Timeout: 60 * time.Second},
	}, nil
}

func (c *forgejoClient) get(path string) ([]byte, error) {
	req, err := http.NewRequest("GET", c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "token "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &forgejoStatusError{path: path, code: resp.StatusCode}
	}
	return body, nil
}

type forgejoStatusError struct {
	path string
	code int
}

func (e *forgejoStatusError) Error() string {
	return fmt.Sprintf("Forgejo %s returned %d", e.path, e.code)
}

type forgejoEvent struct {
	ID      int64     `json:"id"`
	OpType  string    `json:"op_type"`
	Content string    `json:"content"`
	Created time.Time `json:"created"`
	Repo    struct {
		FullName string `json:"full_name"`
	} `json:"repo"`
}

// fetchEvents returns the user's own events in the devlog window. The feed
// filters by calendar day in the server's time zone, so the days around the
// window are fetched and filtered by timestamp.
func (c *forgejoClient) fetchEvents(start, end time.Time) ([]forgejoEvent, error) {
	seen := make(map[int64]bool)
	var events []forgejoEvent
	for d := dateOnly(start).AddDate(0, 0, -1); !d.After(dateOnly(end).AddDate(0, 0, 1)); d = d.AddDate(0, 0, 1) {
		for page := 1; ; page++ {
			if page > forgejoFeedMaxPages {
				return nil, fmt.Errorf("Forgejo feed for %s exceeds %d pages", d.Format("2006-01-02"), forgejoFeedMaxPages)
			}
			q := url.Values{}
			q.Set("only-performed-by", "true")
			q.Set("date", d.Format("2006-01-02"))
			q.Set("limit", fmt.Sprint(forgejoFeedPageSize))
			q.Set("page", fmt.Sprint(page))
			body, err := c.get("/users/" + url.PathEscape(c.user) + "/activities/feeds?" + q.Encode())
			if err != nil {
				return nil, err
			}
			var batch []forgejoEvent
			if err := json.Unmarshal(body, &batch); err != nil {
				return nil, fmt.Errorf("parse Forgejo feed: %w", err)
			}
			// The server may cap the page size below the requested limit, so
			// only an empty page ends the day.
			if len(batch) == 0 {
				break
			}
			for _, e := range batch {
				if seen[e.ID] || !inDevlogWindow(e.Created, start, end) {
					continue
				}
				seen[e.ID] = true
				events = append(events, e)
			}
		}
	}
	return events, nil
}

type forgejoCommit struct {
	SHA    string `json:"sha"`
	Author *struct {
		Login string `json:"login"`
	} `json:"author"`
	Commit struct {
		Message string `json:"message"`
	} `json:"commit"`
}

func toCommitRefs(repo, user string, list []forgejoCommit) []commitRef {
	// A complete expansion may have no attributable commits.
	commits := make([]commitRef, 0, len(list))
	for _, rc := range list {
		// Unlinked authors are retained because the user's email may not be
		// associated with an account. The committer does not establish authorship.
		if rc.Author != nil && !strings.EqualFold(rc.Author.Login, user) {
			continue
		}
		commits = append(commits, commitRef{
			Source:  "forgejo",
			Repo:    repo,
			SHA:     rc.SHA,
			Message: strings.Split(rc.Commit.Message, "\n")[0],
		})
	}
	return commits
}

// fetchPushCommits lists attributable commits of a push the feed truncated: the
// compare range "base...head", or for an initial push, which has none, the
// newest total commits from head. Commits are not filtered by date: work
// committed earlier and pushed now is first visible in this push, and work
// reported on earlier days is skipped by the reported-commit record.
func (c *forgejoClient) fetchPushCommits(repo, baseHead, head string, total int) ([]commitRef, error) {
	if baseHead != "" {
		var list []forgejoCommit
		seen := map[string]bool{}
		for page := 1; page <= forgejoFeedMaxPages; page++ {
			body, err := c.get(fmt.Sprintf("/repos/%s/compare/%s?page=%d&limit=%d", repo, baseHead, page, forgejoFeedPageSize))
			if err != nil {
				return nil, err
			}
			var result struct {
				Commits []forgejoCommit `json:"commits"`
			}
			if err := json.Unmarshal(body, &result); err != nil {
				return nil, err
			}
			for _, commit := range result.Commits {
				if commit.SHA == "" || seen[commit.SHA] {
					return nil, fmt.Errorf("incomplete or repeated Forgejo compare page")
				}
				seen[commit.SHA] = true
				list = append(list, commit)
			}
			if len(list) == total {
				return toCommitRefs(repo, c.user, list), nil
			}
			if len(list) > total || len(result.Commits) == 0 {
				return nil, fmt.Errorf("incomplete Forgejo compare")
			}
		}
		return nil, fmt.Errorf("Forgejo compare pagination limit reached")
	}

	var list []forgejoCommit
	seen := map[string]bool{}
	for page := 1; len(list) < total; page++ {
		if page > forgejoFeedMaxPages {
			return nil, fmt.Errorf("history of %s@%s exceeds %d pages", repo, head, forgejoFeedMaxPages)
		}
		q := url.Values{}
		q.Set("sha", head)
		q.Set("page", fmt.Sprint(page))
		q.Set("limit", fmt.Sprint(forgejoFeedPageSize))
		q.Set("stat", "false")
		q.Set("verification", "false")
		q.Set("files", "false")
		body, err := c.get("/repos/" + repo + "/commits?" + q.Encode())
		if err != nil {
			return nil, err
		}
		var batch []forgejoCommit
		if err := json.Unmarshal(body, &batch); err != nil {
			return nil, fmt.Errorf("parse Forgejo commits: %w", err)
		}
		if len(batch) == 0 {
			break
		}
		for _, commit := range batch {
			if len(list) == total {
				break
			}
			if commit.SHA == "" || seen[commit.SHA] {
				return nil, fmt.Errorf("incomplete or repeated Forgejo history page")
			}
			seen[commit.SHA] = true
			list = append(list, commit)
		}
	}
	if len(list) < total {
		return nil, fmt.Errorf("incomplete Forgejo push history")
	}
	if len(list) > total {
		list = list[:total]
	}
	return toCommitRefs(repo, c.user, list), nil
}

func (c *forgejoClient) fetchDiff(repo, sha string) (string, error) {
	body, err := c.get("/repos/" + repo + "/git/commits/" + url.PathEscape(sha) + ".diff")
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// Actions on pull requests and issues, by feed operation type. Comment
// operations carry the comment text instead of the title.
var forgejoPRActions = map[string]string{
	"create_pull_request":           "opened",
	"merge_pull_request":            "merged",
	"auto_merge_pull_request":       "auto-merged",
	"close_pull_request":            "closed",
	"reopen_pull_request":           "reopened",
	"approve_pull_request":          "approved",
	"reject_pull_request":           "requested changes",
	"comment_pull":                  "commented",
	"pull_request_ready_for_review": "marked ready for review",
	"pull_review_dismissed":         "dismissed a review",
}

// titledActions carry the item's title; the others carry comment or review
// text.
var titledActions = map[string]bool{
	"opened":      true,
	"merged":      true,
	"auto-merged": true,
	"closed":      true,
	"reopened":    true,
}

var forgejoIssueActions = map[string]string{
	"create_issue":  "opened",
	"close_issue":   "closed",
	"reopen_issue":  "reopened",
	"comment_issue": "commented",
}

type forgejoItem struct {
	repo    string
	number  string
	title   string
	actions []string
}

// pushCommitsFunc lists all commits of a push whose feed entry was truncated.
type pushCommitsFunc func(repo, baseHead, head string, total int) ([]commitRef, error)

// summarizeForgejoEvents turns feed events into commits, and pull request and
// issue lines with each item's actions in chronological order. Mirror syncs
// are not the user's work and are skipped, as are pushes without commit
// content, such as feature-branch pushes whose work arrives through the PR.
func summarizeForgejoEvents(events []forgejoEvent, pushCommits pushCommitsFunc) ([]commitRef, string, string, error) {
	var commits []commitRef
	prs := make(map[string]*forgejoItem)
	issues := make(map[string]*forgejoItem)
	var prOrder, issueOrder []string

	sorted := make([]forgejoEvent, len(events))
	copy(sorted, events)
	sort.SliceStable(sorted, func(i, j int) bool {
		if !sorted[i].Created.Equal(sorted[j].Created) {
			return sorted[i].Created.Before(sorted[j].Created)
		}
		return sorted[i].ID < sorted[j].ID
	})

	for _, e := range sorted {
		if e.OpType == "commit_repo" {
			if e.Content == "" {
				continue
			}
			var push struct {
				Commits []struct {
					Sha1    string
					Message string
				}
				Len        int
				CompareURL string
				HeadCommit struct {
					Sha1 string
				}
			}
			if err := json.Unmarshal([]byte(e.Content), &push); err != nil {
				return nil, "", "", fmt.Errorf("invalid Forgejo push: %w", err)
			}
			if push.Len > len(push.Commits) {
				all, err := expandPush(e.Repo.FullName, push.CompareURL, push.HeadCommit.Sha1, len(push.Commits), push.Len, pushCommits)
				if err != nil {
					return nil, "", "", err
				}
				if all != nil {
					commits = append(commits, all...)
					continue
				}
			}
			for _, pc := range push.Commits {
				commits = append(commits, commitRef{
					Source:  "forgejo",
					Repo:    e.Repo.FullName,
					SHA:     pc.Sha1,
					Message: strings.Split(pc.Message, "\n")[0],
				})
			}
			continue
		}

		items, order := prs, &prOrder
		action, ok := forgejoPRActions[e.OpType]
		if !ok {
			items, order = issues, &issueOrder
			if action, ok = forgejoIssueActions[e.OpType]; !ok {
				continue
			}
		}
		var fields []string
		if err := json.Unmarshal([]byte(e.Content), &fields); err != nil || len(fields) == 0 {
			return nil, "", "", fmt.Errorf("invalid Forgejo activity record %d", e.ID)
		}
		key := e.Repo.FullName + "#" + fields[0]
		item, ok := items[key]
		if !ok {
			item = &forgejoItem{repo: e.Repo.FullName, number: fields[0]}
			items[key] = item
			*order = append(*order, key)
		}
		if titledActions[action] && len(fields) > 1 && fields[1] != "" {
			item.title = fields[1]
		}
		if len(item.actions) == 0 || item.actions[len(item.actions)-1] != action {
			item.actions = append(item.actions, action)
		}
	}

	return commits, formatForgejoItems(prs, prOrder), formatForgejoItems(issues, issueOrder), nil
}

// expandPush refuses a truncated push when its full history is unavailable.
func expandPush(repo, compareURL, head string, listed, total int, pushCommits pushCommitsFunc) ([]commitRef, error) {
	_, baseHead, ok := strings.Cut(compareURL, "/compare/")
	if !ok && head == "" {
		return nil, fmt.Errorf("push to %s lists %d of %d commits without a head", repo, listed, total)
	}
	all, err := pushCommits(repo, baseHead, head, total)
	if err != nil {
		return nil, fmt.Errorf("list commits of push to %s: %w", repo, err)
	}
	return all, nil
}

func formatForgejoItems(items map[string]*forgejoItem, order []string) string {
	var lines []string
	for _, key := range order {
		item := items[key]
		line := fmt.Sprintf("- [%s] #%s", item.repo, item.number)
		if item.title != "" {
			line += " " + item.title
		}
		lines = append(lines, line+" ("+strings.Join(item.actions, ", ")+")")
	}
	return strings.Join(lines, "\n")
}
