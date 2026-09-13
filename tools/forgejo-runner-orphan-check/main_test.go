package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestParseContainersUsesOnlyNamesWithValidTaskIDs(t *testing.T) {
	input := strings.Join([]string{
		`{"ID":"abc","Names":"FORGEJO-ACTIONS-TASK-14519_WORKFLOW-deadbeef_JOB-build","Image":"must-not-be-consumed"}`,
		`{"ID":"def","Names":"ordinary-container"}`,
	}, "\n")

	got, err := parseContainers(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	want := []container{{ID: "abc", Name: "FORGEJO-ACTIONS-TASK-14519_WORKFLOW-deadbeef_JOB-build", TaskID: 14519}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestParseContainersRejectsMalformedTaskNames(t *testing.T) {
	for _, name := range []string{
		"FORGEJO-ACTIONS-TASK-nope_WORKFLOW-hash_JOB-test",
		"FORGEJO-ACTIONS-TASK-0_WORKFLOW-hash_JOB-test",
		"FORGEJO-ACTIONS-TASK-999999999999999999999999_WORKFLOW-hash_JOB-test",
		"FORGEJO-ACTIONS-TASK-123_JOB-test",
	} {
		t.Run(name, func(t *testing.T) {
			input := fmt.Sprintf(`{"ID":"abc","Names":%q}`, name)
			if _, err := parseContainers(strings.NewReader(input)); err == nil {
				t.Fatal("malformed task container was accepted")
			}
		})
	}
}

func TestConfigRejectsUnsafeRepositoryNameWithoutEchoingIt(t *testing.T) {
	bad := "owner/repo%2Fprivate"
	_, err := parseConfig([]string{
		"--docker-host", "unix:///run/docker.sock",
		"--container-label", "runner=test",
		"--forgejo-url", "https://forgejo.example",
		"--token-file", "/run/credential/token",
		"--repo", bad,
	}, ioDiscard{})
	if err == nil || strings.Contains(err.Error(), bad) {
		t.Fatalf("unsafe repository error was not sanitized: %v", err)
	}
}

func TestForgejoLookupPaginatesNewestFirst(t *testing.T) {
	var pages []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "token fixture-token" {
			t.Errorf("unexpected authorization header %q", got)
		}
		if r.URL.Path != "/api/v1/repos/acme/widgets/actions/tasks" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		page := r.URL.Query().Get("page")
		pages = append(pages, page)
		w.Header().Set("Content-Type", "application/json")
		switch page {
		case "1":
			fmt.Fprint(w, `{"workflow_runs":[{"id":300,"status":"running"},{"id":250,"status":"failure"}]}`)
		case "2":
			fmt.Fprint(w, `{"workflow_runs":[{"id":200,"status":"success"},{"id":145,"status":"skipped"}]}`)
		default:
			t.Errorf("unexpected page %q", page)
		}
	}))
	defer server.Close()

	base, _ := url.Parse(server.URL)
	client := forgejoClient{
		baseURL: base, token: "fixture-token", repos: []string{"acme/widgets"},
		pageSize: 2, maxPagesPerRepo: 5, client: server.Client(),
	}
	budget := 5
	got, err := client.lookup(context.Background(), map[int64]bool{250: true, 145: true}, &budget)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != "map[145:skipped 250:failure]" {
		t.Fatalf("unexpected tasks: %#v", got)
	}
	if strings.Join(pages, ",") != "1,2" || budget != 3 {
		t.Fatalf("pages=%v budget=%d", pages, budget)
	}
}

func TestForgejoLookupStopsAfterPassingMissingGlobalID(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch r.URL.Query().Get("page") {
		case "1":
			fmt.Fprint(w, `{"workflow_runs":[{"id":300,"status":"running"},{"id":250,"status":"failure"}]}`)
		case "2":
			fmt.Fprint(w, `{"workflow_runs":[{"id":200,"status":"success"},{"id":145,"status":"skipped"}]}`)
		default:
			t.Fatal("lookup did not stop after passing the wanted global ID")
		}
	}))
	defer server.Close()

	base, _ := url.Parse(server.URL)
	client := forgejoClient{baseURL: base, token: "token", repos: []string{"acme/widgets"}, pageSize: 2, maxPagesPerRepo: 10, client: server.Client()}
	budget := 10
	got, err := client.lookup(context.Background(), map[int64]bool{225: true}, &budget)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 || requests != 2 {
		t.Fatalf("got=%v requests=%d", got, requests)
	}
}

