package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// forge files issues through the Forgejo (Gitea-compatible) REST API. The
// token is read from its file when filing starts and only ever sent in the
// Authorization header.
type forge struct {
	tracker *issueTracker
	token   string
	client  *http.Client
}

type forgeIssue struct {
	Number  int    `json:"number"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
}

// issuePage is the page size issue listings ask for; the forge may cap it
// lower. maxOpenIssues bounds the duplicate check, and maxTitle the length
// of an issue title.
const (
	issuePage     = 50
	maxOpenIssues = 2000
	maxTitle      = 200
)

// forgeTransport is the HTTP transport for forge requests; nil is the
// default one. Tests point it at a test server.
var forgeTransport http.RoundTripper

func newForge(tracker *issueTracker) (*forge, error) {
	content, err := os.ReadFile(tracker.TokenFile)
	if err != nil {
		return nil, fmt.Errorf("cannot read the token file %s: %w", tracker.TokenFile, err)
	}
	token := strings.TrimSpace(string(content))
	if token == "" || strings.ContainsAny(token, "\r\n") {
		return nil, fmt.Errorf("the token file %s holds no single-line token", tracker.TokenFile)
	}
	return &forge{tracker: tracker, token: token, client: &http.Client{
		Timeout:   30 * time.Second,
		Transport: forgeTransport,
		// A redirect could carry the token to another host.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func (f *forge) request(ctx context.Context, method, path string, query url.Values, body any, out any) error {
	endpoint := strings.TrimRight(f.tracker.URL, "/") + "/api/v1/repos/" + url.PathEscape(f.tracker.Owner) + "/" +
		url.PathEscape(f.tracker.Repo) + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "token "+f.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	content, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var message struct {
			Message string `json:"message"`
		}
		json.Unmarshal(content, &message)
		return fmt.Errorf("%s %s: %s %s", method, path, resp.Status, oneLine(message.Message))
	}
	if out != nil {
		return json.Unmarshal(content, out)
	}
	return nil
}

// openIssues lists every open issue of the tracker, page by page until an
// empty page (the forge may serve fewer per page than asked), from the
// database rather than the search index, which can lag behind new issues.
func (f *forge) openIssues(ctx context.Context) ([]forgeIssue, error) {
	var all []forgeIssue
	for page := 1; ; page++ {
		var issues []forgeIssue
		query := url.Values{"state": {"open"}, "type": {"issues"}, "limit": {strconv.Itoa(issuePage)}, "page": {strconv.Itoa(page)}}
		if err := f.request(ctx, http.MethodGet, "/issues", query, nil, &issues); err != nil {
			return nil, err
		}
		if len(issues) == 0 {
			return all, nil
		}
		all = append(all, issues...)
		if len(all) > maxOpenIssues {
			return nil, fmt.Errorf("more than %d open issues; cannot check them all for duplicates", maxOpenIssues)
		}
	}
}

func (f *forge) createIssue(ctx context.Context, title, body string) (*forgeIssue, error) {
	var issue forgeIssue
	err := f.request(ctx, http.MethodPost, "/issues", nil, map[string]string{"title": title, "body": body}, &issue)
	if err != nil {
		return nil, err
	}
	if issue.HTMLURL == "" {
		return nil, fmt.Errorf("the forge returned no issue URL")
	}
	return &issue, nil
}

// fileable says why a stored finding may not be filed, or "".
func fileable(sf storedFinding) string {
	switch {
	case sf.Security:
		return "it is security-relevant, and those are never filed"
	case sf.Status == storeFiled:
		return "it is already filed as " + sf.Issue
	case sf.Status != storeOpen:
		return "it is " + sf.Status
	case sf.Verdict != "confirmed":
		return "its verdict is " + sf.Verdict + ", and only confirmed findings are filed"
	}
	return ""
}

// zeroWidth breaks mentions and references in titles without showing.
const zeroWidth = '\u200b'

// fenced shows reviewer text exactly as written, in a fenced code block: the
// forge renders nothing inside one and runs no mention, reference, link or
// short-link processing on it, and its cross-reference scanner skips code.
// The fence is longer than any run of backticks in the text, so the text
// cannot close it. Reviewers read the repository, which may hold text
// written to steer them.
func fenced(text string) string {
	text = strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(text)
	lines := strings.Split(text, "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	text = strings.Join(lines, "\n")
	longest, run := 0, 0
	for _, r := range text {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", max(3, longest+1))
	return fence + "text\n" + text + "\n" + fence
}

// codeSpan shows reviewer text, such as a path, as one line of code: no
// backticks, so the span cannot close early, and nothing inside it renders.
func codeSpan(text string) string {
	return "`" + strings.ReplaceAll(oneLine(text), "`", "'") + "`"
}

