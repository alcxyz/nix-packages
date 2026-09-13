package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	exitOK      = 0
	exitOrphans = 1
	exitUnknown = 2
)

var (
	errDocker       = errors.New("Docker container scan failed")
	errForgejo      = errors.New("Forgejo task lookup failed")
	errRequestLimit = errors.New("Forgejo request budget exhausted")
	taskNamePattern = regexp.MustCompile(`^FORGEJO-ACTIONS-TASK-([1-9][0-9]*)_WORKFLOW-.+_JOB-.+$`)
	repoPartPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
)

type repoList []string

func (r *repoList) String() string { return strings.Join(*r, ",") }
func (r *repoList) Set(value string) error {
	*r = append(*r, value)
	return nil
}

type config struct {
	dockerHost      string
	containerLabel  string
	forgejoURL      string
	tokenFile       string
	repos           []string
	pageSize        int
	maxPagesPerRepo int
	maxRequests     int
	grace           time.Duration
	timeout         time.Duration
}

func parseConfig(args []string, stderr io.Writer) (config, error) {
	var cfg config
	var repos repoList
	flags := flag.NewFlagSet("forgejo-runner-orphan-check", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&cfg.dockerHost, "docker-host", "", "Docker daemon URL")
	flags.StringVar(&cfg.containerLabel, "container-label", "", "label used to select runner containers")
	flags.StringVar(&cfg.forgejoURL, "forgejo-url", "", "Forgejo base URL")
	flags.StringVar(&cfg.tokenFile, "token-file", "", "file containing a Forgejo API token")
	flags.Var(&repos, "repo", "repository in OWNER/REPO form; repeat for each repository")
	flags.IntVar(&cfg.pageSize, "page-size", 50, "Forgejo task page size")
	flags.IntVar(&cfg.maxPagesPerRepo, "max-pages-per-repo", 20, "maximum pages read from one repository per lookup")
	flags.IntVar(&cfg.maxRequests, "max-requests", 100, "total Forgejo request budget across both lookups")
	flags.DurationVar(&cfg.grace, "grace", 2*time.Second, "delay before confirming a possible orphan")
	flags.DurationVar(&cfg.timeout, "timeout", 30*time.Second, "overall scan timeout")
	if err := flags.Parse(args); err != nil {
		return cfg, err
	}
	if flags.NArg() != 0 {
		return cfg, errors.New("unexpected positional arguments")
	}
	cfg.repos = repos
	if cfg.dockerHost == "" || cfg.containerLabel == "" || cfg.forgejoURL == "" || cfg.tokenFile == "" || len(cfg.repos) == 0 {
		return cfg, errors.New("docker-host, container-label, forgejo-url, token-file, and at least one repo are required")
	}
	if cfg.pageSize < 1 || cfg.pageSize > 50 {
		return cfg, errors.New("page-size must be between 1 and 50")
	}
	if cfg.maxPagesPerRepo < 1 || cfg.maxPagesPerRepo > 1000 {
		return cfg, errors.New("max-pages-per-repo must be between 1 and 1000")
	}
	if cfg.maxRequests < 2 || cfg.maxRequests > 10000 {
		return cfg, errors.New("max-requests must be between 2 and 10000")
	}
	if cfg.grace < 0 || cfg.grace > time.Minute {
		return cfg, errors.New("grace must be between 0 and 1m")
	}
	if cfg.timeout <= 0 || cfg.timeout > 10*time.Minute {
		return cfg, errors.New("timeout must be between 1ns and 10m")
	}
	base, err := url.Parse(cfg.forgejoURL)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return cfg, errors.New("forgejo-url must be an http(s) base URL without credentials, query, or fragment")
	}
	for _, repo := range cfg.repos {
		parts := strings.Split(repo, "/")
		if len(parts) != 2 || !repoPartPattern.MatchString(parts[0]) || !repoPartPattern.MatchString(parts[1]) {
			return cfg, errors.New("invalid repo; expected OWNER/REPO using letters, numbers, dot, underscore, or hyphen")
		}
	}
	return cfg, nil
}

type container struct {
	ID     string
	Name   string
	TaskID int64
}

type containerSource interface {
	list(context.Context) ([]container, error)
}

type dockerSource struct {
	host  string
	label string
}

func (d dockerSource) list(ctx context.Context) ([]container, error) {
	cmd := exec.CommandContext(ctx, "docker", "--host", d.host, "ps", "--no-trunc", "--filter", "label="+d.label, "--format", `{"ID":{{json .ID}},"Names":{{json .Names}}}`)
	cmd.Stderr = io.Discard
	output, err := cmd.Output()
	if err != nil {
		return nil, errDocker
	}
	containers, err := parseContainers(bytes.NewReader(output))
	if err != nil {
		return nil, errDocker
	}
	return containers, nil
}

