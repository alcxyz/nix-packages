package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The adapter commands own every query against T3's state database; deck
// runs them and parses their JSON.

type PR struct {
	URL   string `json:"url"`
	State string `json:"state"`
}

type WaitingOn struct {
	Kind        string `json:"kind"`
	Description string `json:"description"`
}

// InventoryThread is the part of a t3-thread-inventory row deck reads. The
// raw row is kept as well and passed on to the group command unchanged.
type InventoryThread struct {
	ThreadID     string      `json:"threadId"`
	Project      string      `json:"project"`
	Title        string      `json:"title"`
	Bucket       string      `json:"bucket"`
	SettledAt    string      `json:"settledAt"`
	Status       string      `json:"status"`
	SnoozedUntil string      `json:"snoozedUntil"`
	PRs          []PR        `json:"prs"`
	WaitingOn    []WaitingOn `json:"waitingOn"`
	CreatedAt    string      `json:"createdAt"`
	UpdatedAt    string      `json:"updatedAt"`
	Acknowledged bool        `json:"acknowledged"`

	raw map[string]json.RawMessage
}

type Inventory struct {
	Database      string            `json:"database"`
	WindowDays    int               `json:"windowDays"`
	EnvironmentID string            `json:"environmentId"`
	Threads       []InventoryThread `json:"threads"`
}

func (t InventoryThread) settled() bool {
	return t.Bucket == "recently-settled"
}

// Settle is a thread's Settle verdict from t3-thread-status.
type Settle struct {
	Verdict          string          `json:"verdict"`
	Line             string          `json:"line"`
	Owner            string          `json:"owner"`
	Stale            bool            `json:"stale"`
	At               string          `json:"at"`
	LeftForYou       json.RawMessage `json:"leftForYou"`
	LeftForYouUnread json.RawMessage `json:"leftForYouUnread"`
}

type Check struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail"`
}

type StatusPR struct {
	URL        string `json:"url"`
	Number     int    `json:"number"`
	State      string `json:"state"`
	BaseBranch string `json:"baseBranch"`
	Watched    bool   `json:"watched"`
}

type StatusThread struct {
	ThreadID     string     `json:"threadId"`
	State        string     `json:"state"`
	Verdict      string     `json:"verdict"`
	StallReasons []string   `json:"stallReasons"`
	Settle       *Settle    `json:"settle"`
	IdleMinutes  *float64   `json:"idleMinutes"`
	IdleSince    string     `json:"idleSince"`
	LastUserAt   string     `json:"lastUserAt"`
	Checks       []Check    `json:"checks"`
	PRs          []StatusPR `json:"prs"`
}

type Status struct {
	Database    string         `json:"database"`
	GeneratedAt string         `json:"generatedAt"`
	Threads     []StatusThread `json:"threads"`
}

type Message struct {
	Position int    `json:"position"`
	Role     string `json:"role"`
	Kind     string `json:"kind"`
	At       string `json:"at"`
	Text     string `json:"text"`
}

type LatestMessage struct {
	ID string `json:"id"`
	At string `json:"at"`
}

// Messages is t3-thread-messages output. Revision changes whenever any
// message of the thread finishes or changes, and LastActivityAt is when that
// last happened; Latest is only the newest message by creation time.
type Messages struct {
	ThreadID       string         `json:"threadId"`
	MessageCount   int            `json:"messageCount"`
	Revision       string         `json:"revision"`
	LastActivityAt string         `json:"lastActivityAt"`
	Latest         *LatestMessage `json:"latest"`
	Messages       []Message      `json:"messages"`
}

type GroupSummary struct {
	Unsettled   int `json:"unsettled"`
	User        int `json:"user"`
	Agent       int `json:"agent"`
	Stalled     int `json:"stalled"`
	Settle      int `json:"settle"`
	SettledOpen int `json:"settledOpen"`
	// UnwatchedChecks counts unsettled threads that stopped while a linked
	// open PR's checks run with no PR watch armed; absent means 0.
	UnwatchedChecks int            `json:"unwatchedChecks"`
	AvgPct          *float64       `json:"avgPct"`
	Phases          map[string]int `json:"phases"`
}

type GroupFacts struct {
	Verdict      string   `json:"verdict"`
	StallReasons []string `json:"stallReasons"`
	Settle       *Settle  `json:"settle"`
}

