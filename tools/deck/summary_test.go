package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validSummary = `{"progressPct": 55, "phase": "implementing", "owner": "agent",
 "goal": "Build the widget", "done": ["a", "b"], "remaining": ["c"],
 "nextAction": "Agent writes tests", "actionKind": "other",
 "blocker": null, "flag": "  ", "verdict": null, "verdictReason": null}`

func TestParseSummaryValid(t *testing.T) {
	s, err := parseSummary([]byte(validSummary), false)
	if err != nil {
		t.Fatal(err)
	}
	if *s.ProgressPct != 55 || s.Phase != "implementing" || len(s.Done) != 2 || s.Flag != nil || s.Verdict != nil {
		t.Fatalf("parsed %+v", s)
	}
	fenced := "```json\n" + validSummary + "\n```"
	if _, err := parseSummary([]byte(fenced), false); err != nil {
		t.Fatalf("a fenced answer: %v", err)
	}
}

func TestParseSummaryClampsProgress(t *testing.T) {
	for in, want := range map[string]int{"150": 100, "-5": 0, "33.6": 34} {
		s, err := parseSummary([]byte(strings.Replace(validSummary, "55", in, 1)), false)
		if err != nil {
			t.Fatal(err)
		}
		if *s.ProgressPct != want {
			t.Errorf("progress %s: got %d, want %d", in, *s.ProgressPct, want)
		}
	}
}