// titleBreaks are where an issue title gets a zero-width break: titles are
// not Markdown, but the forge finds mentions (@), issue and pull request
// references (#, !) and links to its own issues (://) in them.
var titleBreaks = strings.NewReplacer("@", "@\u200b", "#", "#\u200b", "!", "!\u200b", "://", ":\u200b//")

// issueTitle is a finding's issue title: one line, mentions, references and
// links broken, at most maxTitle characters.
func issueTitle(sf storedFinding) string {
	title := []rune(titleBreaks.Replace(oneLine(sf.Title)))
	if len(title) > maxTitle {
		title = append(title[:maxTitle-1], '…')
	}
	return string(title)
}

// issueTitleKey compares issue titles: case, spacing and zero-width breaks do
// not count.
func issueTitleKey(title string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.ReplaceAll(title, string(zeroWidth), ""))), " ")
}

// issueBody is the issue text for a finding of the given store. It names no
// model: what matters is that a second reviewer confirmed it against the
// code. The commit and lens are those of the review whose report the text
// is.
func issueBody(store string, sf storedFinding) string {
	paths := make([]string, len(sf.Paths))
	for i, path := range sf.Paths {
		paths[i] = codeSpan(path)
	}
	// Stores from 0.2 have no describing commit; recover it from the
	// sighting of the describing review, or cite none rather than a wrong one.
	commit, lensName := sf.DescribedCommit, sf.DescribedLens
	if commit == "" {
		lensName = sf.Lens
		switch sf.DescribedBy {
		case sf.FirstSeen.Run:
			commit = sf.FirstSeen.Commit
		case sf.LastSeen.Run:
			commit = sf.LastSeen.Commit
		}
	}
	at := "at an earlier commit"
	if commit != "" {
		at = "at " + codeSpan(short(commit))
	}
	var b strings.Builder
	b.WriteString("**Status:** Confirmed by a second reviewer against the code.\n\n")
	b.WriteString("**Next:** Resolve the finding and verify the correction.\n\n")
	b.WriteString("**Owner:** Unassigned.\n\n")
	fmt.Fprintf(&b, "**Why:** The confirmed finding is described below.\n\n%s\n\n", fenced(sf.Claim))
	b.WriteString("**Plan:**\n1. Review the affected paths and suggested fix below.\n2. Implement and verify the correction.\n\n")
	b.WriteString("**Done when:**\n- [ ] The confirmed finding is resolved and the correction is verified.\n\n")
	b.WriteString("<details>\n<summary>Context and evidence</summary>\n\n")
	fmt.Fprintf(&b, "**Evidence:**\n\n%s\n\n**Where:** %s\n\n**Suggested fix:**\n\n%s\n\n",
		fenced(sf.Evidence), strings.Join(paths, ", "), fenced(sf.Suggestion))
	b.WriteString("**How this was found:** ")
	fmt.Fprintf(&b, "A read-only review of the whole repository (`revkit repo-review`, lens %s) reported this %s", codeSpan(lensName), at)
	if sf.Sightings > 1 {
		fmt.Fprintf(&b, "; %d reviews have reported it", sf.Sightings)
	}
	b.WriteString(". A second reviewer checked it against the code and confirmed it")
	if reason := strings.TrimSpace(sf.Reason); reason != "" {
		fmt.Fprintf(&b, ":\n\n%s", fenced(reason))
	} else {
		b.WriteString(".")
	}
	fmt.Fprintf(&b, "\n\n%s\n\n</details>\n", storeMarker(store, sf))
	return b.String()
}

// footerPattern is the footer revkit ends every issue body with: the
// finding's ID, its store's and the hash of its full title, as a paragraph
// of its own, so a quote of it (a blockquote, or a line continuing one) does
// not match.
var footerPattern = regexp.MustCompile(`(?:^|\n\n)Finding (F[0-9]+) of findings store ([0-9a-f]{16}) \(title ([0-9a-f]{24})\)\.\n\n</details>$`)