// GroupRow is a report row as the group command returns it: the inventory
// row, its summary fields and the grouping.
type GroupRow struct {
	InventoryThread
	SummaryFields
	Group string     `json:"group"`
	Owner string     `json:"owner"`
	Facts GroupFacts `json:"facts"`
	// UnwatchedChecks lists the URLs of open PRs whose checks are pending
	// while the thread has stopped with no PR watch armed. It is
	// informational and absent or null in older output.
	UnwatchedChecks []string `json:"unwatchedChecks"`
}

type GroupSection struct {
	Key     string     `json:"key"`
	Title   string     `json:"title"`
	Threads []GroupRow `json:"threads"`
}

type Grouped struct {
	Summary  GroupSummary   `json:"summary"`
	Sections []GroupSection `json:"sections"`
}

func (t *InventoryThread) UnmarshalJSON(data []byte) error {
	type plain InventoryThread
	var p plain
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*t = InventoryThread(p)
	t.raw = raw
	return nil
}

// GroupRow embeds two structs with their own fields; decode each from the
// same object so neither hides the other.
func (r *GroupRow) UnmarshalJSON(data []byte) error {
	if err := json.Unmarshal(data, &r.InventoryThread); err != nil {
		return err
	}
	if err := json.Unmarshal(data, &r.SummaryFields); err != nil {
		return err
	}
	var rest struct {
		Group           string     `json:"group"`
		Owner           string     `json:"owner"`
		Facts           GroupFacts `json:"facts"`
		UnwatchedChecks []string   `json:"unwatchedChecks"`
	}
	if err := json.Unmarshal(data, &rest); err != nil {
		return err
	}
	r.Group, r.Owner, r.Facts = rest.Group, rest.Owner, rest.Facts
	r.UnwatchedChecks = rest.UnwatchedChecks
	return nil
}

func parseInventory(data []byte) (Inventory, error) {
	var inv Inventory
	if err := json.Unmarshal(data, &inv); err != nil {
		return Inventory{}, fmt.Errorf("parse inventory: %w", err)
	}
	for i, t := range inv.Threads {
		if t.ThreadID == "" {
			return Inventory{}, fmt.Errorf("parse inventory: thread %d has no threadId", i+1)
		}
		if !validThreadID(t.ThreadID) {
			return Inventory{}, fmt.Errorf("parse inventory: thread %d has an invalid threadId", i+1)
		}
	}
	return inv, nil
}

func parseStatus(data []byte) (Status, error) {
	var st Status
	if err := json.Unmarshal(data, &st); err != nil {
		return Status{}, fmt.Errorf("parse status: %w", err)
	}
	return st, nil
}

func parseMessages(data []byte) (Messages, error) {
	var m Messages
	if err := json.Unmarshal(data, &m); err != nil {
		return Messages{}, fmt.Errorf("parse messages: %w", err)
	}
	return m, nil
}

func parseGrouped(data []byte) (Grouped, error) {
	var g Grouped
	if err := json.Unmarshal(data, &g); err != nil {
		return Grouped{}, fmt.Errorf("parse group output: %w", err)
	}
	for _, s := range g.Sections {
		if s.Key == "" {
			return Grouped{}, errors.New("parse group output: section without a key")
		}
	}
	return g, nil
}

var (
	threadIDRE  = regexp.MustCompile(`^[A-Za-z0-9:%._-]+$`)
	settledAtRE = regexp.MustCompile(`^[0-9][0-9T:.Z-]*$`)
)

// validThreadID applies the commands' ID pattern and refuses a leading dash,
// so an ID can never be read as an option.
func validThreadID(id string) bool {
	return threadIDRE.MatchString(id) && !strings.HasPrefix(id, "-")
}

// ackArgs builds the acknowledge command's arguments, one argv entry per
// thread, after checking each part against the command's own patterns.
func ackArgs(ids, settledAts []string) ([]string, error) {
	if len(ids) == 0 || len(ids) != len(settledAts) {
		return nil, errors.New("ack needs one settledAt per thread")
	}
	args := make([]string, 0, len(ids))
	for i, id := range ids {
		if !validThreadID(id) {
			return nil, fmt.Errorf("invalid thread ID %q", id)
		}
		if !settledAtRE.MatchString(settledAts[i]) {
			return nil, fmt.Errorf("invalid settledAt %q", settledAts[i])
		}
		args = append(args, id+"@"+settledAts[i])
	}
	return args, nil
}