func parseContainers(input io.Reader) ([]container, error) {
	decoder := json.NewDecoder(input)
	var result []container
	seen := make(map[string]bool)
	for {
		var row struct {
			ID    string `json:"ID"`
			Names string `json:"Names"`
		}
		if err := decoder.Decode(&row); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		if row.ID == "" || row.Names == "" || seen[row.ID] {
			return nil, errors.New("invalid Docker output")
		}
		seen[row.ID] = true
		match := taskNamePattern.FindStringSubmatch(row.Names)
		if match == nil {
			if strings.HasPrefix(row.Names, "FORGEJO-ACTIONS-TASK-") {
				return nil, errors.New("malformed Forgejo Actions container name")
			}
			continue
		}
		taskID, err := strconv.ParseInt(match[1], 10, 64)
		if err != nil || taskID <= 0 {
			return nil, errors.New("invalid Forgejo Actions task ID")
		}
		result = append(result, container{ID: row.ID, Name: row.Names, TaskID: taskID})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].TaskID == result[j].TaskID {
			return result[i].ID < result[j].ID
		}
		return result[i].TaskID < result[j].TaskID
	})
	return result, nil
}

type taskLookup interface {
	lookup(context.Context, map[int64]bool, *int) (map[int64]string, error)
}

type forgejoClient struct {
	baseURL         *url.URL
	token           string
	repos           []string
	pageSize        int
	maxPagesPerRepo int
	client          *http.Client
}

type taskResponse struct {
	WorkflowRuns []struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
	} `json:"workflow_runs"`
}

func (f forgejoClient) lookup(ctx context.Context, wanted map[int64]bool, requestsLeft *int) (map[int64]string, error) {
	found := make(map[int64]string)
	for _, repo := range f.repos {
		unresolved := unresolvedIDs(wanted, found)
		if len(unresolved) == 0 {
			break
		}
		parts := strings.Split(repo, "/")
		for page := 1; page <= f.maxPagesPerRepo; page++ {
			if *requestsLeft <= 0 {
				return found, errRequestLimit
			}
			*requestsLeft--
			endpoint := *f.baseURL
			endpoint.Path = strings.TrimSuffix(endpoint.Path, "/") + "/api/v1/repos/" + parts[0] + "/" + parts[1] + "/actions/tasks"
			query := endpoint.Query()
			query.Set("page", strconv.Itoa(page))
			query.Set("limit", strconv.Itoa(f.pageSize))
			endpoint.RawQuery = query.Encode()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
			if err != nil {
				return found, errForgejo
			}
			req.Header.Set("Authorization", "token "+f.token)
			req.Header.Set("Accept", "application/json")
			req.Header.Set("User-Agent", "forgejo-runner-orphan-check")
			response, err := f.client.Do(req)
			if err != nil {
				return found, errForgejo
			}
			var body taskResponse
			bodyBytes, readErr := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
			response.Body.Close()
			if response.StatusCode != http.StatusOK || readErr != nil || len(bodyBytes) > 1<<20 || json.Unmarshal(bodyBytes, &body) != nil {
				return found, errForgejo
			}
			oldest, err := consumeTaskPage(body, wanted, found)
			if err != nil {
				return found, errForgejo
			}
			unresolved = unresolvedIDs(wanted, found)
			if len(unresolved) == 0 || len(body.WorkflowRuns) == 0 || len(body.WorkflowRuns) < f.pageSize || oldest < unresolved[0] {
				break
			}
		}
	}
	return found, nil
}

func consumeTaskPage(body taskResponse, wanted map[int64]bool, found map[int64]string) (int64, error) {
	var previous int64
	for index, task := range body.WorkflowRuns {
		if task.ID <= 0 || task.Status == "" || (index > 0 && task.ID >= previous) {
			return 0, errors.New("invalid task page")
		}
		previous = task.ID
		if wanted[task.ID] {
			found[task.ID] = task.Status
		}
	}
	return previous, nil
}