func TestParseSummaryRejects(t *testing.T) {
	cases := map[string]string{
		"unknown key":     strings.Replace(validSummary, `"goal"`, `"extra": 1, "goal"`, 1),
		"bad phase":       strings.Replace(validSummary, `"implementing"`, `"coding"`, 1),
		"bad owner":       strings.Replace(validSummary, `"owner": "agent"`, `"owner": "both"`, 1),
		"bad action kind": strings.Replace(validSummary, `"actionKind": "other"`, `"actionKind": "x"`, 1),
		"too many done":   strings.Replace(validSummary, `["a", "b"]`, `["a","b","c","d","e"]`, 1),
		"too many left":   strings.Replace(validSummary, `["c"]`, `["a","b","c","d"]`, 1),
		"missing goal":    strings.Replace(validSummary, `"goal": "Build the widget",`, ``, 1),
		"empty goal":      strings.Replace(validSummary, `"Build the widget"`, `" "`, 1),
		"missing done":    strings.Replace(validSummary, `"done": ["a", "b"],`, ``, 1),
		"string progress": strings.Replace(validSummary, `55`, `"55"`, 1),
		"list as string":  strings.Replace(validSummary, `["c"]`, `"c"`, 1),
		"verdict":         strings.Replace(validSummary, `"verdict": null`, `"verdict": "done"`, 1),
		"trailing text":   validSummary + " and more",
		"two objects":     validSummary + validSummary,
		"closing garbage": validSummary + "]garbage",
		"closing brace":   validSummary + "}",
		"prose":           "Here is the summary: " + validSummary,
		"long text":       strings.Replace(validSummary, `"Build the widget"`, `"`+strings.Repeat("x", 600)+`"`, 1),
	}
	for name, in := range cases {
		if _, err := parseSummary([]byte(in), false); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestParseSummaryErrorsDoNotQuoteValues(t *testing.T) {
	in := strings.Replace(validSummary, `"implementing"`, `"SECRET-THREAD-TEXT"`, 1)
	_, err := parseSummary([]byte(in), false)
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("error must name the field only: %v", err)
	}
}

func TestParseSummarySettledVerdict(t *testing.T) {
	if _, err := parseSummary([]byte(validSummary), true); err == nil {
		t.Fatal("a settled thread needs a verdict")
	}
	in := strings.Replace(validSummary, `"verdict": null`, `"verdict": "follow-up"`, 1)
	s, err := parseSummary([]byte(in), true)
	if err != nil || *s.Verdict != "follow-up" {
		t.Fatalf("settled verdict: %+v, %v", s, err)
	}
	bad := strings.Replace(validSummary, `"verdict": null`, `"verdict": "maybe"`, 1)
	if _, err := parseSummary([]byte(bad), true); err == nil {
		t.Fatal("an unknown verdict was accepted")
	}
}

func TestSummaryCacheRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deck", "threads", "summaries.json")
	c, err := openSummaryCache(path)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := parseSummary([]byte(validSummary), false)
	e := CacheEntry{Summary: &s, Basis: Basis{Revision: "rev-1", Bucket: "unsettled"}, Model: "anthropic/light", MadeAt: time.Unix(100, 0).UTC(), AttemptBasis: Basis{Revision: "rev-1", Bucket: "unsettled"}}
	if err := c.put(cacheKey("main", "thread-a1"), e); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
	info, _ := os.Stat(filepath.Dir(path))
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("cache directory mode %v", info.Mode().Perm())
	}

	c2, err := openSummaryCache(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := c2.get("main/thread-a1")
	if !ok || got.Basis != e.Basis || got.Summary == nil || got.Summary.Goal != "Build the widget" || !got.MadeAt.Equal(e.MadeAt) {
		t.Fatalf("reloaded %+v", got)
	}

	if err := os.WriteFile(path, []byte(`{"version": 9}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openSummaryCache(path); err == nil {
		t.Fatal("an unknown cache version was accepted")
	}
}

func TestBuildPromptMarksUntrustedData(t *testing.T) {
	msgs := []Message{
		{Position: 1, Role: "user", Text: strings.Repeat("h", 1200)},
		{Position: 40, Role: "assistant", Text: strings.Repeat("t", 800)},
	}
	merged := mergeMessages(Messages{Messages: append(msgs, msgs[1])}, 3, 500)
	if len(merged) != 2 {
		t.Fatalf("duplicates kept: %d", len(merged))
	}
	if len([]rune(merged[0].Text)) != 1200 || len([]rune(merged[1].Text)) != 501 {
		t.Fatal("head messages keep their length, tail messages are cut to 500 plus a marker")
	}
	p, err := buildPrompt(promptFacts{
		Title: "TITLE-TEXT", Project: "P", SettleLine: "SETTLE-TEXT", OpenPRs: []string{},
		WaitingOn: []WaitingOn{{Kind: "user", Description: "WAITING-TEXT"}},
	}, merged)
	if err != nil {
		t.Fatal(err)
	}
	// Text written in the thread is data, not facts.
	begin := strings.Index(p, "BEGIN THREAD DATA")
	for _, text := range []string{"TITLE-TEXT", "SETTLE-TEXT", "WAITING-TEXT"} {
		if i := strings.Index(p, text); i < begin {
			t.Errorf("%s is outside the thread data", text)
		}
	}
	for _, want := range []string{"untrusted data", "BEGIN THREAD DATA", "END THREAD DATA", `"verdict": null`, "exactly one JSON object"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
}

func TestCLIArgs(t *testing.T) {
	c := strings.Join(claudeArgs("haiku", "low"), " ")
	for _, want := range []string{"-p --model haiku --effort low", "--safe-mode", "--restricted", "--tools  ", "--strict-mcp-config", "--no-session-persistence", "--disable-slash-commands"} {
		if !strings.Contains(c, want) {
			t.Errorf("claude args %q lack %q", c, want)
		}
	}
	if strings.Contains(strings.Join(claudeArgs("m", ""), " "), "--effort") {
		t.Error("an empty effort must keep the CLI default")
	}
}

func TestLoadRole(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	cfg := `[roles.light]
provider = "anthropic"
model = "haiku"
transport = "cli"
effort = "low"
[roles.light.backup]
provider = "openai"
model = "gpt-mini"
transport = "cli"
[roles.api]
model = "x"
transport = "api"
`
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := loadRole(path, "light")
	if err != nil {
		t.Fatal(err)
	}
	if r.label() != "anthropic/haiku" || r.Effort != "low" || r.Backup == nil || r.Backup.label() != "openai/gpt-mini" {
		t.Fatalf("role %+v", r)
	}
	// An api entry loads, as devlog accepts it, and is skipped for summaries.
	api, err := loadRole(path, "api")
	if err != nil {
		t.Fatal(err)
	}
	if models, skipped := summaryModels(api); len(models) != 0 || len(skipped) != 1 || !strings.Contains(skipped[0], "API") {
		t.Fatalf("api entry: %v %v", models, skipped)
	}
	if _, err := loadRole(path, "missing"); err == nil {
		t.Fatal("a missing role was accepted")
	}
}

func TestSummaryModelsSkipCodex(t *testing.T) {
	role := ModelConfig{Provider: "openai", Model: "mini", Effort: "high",
		Backup: &ModelConfig{Provider: "anthropic", Model: "haiku", Effort: "low",
			Backup: &ModelConfig{Provider: "openai", Model: "other"}}}
	models, skipped := summaryModels(role)
	if len(models) != 1 || models[0].label() != "anthropic/haiku" || models[0].Effort != "low" || models[0].Backup != nil {
		t.Fatalf("models %+v", models)
	}
	if len(skipped) != 2 || !strings.Contains(skipped[0], "openai/mini") {
		t.Fatalf("skipped %v", skipped)
	}
	if models, _ := summaryModels(ModelConfig{Provider: "openai", Model: "x"}); len(models) != 0 {
		t.Fatal("a Codex-only role must leave no summary model")
	}
	if _, err := runModelCLI(context.Background(), ModelConfig{Provider: "openai", Model: "x"}, "p"); err == nil {
		t.Fatal("the runner accepted a provider that keeps tools")
	}
}

func TestModelEnvIsMinimal(t *testing.T) {
	env := modelEnv([]string{"PATH=/bin", "HOME=/home/u", "CLAUDE_CONFIG_DIR=/c", "FORGEJO_TOKEN_FILE=/secret", "AWS_SECRET_ACCESS_KEY=x", "LANG=C.UTF-8", "PATHX=1"})
	if strings.Join(env, " ") != "PATH=/bin HOME=/home/u CLAUDE_CONFIG_DIR=/c LANG=C.UTF-8" {
		t.Fatalf("env %v", env)
	}
}

func TestLoadSummaryRoles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	config := `[roles.light]
provider = "anthropic"
model = "light-model"
transport = "cli"
effort = "low"
[roles.standard]
provider = "openai"
model = "codex-model"
transport = "cli"
[roles.standard.backup]
provider = "anthropic"
model = "backup-model"
transport = "prefer-cli"
effort = "high"
[roles.standard.backup.backup]
provider = "anthropic"
model = "api-backup"
transport = "api"
[roles.deep]
provider = "anthropic"
model = "api-model"
transport = "api"
`
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, defaultRole := range []string{"light", "standard", "deep", "missing"} {
		t.Run(defaultRole, func(t *testing.T) {
			var logs bytes.Buffer
			log := slog.New(slog.NewTextHandler(&logs, nil))
			models, err := loadSummaryRoles(path, ThreadsConfig{
				SummaryRole: defaultRole, SummaryRoles: []string{"light", "standard", "deep", "missing"},
			}, log)
			wantError := defaultRole == "deep" || defaultRole == "missing"
			if (err != nil) != wantError {
				t.Fatalf("default %q: error %v, want error %v", defaultRole, err, wantError)
			}
			// Unavailable non-default roles do not disable automatic summaries,
			// and usable roles survive when the default is unavailable.
			if len(models) != 2 {
				t.Fatalf("loaded roles %v, want only light and standard", models)
			}
			light := models["light"]
			if len(light) != 1 || light[0].label() != "anthropic/light-model" || light[0].Effort != "low" {
				t.Fatalf("light models %+v", light)
			}
			standard := models["standard"]
			if len(standard) != 1 || standard[0].label() != "anthropic/backup-model" || standard[0].Effort != "high" || standard[0].Backup != nil {
				t.Fatalf("standard must use only its runnable backup: %+v", standard)
			}
			for _, role := range []string{"deep", "missing"} {
				if _, ok := models[role]; ok {
					t.Errorf("unavailable role %q is offered", role)
				}
			}
			for _, skipped := range []string{"openai/codex-model", "anthropic/api-backup", "anthropic/api-model", "missing"} {
				if !strings.Contains(logs.String(), skipped) {
					t.Errorf("skipped entry %q was not logged: %s", skipped, logs.String())
				}
			}
		})
	}
}