func forgetArgs(ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, errors.New("forget needs a thread ID")
	}
	args := []string{"--forget"}
	for _, id := range ids {
		if !validThreadID(id) {
			return nil, fmt.Errorf("invalid thread ID %q", id)
		}
		args = append(args, id)
	}
	return args, nil
}

// Commands runs the adapter commands.
type Commands struct {
	cfg     CommandsConfig
	timeout time.Duration
}

func (c Commands) run(parent context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, c.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := runGroup(cmd); err != nil {
		// The caller's deadline or shutdown stopped the command, not its
		// own timeout.
		if parent.Err() != nil {
			return nil, fmt.Errorf("%s stopped: %w", name, parent.Err())
		}
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("%s timed out after %s", name, c.timeout)
		}
		return nil, fmt.Errorf("%s: %w%s", name, err, stderrHint(stderr.Bytes()))
	}
	return stdout.Bytes(), nil
}

func (c Commands) inventory(ctx context.Context, in InstanceConfig, days int) (Inventory, error) {
	out, err := c.run(ctx, c.cfg.Inventory, "--days", strconv.Itoa(days), "--db", in.DB)
	if err != nil {
		return Inventory{}, err
	}
	return parseInventory(out)
}

// status also returns the command's output, which the group command reads.
func (c Commands) status(ctx context.Context, in InstanceConfig, ids []string) (Status, []byte, error) {
	if len(ids) == 0 {
		return Status{}, []byte(`{"threads":[]}`), nil
	}
	args := []string{"--db", in.DB}
	for _, id := range ids {
		if !validThreadID(id) {
			return Status{}, nil, fmt.Errorf("invalid thread ID %q", id)
		}
	}
	out, err := c.run(ctx, c.cfg.Status, append(args, ids...)...)
	if err != nil {
		return Status{}, nil, err
	}
	st, err := parseStatus(out)
	if err != nil {
		return Status{}, nil, err
	}
	return st, out, nil
}

func (c Commands) messages(ctx context.Context, in InstanceConfig, id string, head, tail, maxChars int) (Messages, error) {
	if !validThreadID(id) {
		return Messages{}, fmt.Errorf("invalid thread ID %q", id)
	}
	out, err := c.run(ctx, c.cfg.Messages, "--db", in.DB,
		"--head", strconv.Itoa(head), "--tail", strconv.Itoa(tail),
		"--max-chars", strconv.Itoa(maxChars), id)
	if err != nil {
		return Messages{}, err
	}
	m, err := parseMessages(out)
	if err != nil {
		return Messages{}, err
	}
	if m.ThreadID != "" && m.ThreadID != id {
		return Messages{}, fmt.Errorf("messages for %s came back for another thread", id)
	}
	return m, nil
}

// group writes the report and status to private temporary files and runs
// the group command on them.
func (c Commands) group(ctx context.Context, report []byte, status []byte) (Grouped, error) {
	dir, err := os.MkdirTemp("", "deck-group-*")
	if err != nil {
		return Grouped{}, err
	}
	defer os.RemoveAll(dir)
	reportPath := dir + "/report.json"
	statusPath := dir + "/status.json"
	if err := os.WriteFile(reportPath, report, 0o600); err != nil {
		return Grouped{}, err
	}
	if err := os.WriteFile(statusPath, status, 0o600); err != nil {
		return Grouped{}, err
	}
	out, err := c.run(ctx, c.cfg.Group, reportPath, statusPath)
	if err != nil {
		return Grouped{}, err
	}
	return parseGrouped(out)
}

func (c Commands) ack(ctx context.Context, args []string) error {
	_, err := c.run(ctx, c.cfg.Ack, args...)
	return err
}

// stderrHint keeps the first line of a command's error output, shortened, so
// errors stay readable without carrying thread text along.
func stderrHint(stderr []byte) string {
	line := strings.TrimSpace(string(stderr))
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	if line == "" {
		return ""
	}
	if len(line) > 200 {
		line = line[:200] + "..."
	}
	return ": " + line
}