func unresolvedIDs(wanted map[int64]bool, found map[int64]string) []int64 {
	ids := make([]int64, 0, len(wanted))
	for id := range wanted {
		if _, ok := found[id]; !ok {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func terminal(status string) bool {
	switch status {
	case "success", "failure", "cancelled", "skipped":
		return true
	default:
		return false
	}
}

func recognizedNonterminal(status string) bool {
	switch status {
	case "running", "waiting", "blocked":
		return true
	default:
		return false
	}
}

type dependencies struct {
	containers containerSource
	newLookup  func(string) taskLookup
	readFile   func(string) ([]byte, error)
	sleep      func(context.Context, time.Duration) error
}

func execute(ctx context.Context, cfg config, deps dependencies, stdout, stderr io.Writer) int {
	initial, err := deps.containers.list(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "unknown: Docker container scan failed")
		return exitUnknown
	}
	if len(initial) == 0 {
		return exitOK
	}
	tokenData, err := deps.readFile(cfg.tokenFile)
	if err != nil || strings.TrimSpace(string(tokenData)) == "" {
		fmt.Fprintln(stderr, "unknown: Forgejo token file could not be read")
		return exitUnknown
	}
	lookup := deps.newLookup(strings.TrimSpace(string(tokenData)))
	wanted := taskIDs(initial)
	requestsLeft := cfg.maxRequests
	first, err := lookup.lookup(ctx, wanted, &requestsLeft)
	if err != nil {
		fmt.Fprintln(stderr, "unknown: Forgejo task lookup incomplete")
		return exitUnknown
	}
	possible := make(map[int64]bool)
	unknown := false
	for id := range wanted {
		status, ok := first[id]
		switch {
		case !ok:
			unknown = true
		case terminal(status):
			possible[id] = true
		case recognizedNonterminal(status):
		case status == "unknown":
			unknown = true
		default:
			unknown = true
		}
	}
	if len(possible) == 0 {
		if unknown {
			fmt.Fprintln(stderr, "unknown: one or more Forgejo tasks could not be resolved")
			return exitUnknown
		}
		return exitOK
	}
	if err := deps.sleep(ctx, cfg.grace); err != nil {
		fmt.Fprintln(stderr, "unknown: scan interrupted during confirmation grace")
		return exitUnknown
	}
	current, err := deps.containers.list(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "unknown: Docker confirmation scan failed")
		return exitUnknown
	}
	stillRunning := intersectContainers(initial, current, possible)
	if len(stillRunning) == 0 {
		if unknown {
			fmt.Fprintln(stderr, "unknown: one or more Forgejo tasks could not be resolved")
			return exitUnknown
		}
		return exitOK
	}
	confirmWanted := taskIDs(stillRunning)
	second, err := lookup.lookup(ctx, confirmWanted, &requestsLeft)
	if err != nil {
		fmt.Fprintln(stderr, "unknown: Forgejo confirmation lookup incomplete")
		return exitUnknown
	}
	confirmed := 0
	for _, item := range stillRunning {
		status, ok := second[item.TaskID]
		if !ok || !terminal(status) {
			unknown = true
			continue
		}
		fmt.Fprintf(stdout, "confirmed orphan: container=%s name=%s task=%d status=%s\n", item.ID, item.Name, item.TaskID, status)
		confirmed++
	}
	if unknown {
		fmt.Fprintln(stderr, "unknown: one or more Forgejo tasks could not be confirmed")
		return exitUnknown
	}
	if confirmed > 0 {
		return exitOrphans
	}
	return exitOK
}

func taskIDs(containers []container) map[int64]bool {
	result := make(map[int64]bool)
	for _, item := range containers {
		result[item.TaskID] = true
	}
	return result
}

func intersectContainers(initial, current []container, wanted map[int64]bool) []container {
	currentByID := make(map[string]container, len(current))
	for _, item := range current {
		currentByID[item.ID] = item
	}
	var result []container
	for _, item := range initial {
		now, ok := currentByID[item.ID]
		if ok && now.TaskID == item.TaskID && wanted[item.TaskID] {
			result = append(result, item)
		}
	}
	return result
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func newHTTPClient() *http.Client {
	return &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

func main() {
	cfg, err := parseConfig(os.Args[1:], os.Stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintf(os.Stderr, "configuration error: %v\n", err)
		os.Exit(exitUnknown)
	}
	baseURL, _ := url.Parse(cfg.forgejoURL)
	ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
	defer cancel()
	deps := dependencies{
		containers: dockerSource{host: cfg.dockerHost, label: cfg.containerLabel},
		newLookup: func(token string) taskLookup {
			return forgejoClient{
				baseURL: baseURL, token: token, repos: cfg.repos,
				pageSize: cfg.pageSize, maxPagesPerRepo: cfg.maxPagesPerRepo,
				client: newHTTPClient(),
			}
		},
		readFile: os.ReadFile,
		sleep:    sleepContext,
	}
	os.Exit(execute(ctx, cfg, deps, os.Stdout, os.Stderr))
}
