package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeForge is a Forgejo issue API whose open issues are "Known" and, if
// set, filler issues listed before it, served 50 to a page.
type fakeForge struct {
	mu        sync.Mutex
	created   []map[string]string
	auth      []string
	failAfter int
	filler    int
	posts     int
	pageSize  int
	extra     []forgeIssue
	onList    func()
	onPost    func()
}

func (f *fakeForge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	if r.URL.Path != "/api/v1/repos/owner/repo/issues" {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		var open []forgeIssue
		for i := 0; i < f.filler; i++ {
			open = append(open, forgeIssue{Number: 1000 + i, Title: fmt.Sprint("filler ", i)})
		}
		open = append(open, forgeIssue{Number: 7, Title: "Known ", HTMLURL: "https://forge.example/owner/repo/issues/7"})
		open = append(open, f.extra...)
		page, size := 1, 50
		if f.pageSize > 0 {
			size = f.pageSize
		}
		fmt.Sscan(r.URL.Query().Get("page"), &page)
		start, end := (page-1)*size, page*size
		if f.onList != nil {
			f.mu.Unlock()
			f.onList()
			f.mu.Lock()
		}
		if start > len(open) {
			start = len(open)
		}
		if end > len(open) {
			end = len(open)
		}
		json.NewEncoder(w).Encode(open[start:end])
	case http.MethodPost:
		f.posts++
		if f.onPost != nil {
			f.onPost()
		}
		if f.failAfter > 0 && len(f.created) >= f.failAfter {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"message": "broken"}`)
			return
		}
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		f.created = append(f.created, body)
		number := 10 + len(f.created)
		json.NewEncoder(w).Encode(forgeIssue{Number: number, Title: body["title"], HTMLURL: fmt.Sprintf("https://forge.example/owner/repo/issues/%d", number)})
	}
}

func fileFixture(t *testing.T, failAfter int) (*fakeForge, *repository, string) {
	t.Helper()
	forge := &fakeForge{failAfter: failAfter}
	server := httptest.NewTLSServer(forge)
	t.Cleanup(server.Close)
	saved := forgeTransport
	forgeTransport = server.Client().Transport
	t.Cleanup(func() { forgeTransport = saved })

	dir := t.TempDir()
	token := filepath.Join(dir, "token")
	os.WriteFile(token, []byte("secret-token\n"), 0o600)
	repo := &repository{Name: "example", Issues: &issueTracker{URL: server.URL, Owner: "owner", Repo: "repo", TokenFile: token, Limit: 3}}
	path := filepath.Join(dir, "findings.json")
	err := withStore(path, func(s *findingStore) error {
		add := func(title, verdict string, security bool) {
			s.add(finding{Lens: "docs-drift", Title: title, Claim: "The README names a removed file.", Evidence: "README.md line 1",
				Suggestion: "Drop the line.", Paths: []string{"README.md:1"}, Reviewer: "opus", Verdict: verdict,
				Reason: "README.md:1 names it.", Security: security}, sighting{Commit: "0123456789abcdef", Run: "r1"})
		}
		add("Stale note", "confirmed", false) // F1
		add("Half true", "partly", false)     // F2
		add("Leaked host", "confirmed", true) // F3
		add("Dismissed one", "confirmed", false)
		s.find("F4").Status = storeDismissed
		add("known", "confirmed", false)              // F5, matches the open issue
		add("Another stale note", "confirmed", false) // F6
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return forge, repo, path
}

func TestFileRefusesWhatMayNotBeFiled(t *testing.T) {
	forge, repo, path := fileFixture(t, 0)
	for ids, want := range map[string]string{
		"F2":          "only confirmed",
		"F3":          "security-relevant",
		"F4":          "dismissed",
		"F9":          "no finding",
		"F1 F1":       "listed twice",
		"F1 F5 F6 F2": "at most 3",
	} {
		err := fileFindings(context.Background(), &bytes.Buffer{}, path, repo, strings.Fields(ids), false, false)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("file %s: %v, want %q", ids, err, want)
		}
	}
	if len(forge.auth) != 0 {
		t.Fatalf("refused requests reached the forge: %d", len(forge.auth))
	}
	noTracker := &repository{Name: "example"}
	if err := fileFindings(context.Background(), &bytes.Buffer{}, path, noTracker, []string{"F1"}, false, false); err == nil {
		t.Fatal("filed without a tracker")
	}
}

func TestFileDryRunChangesNothing(t *testing.T) {
	forge, repo, path := fileFixture(t, 0)
	var out bytes.Buffer
	if err := fileFindings(context.Background(), &out, path, repo, []string{"F1"}, true, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "would file F1") || !strings.Contains(out.String(), "How this was found") {
		t.Fatalf("dry run printed:\n%s", out.String())
	}
	out.Reset()
	if err := fileFindings(context.Background(), &out, path, repo, []string{"F5"}, true, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "would link F5 to https://forge.example/owner/repo/issues/7") {
		t.Fatalf("dry run printed:\n%s", out.String())
	}
	s, _ := readStore(path)
	if s.find("F1").Status != storeOpen || s.find("F5").Status != storeOpen || forge.posts != 0 {
		t.Fatal("a dry run filed")
	}
}

func TestFileCreatesOrLinksIssues(t *testing.T) {
	forge, repo, path := fileFixture(t, 0)
	var out bytes.Buffer
	if err := fileFindings(context.Background(), &out, path, repo, []string{"F1", "F5"}, false, false); err != nil {
		t.Fatal(err)
	}
	if len(forge.created) != 1 || forge.created[0]["title"] != "Stale note" {
		t.Fatalf("created = %v", forge.created)
	}
	body := forge.created[0]["body"]
	for _, want := range []string{"```text\nThe README names a removed file.\n```", "`README.md:1`", "lens `docs-drift`", "`0123456789ab`", "```text\nREADME.md:1 names it.\n```", "Finding F1"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body lacks %q:\n%s", want, body)
		}
	}
	for _, header := range forge.auth {
		if header != "token secret-token" {
			t.Fatalf("Authorization = %q", header)
		}
	}
	if strings.Contains(out.String(), "secret-token") {
		t.Fatal("the token was printed")
	}
	s, _ := readStore(path)
	if f := s.find("F1"); f.Status != storeFiled || f.Issue != "https://forge.example/owner/repo/issues/11" {
		t.Fatalf("F1 = %+v", f)
	}
	// F5's title matches the open issue, so it is linked, not filed again.
	if f := s.find("F5"); f.Status != storeFiled || f.Issue != "https://forge.example/owner/repo/issues/7" {
		t.Fatalf("F5 = %+v", f)
	}
	if err := fileFindings(context.Background(), &bytes.Buffer{}, path, repo, []string{"F1"}, false, false); err == nil || !strings.Contains(err.Error(), "already filed") {
		t.Fatalf("filed twice: %v", err)
	}
}

func TestFileKeepsWhatWasFiledBeforeAFailure(t *testing.T) {
	forge, repo, path := fileFixture(t, 1)
	err := fileFindings(context.Background(), &bytes.Buffer{}, path, repo, []string{"F1", "F6"}, false, false)
	if err == nil || !strings.Contains(err.Error(), "F6") || !strings.Contains(err.Error(), "broken") {
		t.Fatalf("err = %v", err)
	}
	s, _ := readStore(path)
	if s.find("F1").Status != storeFiled || s.find("F6").Status != storeOpen || len(forge.created) != 1 {
		t.Fatalf("store = %+v", s.Findings)
	}
}

func TestFileNeedsTheTokenFile(t *testing.T) {
	_, repo, path := fileFixture(t, 0)
	repo.Issues.TokenFile = filepath.Join(t.TempDir(), "missing")
	if err := fileFindings(context.Background(), &bytes.Buffer{}, path, repo, []string{"F1"}, false, false); err == nil || !strings.Contains(err.Error(), "token file") {
		t.Fatalf("err = %v", err)
	}
}

func TestFileChecksEveryPageForDuplicates(t *testing.T) {
	forge, repo, path := fileFixture(t, 0)
	forge.filler = 120 // "Known" is on the third page
	if err := fileFindings(context.Background(), &bytes.Buffer{}, path, repo, []string{"F5"}, false, false); err != nil {
		t.Fatal(err)
	}
	if s, _ := readStore(path); s.find("F5").Issue != "https://forge.example/owner/repo/issues/7" || forge.posts != 0 {
		t.Fatalf("F5 = %+v, posts %d", s.find("F5"), forge.posts)
	}
}

func TestFileStopsWhenCancelled(t *testing.T) {
	forge, repo, path := fileFixture(t, 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := fileFindings(ctx, &bytes.Buffer{}, path, repo, []string{"F1"}, false, false); err == nil {
		t.Fatal("filed after cancellation")
	}
	if forge.posts != 0 {
		t.Fatalf("posts = %d", forge.posts)
	}
}

func TestFileLinksSameTitlesWithinACommand(t *testing.T) {
	forge, repo, path := fileFixture(t, 0)
	withStore(path, func(s *findingStore) error {
		s.add(finding{Lens: "other-lens", Title: "Stale note", Claim: "c", Reviewer: "b", Verdict: "confirmed"}, sighting{Run: "r1"})
		return nil
	})
	if err := fileFindings(context.Background(), &bytes.Buffer{}, path, repo, []string{"F1", "F7"}, false, false); err != nil {
		t.Fatal(err)
	}
	s, _ := readStore(path)
	if forge.posts != 1 || s.find("F7").Issue != s.find("F1").Issue {
		t.Fatalf("posts %d, F1 %s, F7 %s", forge.posts, s.find("F1").Issue, s.find("F7").Issue)
	}
}

func TestFileHoldsTheFilingLock(t *testing.T) {
	_, repo, path := fileFixture(t, 0)
	unlock, err := lockFiling(path)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err := fileFindings(context.Background(), &bytes.Buffer{}, path, repo, []string{"F1"}, false, false); err == nil || !strings.Contains(err.Error(), "another") {
		t.Fatalf("err = %v", err)
	}
}

func TestFileRechecksBeforeCreating(t *testing.T) {
	forge, repo, path := fileFixture(t, 0)
	// A run flags F6 security-relevant after the command picked it: the
	// fake forge's listing is the moment in between.
	forge.onList = func() {
		withStore(path, func(s *findingStore) error {
			s.find("F6").Security = true
			return nil
		})
	}
	err := fileFindings(context.Background(), &bytes.Buffer{}, path, repo, []string{"F6"}, false, false)
	if err == nil || !strings.Contains(err.Error(), "security") || forge.posts != 0 {
		t.Fatalf("err = %v, posts %d", err, forge.posts)
	}
}

func TestFileStopsWhenCancelledPartWay(t *testing.T) {
	forge, repo, path := fileFixture(t, 0)
	ctx, cancel := context.WithCancel(context.Background())
	forge.onPost = cancel
	err := fileFindings(ctx, &bytes.Buffer{}, path, repo, []string{"F1", "F6"}, false, false)
	// F1's request was under way, so its outcome may be lost (a retry links
	// it by its title or store marker); F6 is never filed.
	if err == nil || forge.posts != 1 {
		t.Fatalf("err = %v, posts %d", err, forge.posts)
	}
	if s, _ := readStore(path); s.find("F6").Status != storeOpen {
		t.Fatalf("F6 = %+v", s.find("F6"))
	}
}

func TestFilePagesUntilEmpty(t *testing.T) {
	forge, repo, path := fileFixture(t, 0)
	forge.filler, forge.pageSize = 45, 20 // a forge that caps pages at 20
	if err := fileFindings(context.Background(), &bytes.Buffer{}, path, repo, []string{"F5"}, false, false); err != nil {
		t.Fatal(err)
	}
	if forge.posts != 0 {
		t.Fatal("missed the issue on the third short page")
	}
}

func TestFileLinksTitlesWithBreaks(t *testing.T) {
	forge, repo, path := fileFixture(t, 0)
	withStore(path, func(s *findingStore) error {
		s.add(finding{Lens: "l", Title: "Handle # comments", Claim: "c", Reviewer: "a", Verdict: "confirmed"}, sighting{Run: "r1"})
		return nil
	})
	// The forge already has an issue titled like F7, breaks and all.
	forge.extra = []forgeIssue{{Number: 9, Title: "Handle # comments", HTMLURL: "https://forge.example/owner/repo/issues/9"}}
	if err := fileFindings(context.Background(), &bytes.Buffer{}, path, repo, []string{"F7"}, false, false); err != nil {
		t.Fatal(err)
	}
	if s, _ := readStore(path); s.find("F7").Issue != "https://forge.example/owner/repo/issues/9" || forge.posts != 0 {
		t.Fatalf("F7 = %+v, posts %d", s.find("F7"), forge.posts)
	}
}

func TestLinkingKeepsADismissalMadeMeanwhile(t *testing.T) {
	forge, repo, path := fileFixture(t, 0)
	forge.onList = func() {
		withStore(path, func(s *findingStore) error {
			s.find("F5").Status, s.find("F5").Note = storeDismissed, "meanwhile"
			return nil
		})
	}
	err := fileFindings(context.Background(), &bytes.Buffer{}, path, repo, []string{"F5"}, false, false)
	if err == nil || !strings.Contains(err.Error(), "was not linked") {
		t.Fatalf("err = %v", err)
	}
	if s, _ := readStore(path); s.find("F5").Status != storeDismissed {
		t.Fatalf("F5 = %+v", s.find("F5"))
	}
}

func TestFileLinksATruncatedTitleByItsFullText(t *testing.T) {
	forge, repo, path := fileFixture(t, 0)
	long := "Bang! " + strings.Repeat("x", maxTitle-6)
	withStore(path, func(s *findingStore) error {
		s.add(finding{Lens: "l", Title: long, Claim: "c", Reviewer: "a", Verdict: "confirmed"}, sighting{Run: "r1"})
		return nil
	})
	forge.extra = []forgeIssue{{Number: 9, Title: long, HTMLURL: "https://forge.example/owner/repo/issues/9"}}
	if err := fileFindings(context.Background(), &bytes.Buffer{}, path, repo, []string{"F7"}, false, false); err != nil {
		t.Fatal(err)
	}
	if forge.posts != 0 {
		t.Fatal("filed a duplicate of a title the cap shortened")
	}
}

func TestReviewerTextIsFenced(t *testing.T) {
	got := fenced("  \n    if ready:\r\n        run()\n``` and ````\n[[https://evil.example/p.png]] @owner #12\n\n")
	want := "`````text\n    if ready:\n        run()\n``` and ````\n[[https://evil.example/p.png]] @owner #12\n`````"
	if got != want {
		t.Fatalf("fenced = %q\nwant     %q", got, want)
	}
	sf := storedFinding{ID: "F1", Claim: "c", Evidence: "e", Suggestion: "s", Reason: "r", Paths: []string{"a`b"}, Lens: "l"}
	if body := issueBody("0123456789abcdef", sf); strings.Contains(body, "a`b") || !strings.Contains(body, "```text\nr\n```") {
		t.Fatalf("body:\n%s", body)
	}
}

func TestFindingBodyUsesLabelsAndKeepsUntrustedTextFenced(t *testing.T) {
	sf := storedFinding{
		ID: "F1", Title: "A finding", Lens: "lens",
		Claim:      "## Claim\n@owner #12 https://example.test/claim",
		Evidence:   "## Evidence\n```text\n@owner #13\n```",
		Suggestion: "## Suggestion\n@owner #14 https://example.test/fix",
		Reason:     "## Reason\n@owner #15",
		Paths:      []string{"src/a.go"},
	}
	body := issueBody("0123456789abcdef", sf)
	previous := -1
	for _, label := range []string{"Status", "Next", "Owner", "Why", "Plan", "Done when"} {
		position := strings.Index(body, "**"+label+":**")
		if position <= previous {
			t.Fatalf("opening label %q absent or out of order:\n%s", label, body)
		}
		previous = position
	}
	details := strings.Index(body, "<details>")
	if strings.Index(body, fenced(sf.Claim)) > details || strings.Index(body, fenced(sf.Evidence)) < details {
		t.Fatalf("claim must stay visible and evidence collapsed:\n%s", body)
	}
	for _, value := range []string{sf.Claim, sf.Evidence, sf.Suggestion, sf.Reason} {
		if !strings.Contains(body, fenced(value)) {
			t.Fatalf("reviewer text was changed or lost its fence: %q\n%s", value, body)
		}
	}
	id, store, hash := footer(forgeIssue{Body: body})
	if id != sf.ID || store != "0123456789abcdef" || hash != titleHash(sf.Title) {
		t.Fatalf("finding identity lost: %q %q %q", id, store, hash)
	}
}

func TestIssueTitlesBreakReferences(t *testing.T) {
	title := issueTitle(storedFinding{Title: "Ping @owner about #12, other/repo!7 and https://git.example/o/r/issues/3"})
	for _, bad := range []string{"@owner", "#12", "repo!7", "https://"} {
		if strings.Contains(title, bad) {
			t.Fatalf("title keeps %q: %q", bad, title)
		}
	}
	for _, long := range []storedFinding{{Title: strings.Repeat("x", 300)}, {Title: strings.Repeat("@#", 150)}} {
		if n := len([]rune(issueTitle(long))); n != maxTitle {
			t.Fatalf("title length = %d", n)
		}
	}
}

func TestFileKeepsFindingsWithTheSameCappedTitleApart(t *testing.T) {
	forge, repo, path := fileFixture(t, 0)
	prefix := strings.Repeat("x", maxTitle)
	withStore(path, func(s *findingStore) error {
		s.add(finding{Lens: "l", Title: prefix + " one", Claim: "c", Reviewer: "a", Verdict: "confirmed"}, sighting{Run: "r1"})
		s.add(finding{Lens: "l", Title: prefix + " two", Claim: "c", Reviewer: "a", Verdict: "confirmed"}, sighting{Run: "r1"})
		return nil
	})
	if err := fileFindings(context.Background(), &bytes.Buffer{}, path, repo, []string{"F7", "F8"}, false, false); err != nil {
		t.Fatal(err)
	}
	s, _ := readStore(path)
	if forge.posts != 2 || s.find("F7").Issue == s.find("F8").Issue {
		t.Fatalf("posts %d, F7 %s, F8 %s", forge.posts, s.find("F7").Issue, s.find("F8").Issue)
	}
}

func TestRetryLinksALongTitledIssueFiledBefore(t *testing.T) {
	forge, repo, path := fileFixture(t, 0)
	long := strings.Repeat("y", maxTitle+20)
	withStore(path, func(s *findingStore) error {
		s.add(finding{Lens: "l", Title: long, Claim: "c", Reviewer: "a", Verdict: "confirmed"}, sighting{Run: "r1"})
		return nil
	})
	// An earlier attempt created the issue but lost the response; another
	// issue quotes F7's marker in its text, which does not count.
	sf := storedFinding{ID: "F7", Title: long}
	store := storeOf(t, path)
	forge.extra = []forgeIssue{
		{Number: 8, Title: issueTitle(sf), Body: "```text\n" + storeMarker(store, sf) + "\n```\n\nmore", HTMLURL: "https://forge.example/owner/repo/issues/8"},
		{Number: 9, Title: issueTitle(sf), Body: "...\n\n" + storeMarker(store, sf) + "\n\n</details>\n", HTMLURL: "https://forge.example/owner/repo/issues/9"},
	}
	if err := fileFindings(context.Background(), &bytes.Buffer{}, path, repo, []string{"F7"}, false, false); err != nil {
		t.Fatal(err)
	}
	if s, _ := readStore(path); forge.posts != 0 || s.find("F7").Issue != "https://forge.example/owner/repo/issues/9" {
		t.Fatalf("posts %d, F7 %+v", forge.posts, s.find("F7"))
	}
}

func TestMarkersDifferByFullTitle(t *testing.T) {
	a := storedFinding{ID: "F1", Title: strings.Repeat("z", maxTitle) + " one"}
	b := storedFinding{ID: "F1", Title: strings.Repeat("z", maxTitle) + " two"}
	if storeMarker("0123456789abcdef", a) == storeMarker("0123456789abcdef", b) {
		t.Fatal("two F1s with different titles share a marker")
	}
}

func TestBodyCitesTheDescribingCommit(t *testing.T) {
	sf := storedFinding{ID: "F1", Lens: "l", DescribedBy: "r1",
		FirstSeen: sighting{Run: "r1", Commit: "1111111111111"}, LastSeen: sighting{Run: "r2", Commit: "2222222222222"}}
	if body := issueBody("0123456789abcdef", sf); !strings.Contains(body, "`111111111111`") {
		t.Fatalf("body:\n%s", body)
	}
	sf.DescribedBy = "r0"
	if body := issueBody("0123456789abcdef", sf); !strings.Contains(body, "at an earlier commit") {
		t.Fatalf("body:\n%s", body)
	}
}

func TestAnyFindingWithTheFullTitleLinksRevkitsIssue(t *testing.T) {
	forge, repo, path := fileFixture(t, 0)
	long := strings.Repeat("w", maxTitle+20)
	withStore(path, func(s *findingStore) error {
		s.add(finding{Lens: "l", Title: long, Claim: "c", Reviewer: "a", Verdict: "confirmed"}, sighting{Run: "r1"}) // F7
		s.add(finding{Lens: "m", Title: long, Claim: "c", Reviewer: "b", Verdict: "confirmed"}, sighting{Run: "r1"}) // F8
		return nil
	})
	if err := fileFindings(context.Background(), &bytes.Buffer{}, path, repo, []string{"F7"}, false, false); err != nil {
		t.Fatal(err)
	}
	// The forge now lists the issue filed for F7, under its capped title.
	forge.extra = []forgeIssue{{Number: 11, Title: forge.created[0]["title"], Body: forge.created[0]["body"], HTMLURL: "https://forge.example/owner/repo/issues/11"}}
	if err := fileFindings(context.Background(), &bytes.Buffer{}, path, repo, []string{"F8"}, false, false); err != nil {
		t.Fatal(err)
	}
	if s, _ := readStore(path); forge.posts != 1 || s.find("F8").Issue != "https://forge.example/owner/repo/issues/11" {
		t.Fatalf("posts %d, F8 %+v", forge.posts, s.find("F8"))
	}
}

func TestRevkitIssuesMatchOnlyByTheirFullTitle(t *testing.T) {
	forge, repo, path := fileFixture(t, 0)
	capped := strings.Repeat("v", maxTitle-1) + "…"
	withStore(path, func(s *findingStore) error {
		// Its full title is what another finding's capped title shows.
		s.add(finding{Lens: "l", Title: capped, Claim: "c", Reviewer: "a", Verdict: "confirmed"}, sighting{Run: "r1"})
		return nil
	})
	other := storedFinding{ID: "F1", Title: strings.Repeat("v", maxTitle+20)}
	forge.extra = []forgeIssue{{Number: 12, Title: issueTitle(other), Body: "...\n\n" + storeMarker("0123456789abcdef", other) + "\n\n</details>\n", HTMLURL: "https://forge.example/owner/repo/issues/12"}}
	if err := fileFindings(context.Background(), &bytes.Buffer{}, path, repo, []string{"F7"}, false, false); err != nil {
		t.Fatal(err)
	}
	if forge.posts != 1 {
		t.Fatal("linked a revkit issue by its capped title")
	}
}

func TestFileNewSkipsSomeoneElsesSameTitledIssue(t *testing.T) {
	forge, repo, path := fileFixture(t, 0)
	// F5's title matches the unrelated open issue "Known".
	if err := fileFindings(context.Background(), &bytes.Buffer{}, path, repo, []string{"F5"}, false, true); err != nil {
		t.Fatal(err)
	}
	if s, _ := readStore(path); forge.posts != 1 || s.find("F5").Issue == "https://forge.example/owner/repo/issues/7" {
		t.Fatalf("posts %d, F5 %+v", forge.posts, s.find("F5"))
	}
}

func TestFileNewLinksNothing(t *testing.T) {
	forge, repo, path := fileFixture(t, 0)
	if err := fileFindings(context.Background(), &bytes.Buffer{}, path, repo, []string{"F1"}, false, false); err != nil {
		t.Fatal(err)
	}
	// F7 shares F1's title; revkit's issue for F1 does not describe it.
	withStore(path, func(s *findingStore) error {
		s.add(finding{Lens: "other-lens", Title: "Stale note", Claim: "c", Reviewer: "b", Verdict: "confirmed"}, sighting{Run: "r1"})
		return nil
	})
	forge.extra = []forgeIssue{{Number: 11, Title: forge.created[0]["title"], Body: forge.created[0]["body"], HTMLURL: "https://forge.example/owner/repo/issues/11"}}
	if err := fileFindings(context.Background(), &bytes.Buffer{}, path, repo, []string{"F7"}, false, true); err != nil {
		t.Fatal(err)
	}
	if s, _ := readStore(path); forge.posts != 2 || s.find("F7").Issue == "https://forge.example/owner/repo/issues/11" {
		t.Fatalf("posts %d, F7 %+v", forge.posts, s.find("F7"))
	}
}

func TestDryRunNewPredictsNoLinks(t *testing.T) {
	_, repo, path := fileFixture(t, 0)
	withStore(path, func(s *findingStore) error {
		s.add(finding{Lens: "other-lens", Title: "Stale note", Claim: "c", Reviewer: "b", Verdict: "confirmed"}, sighting{Run: "r1"})
		return nil
	})
	var out bytes.Buffer
	if err := fileFindings(context.Background(), &out, path, repo, []string{"F1", "F7"}, true, true); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "would link") || strings.Count(out.String(), "would file") != 2 {
		t.Fatalf("dry run printed:\n%s", out.String())
	}
}

func TestQuotedFootersDoNotCount(t *testing.T) {
	forge, repo, path := fileFixture(t, 0)
	marker := storeMarker(storeOf(t, path), storedFinding{ID: "F1", Title: "Stale note"})
	forge.extra = []forgeIssue{
		{Number: 8, Title: "Quotes it", Body: "<details>\n\n> " + marker + "\n\n</details>", HTMLURL: "https://forge.example/owner/repo/issues/8"},
		{Number: 9, Title: "Continues a quote", Body: "<details>\n\n> as filed:\n" + marker + "\n\n</details>", HTMLURL: "https://forge.example/owner/repo/issues/9"},
	}
	if err := fileFindings(context.Background(), &bytes.Buffer{}, path, repo, []string{"F1"}, false, false); err != nil {
		t.Fatal(err)
	}
	if s, _ := readStore(path); forge.posts != 1 || !strings.HasSuffix(s.find("F1").Issue, "/11") {
		t.Fatalf("posts %d, F1 %+v", forge.posts, s.find("F1"))
	}
}

func TestRetryLinksTheFindingsOwnIssueFirst(t *testing.T) {
	forge, repo, path := fileFixture(t, 0)
	// F5 shares its title with the unrelated issue #7; an earlier --new
	// filed #11 for it but lost the response.
	s, _ := readStore(path)
	own := forgeIssue{Number: 11, Title: issueTitle(*s.find("F5")), Body: issueBody(s.Store, *s.find("F5")), HTMLURL: "https://forge.example/owner/repo/issues/11"}
	forge.extra = []forgeIssue{own}
	for _, fresh := range []bool{true, false} {
		var out bytes.Buffer
		if err := fileFindings(context.Background(), &out, path, repo, []string{"F5"}, true, fresh); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "would link F5 to "+own.HTMLURL+", the open issue revkit filed for it") {
			t.Fatalf("new %v, dry run printed:\n%s", fresh, out.String())
		}
	}
	if err := fileFindings(context.Background(), &bytes.Buffer{}, path, repo, []string{"F5"}, false, true); err != nil {
		t.Fatal(err)
	}
	if s, _ := readStore(path); forge.posts != 0 || s.find("F5").Issue != own.HTMLURL {
		t.Fatalf("posts %d, F5 %+v", forge.posts, s.find("F5"))
	}
}

// storeOf is the ID of the store at path.
func storeOf(t *testing.T, path string) string {
	t.Helper()
	s, err := readStore(path)
	if err != nil || s.Store == "" {
		t.Fatalf("store %+v: %v", s, err)
	}
	return s.Store
}

func TestAnotherStoresIssueIsNotAFindingsOwn(t *testing.T) {
	forge, repo, path := fileFixture(t, 0)
	// Another store, such as a renamed repository's, filed its own F1
	// with the same title.
	s, _ := readStore(path)
	other := forgeIssue{Number: 12, Title: "Stale note", Body: issueBody("fedcba9876543210", *s.find("F1")), HTMLURL: "https://forge.example/owner/repo/issues/12"}
	forge.extra = []forgeIssue{other}
	if err := fileFindings(context.Background(), &bytes.Buffer{}, path, repo, []string{"F1"}, false, true); err != nil {
		t.Fatal(err)
	}
	if s, _ := readStore(path); forge.posts != 1 || s.find("F1").Issue == other.HTMLURL {
		t.Fatalf("posts %d, F1 %+v", forge.posts, s.find("F1"))
	}
}

func TestAStoreKeepsItsID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "findings.json")
	// A store from 0.2 has no ID; reading it under the lock gives it one.
	os.WriteFile(path, []byte(`{"version": 1, "nextId": 1, "findings": []}`), 0o644)
	if err := withStore(path, func(*findingStore) error { return errUnchanged }); err != errUnchanged {
		t.Fatal(err)
	}
	id := storeOf(t, path)
	withStore(path, func(*findingStore) error { return nil })
	if storeOf(t, path) != id {
		t.Fatal("the store's ID changed")
	}
}