// titleHash identifies a finding's full title, which the issue title may
// show only capped.
func titleHash(title string) string {
	sum := sha256.Sum256([]byte(issueTitleKey(oneLine(title))))
	return fmt.Sprintf("%x", sum[:12])
}

// footer returns the finding ID, store ID and title hash of the footer an
// issue revkit filed ends with, or empty strings for any other issue.
func footer(issue forgeIssue) (id, store, hash string) {
	if m := footerPattern.FindStringSubmatch(strings.TrimSpace(strings.ReplaceAll(issue.Body, "\r\n", "\n"))); m != nil {
		return m[1], m[2], m[3]
	}
	return "", "", ""
}

// ownIssue keys the issue revkit filed for a finding: its ID, its store's
// and the hash of its full title.
func ownIssue(id, store, hash string) string {
	return id + " " + store + " " + hash
}

// filedIssues indexes the issues revkit filed by the full-title hash in
// their footer, so any finding with that full title links the issue, even
// one whose title the cap shortened, and by the finding they were filed
// for, so a retry after a lost create response links that finding's own
// issue first, even under --new.
func filedIssues(open []forgeIssue) (byHash, byFinding map[string]string) {
	byHash, byFinding = map[string]string{}, map[string]string{}
	for _, issue := range open {
		id, store, hash := footer(issue)
		if hash == "" {
			continue
		}
		if byHash[hash] == "" {
			byHash[hash] = issue.HTMLURL
		}
		if key := ownIssue(id, store, hash); byFinding[key] == "" {
			byFinding[key] = issue.HTMLURL
		}
	}
	return byHash, byFinding
}

// storeMarker ends every issue revkit files; see footerPattern.
func storeMarker(store string, sf storedFinding) string {
	return fmt.Sprintf("Finding %s of findings store %s (title %s).", sf.ID, store, titleHash(sf.Title))
}

// errNotFileable marks a finding the filing rules no longer allow.
var errNotFileable = errors.New("can no longer be filed")

// errUnchanged leaves the store as it was after a read under its lock.
var errUnchanged = fmt.Errorf("unchanged")