func TestForgejoLookupSanitizesFailuresAndRejectsMalformedOrder(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
	}{
		{name: "server error", status: http.StatusInternalServerError, body: `secret response body`},
		{name: "malformed order", status: http.StatusOK, body: `{"workflow_runs":[{"id":10,"status":"running"},{"id":11,"status":"success"}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				fmt.Fprint(w, test.body)
			}))
			defer server.Close()
			base, _ := url.Parse(server.URL)
			client := forgejoClient{baseURL: base, token: "token", repos: []string{"acme/widgets"}, pageSize: 2, maxPagesPerRepo: 1, client: server.Client()}
			budget := 2
			_, err := client.lookup(context.Background(), map[int64]bool{10: true}, &budget)
			if !errors.Is(err, errForgejo) || strings.Contains(err.Error(), "secret") {
				t.Fatalf("error was not sanitized: %v", err)
			}
		})
	}
}

func TestForgejoLookupRejectsRedirectWithoutForwardingToken(t *testing.T) {
	targetRequests := 0
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		targetRequests++
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, &http.Request{}, target.URL, http.StatusFound)
	}))
	defer source.Close()

	base, _ := url.Parse(source.URL)
	client := forgejoClient{baseURL: base, token: "fixture-token", repos: []string{"acme/widgets"}, pageSize: 1, maxPagesPerRepo: 1, client: newHTTPClient()}
	budget := 2
	_, err := client.lookup(context.Background(), map[int64]bool{1: true}, &budget)
	if !errors.Is(err, errForgejo) || targetRequests != 0 {
		t.Fatalf("err=%v redirected requests=%d", err, targetRequests)
	}
}

func TestExecuteConfirmsEveryTerminalStatus(t *testing.T) {
	containers := []container{
		{ID: "a", Name: "FORGEJO-ACTIONS-TASK-1_WORKFLOW-a_JOB-a", TaskID: 1},
		{ID: "b", Name: "FORGEJO-ACTIONS-TASK-2_WORKFLOW-b_JOB-b", TaskID: 2},
		{ID: "c", Name: "FORGEJO-ACTIONS-TASK-3_WORKFLOW-c_JOB-c", TaskID: 3},
		{ID: "d", Name: "FORGEJO-ACTIONS-TASK-4_WORKFLOW-d_JOB-d", TaskID: 4},
	}
	statuses := map[int64]string{1: "success", 2: "failure", 3: "cancelled", 4: "skipped"}
	docker := &fakeContainers{responses: [][]container{containers, containers}}
	lookup := &fakeLookup{responses: []map[int64]string{statuses, statuses}}
	stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
	exit := execute(context.Background(), testConfig(), testDependencies(docker, lookup), stdout, stderr)
	if exit != exitOrphans || lookup.calls != 2 || docker.calls != 2 {
		t.Fatalf("exit=%d lookup calls=%d docker calls=%d stderr=%q", exit, lookup.calls, docker.calls, stderr.String())
	}
	for id, status := range statuses {
		if !strings.Contains(stdout.String(), fmt.Sprintf("task=%d status=%s", id, status)) {
			t.Errorf("missing task %d status %s in %q", id, status, stdout.String())
		}
	}
}

func TestExecuteDoesNotConfirmNonterminalTask(t *testing.T) {
	for _, status := range []string{"running", "waiting", "blocked"} {
		t.Run(status, func(t *testing.T) {
			item := container{ID: "a", Name: "FORGEJO-ACTIONS-TASK-1_WORKFLOW-a_JOB-a", TaskID: 1}
			docker := &fakeContainers{responses: [][]container{{item}}}
			lookup := &fakeLookup{responses: []map[int64]string{{1: status}}}
			exit := execute(context.Background(), testConfig(), testDependencies(docker, lookup), ioDiscard{}, ioDiscard{})
			if exit != exitOK || docker.calls != 1 || lookup.calls != 1 {
				t.Fatalf("exit=%d docker=%d lookup=%d", exit, docker.calls, lookup.calls)
			}
		})
	}
}

func TestExecuteTreatsMissingAndAPIErrorsAsUnknown(t *testing.T) {
	item := container{ID: "a", Name: "FORGEJO-ACTIONS-TASK-1_WORKFLOW-a_JOB-a", TaskID: 1}
	for _, test := range []struct {
		name   string
		lookup *fakeLookup
	}{
		{name: "missing", lookup: &fakeLookup{responses: []map[int64]string{{}}}},
		{name: "api failure", lookup: &fakeLookup{errors: []error{errors.New("raw secret API failure")}}},
		{name: "unknown status", lookup: &fakeLookup{responses: []map[int64]string{{1: "unknown"}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			stderr := new(bytes.Buffer)
			exit := execute(context.Background(), testConfig(), testDependencies(&fakeContainers{responses: [][]container{{item}}}, test.lookup), ioDiscard{}, stderr)
			if exit != exitUnknown || strings.Contains(stderr.String(), "raw secret") {
				t.Fatalf("exit=%d stderr=%q", exit, stderr.String())
			}
		})
	}
}

func TestExecuteSkipsTokenAndAPIWithoutCandidateContainers(t *testing.T) {
	read := false
	lookup := &fakeLookup{}
	deps := testDependencies(&fakeContainers{responses: [][]container{{}}}, lookup)
	deps.readFile = func(string) ([]byte, error) {
		read = true
		return nil, errors.New("must not be called")
	}
	exit := execute(context.Background(), testConfig(), deps, ioDiscard{}, ioDiscard{})
	if exit != exitOK || read || lookup.calls != 0 {
		t.Fatalf("exit=%d read=%t lookup=%d", exit, read, lookup.calls)
	}
}

func TestExecuteRechecksContainerAndTask(t *testing.T) {
	item := container{ID: "a", Name: "FORGEJO-ACTIONS-TASK-1_WORKFLOW-a_JOB-a", TaskID: 1}
	for _, test := range []struct {
		name      string
		docker    *fakeContainers
		lookup    *fakeLookup
		wantExit  int
		wantCalls int
	}{
		{name: "container cleaned during grace", docker: &fakeContainers{responses: [][]container{{item}, {}}}, lookup: &fakeLookup{responses: []map[int64]string{{1: "success"}}}, wantExit: exitOK, wantCalls: 1},
		{name: "status no longer terminal", docker: &fakeContainers{responses: [][]container{{item}, {item}}}, lookup: &fakeLookup{responses: []map[int64]string{{1: "success"}, {1: "running"}}}, wantExit: exitUnknown, wantCalls: 2},
		{name: "confirmation API failure", docker: &fakeContainers{responses: [][]container{{item}, {item}}}, lookup: &fakeLookup{responses: []map[int64]string{{1: "success"}}, errors: []error{nil, errors.New("secret response")}}, wantExit: exitUnknown, wantCalls: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			stderr := new(bytes.Buffer)
			exit := execute(context.Background(), testConfig(), testDependencies(test.docker, test.lookup), ioDiscard{}, stderr)
			if exit != test.wantExit || test.lookup.calls != test.wantCalls || strings.Contains(stderr.String(), "secret") {
				t.Fatalf("exit=%d calls=%d stderr=%q", exit, test.lookup.calls, stderr.String())
			}
		})
	}
}

type fakeContainers struct {
	responses [][]container
	errors    []error
	calls     int
}

func (f *fakeContainers) list(context.Context) ([]container, error) {
	index := f.calls
	f.calls++
	if index < len(f.errors) && f.errors[index] != nil {
		return nil, f.errors[index]
	}
	if index >= len(f.responses) {
		return nil, errors.New("unexpected container call")
	}
	return f.responses[index], nil
}

type fakeLookup struct {
	responses []map[int64]string
	errors    []error
	calls     int
}

func (f *fakeLookup) lookup(_ context.Context, _ map[int64]bool, requestsLeft *int) (map[int64]string, error) {
	index := f.calls
	f.calls++
	*requestsLeft--
	if index < len(f.errors) && f.errors[index] != nil {
		return nil, f.errors[index]
	}
	if index >= len(f.responses) {
		return nil, errors.New("unexpected lookup call")
	}
	return f.responses[index], nil
}

func testConfig() config {
	return config{tokenFile: "token", maxRequests: 10, grace: 0}
}

func testDependencies(containers containerSource, lookup taskLookup) dependencies {
	return dependencies{
		containers: containers,
		newLookup:  func(string) taskLookup { return lookup },
		readFile:   func(string) ([]byte, error) { return []byte("fixture-token\n"), nil },
		sleep:      func(context.Context, time.Duration) error { return nil },
	}
}

type ioDiscard struct{}

func (ioDiscard) Write(data []byte) (int, error) { return len(data), nil }