// fileFindings files the given findings as issues, or links them to open
// issues with the same title, and marks each one filed as soon as its issue
// exists. It refuses the whole request if any finding may not be filed or
// there are more than the tracker's limit. The store is locked only to pick
// the findings and to mark each one, not while the forge is called. With
// dryRun it prints what it would file or link; with fresh it files a new
// issue for each finding and links only the one filed for it.
func fileFindings(ctx context.Context, out io.Writer, path string, repo *repository, ids []string, dryRun, fresh bool) error {
	tracker := repo.Issues
	if tracker == nil {
		return fmt.Errorf("repository %s has no issue tracker configured", repo.Name)
	}
	if len(ids) > tracker.limit() {
		return fmt.Errorf("%d findings asked, but one command files at most %d", len(ids), tracker.limit())
	}
	// One filing command at a time per store, so two cannot both pick a
	// finding and file it twice.
	unlock, err := lockFiling(path)
	if err != nil {
		return err
	}
	defer unlock()
	var chosen []storedFinding
	var store string
	err = withStore(path, func(s *findingStore) error {
		store = s.Store
		for _, id := range ids {
			sf := s.find(id)
			if sf == nil {
				return fmt.Errorf("%s has no finding %s", repo.Name, id)
			}
			if reason := fileable(*sf); reason != "" {
				return fmt.Errorf("%s cannot be filed: %s", sf.ID, reason)
			}
			for _, earlier := range chosen {
				if earlier.ID == sf.ID {
					return fmt.Errorf("%s is listed twice", sf.ID)
				}
			}
			chosen = append(chosen, *sf)
		}
		return errUnchanged
	})
	if err != errUnchanged {
		return err
	}

	f, err := newForge(tracker)
	if err != nil {
		return err
	}
	open, err := f.openIssues(ctx)
	if err != nil {
		return err
	}
	// Issues revkit filed count only under the full titles their footers
	// name, since their own titles may be capped; other open issues count
	// under their titles. With fresh, only a finding's own issue links:
	// each finding gets an issue of its own, after reopen untied it from
	// one that did not describe it.
	filed, own := filedIssues(open)
	byTitle := map[string]string{}
	if !fresh {
		for _, issue := range open {
			if _, _, hash := footer(issue); hash == "" {
				byTitle[issueTitleKey(issue.Title)] = issue.HTMLURL
			}
		}
		for _, sf := range chosen {
			key := issueTitleKey(oneLine(sf.Title))
			if issue := filed[titleHash(sf.Title)]; issue != "" && byTitle[key] == "" {
				byTitle[key] = issue
			}
		}
	}
	for _, sf := range chosen {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("stopped before %s: %w", sf.ID, err)
		}
		title := issueTitle(sf)
		// Only the full title links: two findings whose titles the cap
		// shortens to the same text stay two issues.
		key := issueTitleKey(oneLine(sf.Title))
		issueURL, mine := own[ownIssue(sf.ID, store, titleHash(sf.Title))]
		linked := mine
		if !mine {
			issueURL, linked = byTitle[key]
		}
		switch {
		case dryRun && mine:
			fmt.Fprintf(out, "would link %s to %s, the open issue revkit filed for it\n", sf.ID, issueURL)
			continue
		case dryRun && linked:
			fmt.Fprintf(out, "would link %s to %s, which has the same title\n", sf.ID, issueURL)
			continue
		case dryRun:
			fmt.Fprintf(out, "would file %s in %s/%s: %s\n\n%s\n", sf.ID, tracker.Owner, tracker.Repo, title, issueBody(store, sf))
			if !fresh {
				byTitle[key] = "the issue filed for " + sf.ID
			}
			continue
		case !linked:
			// The store may have changed since the findings were picked:
			// a dismissal, or a run that flagged it security-relevant.
			if err := stillFileable(path, sf.ID); err != nil {
				return err
			}
			issue, err := f.createIssue(ctx, title, issueBody(store, sf))
			if err != nil {
				return fmt.Errorf("%s: %w", sf.ID, err)
			}
			issueURL = issue.HTMLURL
			// A later finding of this command with the same title links it.
			if !fresh {
				byTitle[key] = issueURL
			}
		}
		if err := markFiled(path, sf.ID, issueURL); err != nil {
			if linked {
				return fmt.Errorf("%s was not linked to %s: %w", sf.ID, issueURL, err)
			}
			if errors.Is(err, errNotFileable) {
				// The finding changed while its issue was being created.
				return fmt.Errorf("%s was filed as %s, but changed meanwhile, so the store does not mark it filed; review that issue: %w", sf.ID, issueURL, err)
			}
			return fmt.Errorf("%s was filed as %s, but the store was not updated: %w", sf.ID, issueURL, err)
		}
		switch {
		case mine:
			fmt.Fprintf(out, "%s: revkit already filed it as %s, linked\n", sf.ID, issueURL)
		case linked:
			fmt.Fprintf(out, "%s: an open issue already has this title, linked %s\n", sf.ID, issueURL)
		default:
			fmt.Fprintf(out, "%s: filed %s\n", sf.ID, issueURL)
		}
	}
	return nil
}

// stillFileable checks a finding against the filing rules as the store is
// now.
func stillFileable(path, id string) error {
	err := withStore(path, func(s *findingStore) error {
		sf := s.find(id)
		if sf == nil {
			return fmt.Errorf("no finding %s", id)
		}
		if reason := fileable(*sf); reason != "" {
			return fmt.Errorf("%s can no longer be filed: %s", id, reason)
		}
		return errUnchanged
	})
	if err == errUnchanged {
		return nil
	}
	return err
}

// lockFiling takes the store's filing lock without waiting, and returns its
// release.
func lockFiling(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(path+".filing", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("another revkit findings file is running for this repository")
	}
	return func() {
		syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		lock.Close()
	}, nil
}

// markFiled records that a finding is filed as the issue at issueURL, if
// the rules still allow it: a finding dismissed or flagged meanwhile keeps
// that status even when an open issue already had its title.
func markFiled(path, id, issueURL string) error {
	return withStore(path, func(s *findingStore) error {
		sf := s.find(id)
		if sf == nil {
			return fmt.Errorf("no finding %s", id)
		}
		if reason := fileable(*sf); reason != "" {
			return fmt.Errorf("%w: %s %s", errNotFileable, id, reason)
		}
		sf.Status, sf.Issue = storeFiled, issueURL
		return nil
	})
}
