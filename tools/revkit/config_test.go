package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validConfig = `{
  "schemaVersion": 1,
  "reviewers": [
    {"name": "gpt", "client": "codex", "model": "gpt-x", "effort": "xhigh"},
    {"name": "opus", "client": "claude", "model": "opus", "effort": "high"}
  ],
  "timeoutSeconds": 1800,
  "environment": ["EXTRA"],
  "homeLinks": [".nix-profile"],
  "readablePaths": ["/nix/store"],
  "repositories": [
    {"name": "example", "context": "A small example.", "lenses": [
      {"name": "docs-drift", "prompt": "Documentation drift."}
    ]}
  ]
}`

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "revkit.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfig(t *testing.T) {
	c, err := loadConfig(writeConfig(t, validConfig))
	if err != nil {
		t.Fatal(err)
	}
	if repo, ok := c.repository("example"); !ok || repo.Lenses[0].Name != "docs-drift" {
		t.Fatalf("repository = %+v, %v", repo, ok)
	}
}

func TestLoadConfigRejects(t *testing.T) {
	cases := map[string][2]string{
		"version":           {`"schemaVersion": 1`, `"schemaVersion": 2`},
		"unknown field":     {`"timeoutSeconds"`, `"timeout": 1, "timeoutSeconds"`},
		"duplicate":         {`"name": "opus"`, `"name": "gpt"`},
		"client":            {`"client": "claude"`, `"client": "gemini"`},
		"model":             {`"model": "opus"`, `"model": "opus --yolo"`},
		"effort":            {`"effort": "high"`, `"effort": "high\"; x"`},
		"timeout":           {`"timeoutSeconds": 1800`, `"timeoutSeconds": 0`},
		"home variable":     {`["EXTRA"]`, `["HOME"]`},
		"xdg variable":      {`["EXTRA"]`, `["XDG_RUNTIME_DIR"]`},
		"bad variable":      {`["EXTRA"]`, `["A-B"]`},
		"relative state":    {`"timeoutSeconds": 1800`, `"timeoutSeconds": 1800, "stateDir": "state"`},
		"empty prompt":      {`"prompt": "Documentation drift."`, `"prompt": " "`},
		"bad lens name":     {`"name": "docs-drift"`, `"name": "docs drift"`},
		"bad repository":    {`"name": "example"`, `"name": "../example"`},
		"reserved link":     {`[".nix-profile"]`, `[".codex"]`},
		"nested link":       {`[".nix-profile"]`, `[".nix-profile/bin"]`},
		"relative readable": {`["/nix/store"]`, `["nix/store"]`},
		"quoted readable":   {`["/nix/store"]`, `["/nix/store\"=\"read"]`},
		"bad matcher":       {`"timeoutSeconds": 1800`, `"matcher": {"name": "m", "client": "gemini", "model": "x", "effort": "low"}, "timeoutSeconds": 1800`},
		"http tracker":      {`"lenses": [`, `"issues": {"url": "http://forge.example", "owner": "o", "repo": "r", "tokenFile": "/t"}, "lenses": [`},
		"relative token":    {`"lenses": [`, `"issues": {"url": "https://forge.example", "owner": "o", "repo": "r", "tokenFile": "t"}, "lenses": [`},
		"bad owner":         {`"lenses": [`, `"issues": {"url": "https://forge.example", "owner": "o/x", "repo": "r", "tokenFile": "/t"}, "lenses": [`},
		"trailing content":  {`"timeoutSeconds": 1800`, `"timeoutSeconds": 1800`},
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			content := strings.Replace(validConfig, edit[0], edit[1], 1)
			if name == "trailing content" {
				content += "{}"
			}
			if content == validConfig && name != "trailing content" {
				t.Fatalf("the edit %q did not apply", edit[0])
			}
			if _, err := loadConfig(writeConfig(t, content)); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

// TestLoadConfigRejectsEmptyLists empties the lists in the parsed document,
// so nothing but the empty-list checks can reject it.
func TestLoadConfigRejectsEmptyLists(t *testing.T) {
	for _, empty := range []string{"reviewers", "lenses"} {
		var document map[string]any
		if err := json.Unmarshal([]byte(validConfig), &document); err != nil {
			t.Fatal(err)
		}
		if empty == "reviewers" {
			document["reviewers"] = []any{}
		} else {
			document["repositories"].([]any)[0].(map[string]any)["lenses"] = []any{}
		}
		content, _ := json.Marshal(document)
		if _, err := loadConfig(writeConfig(t, string(content))); err == nil || !strings.Contains(err.Error(), "no lenses") && !strings.Contains(err.Error(), "at least one reviewer") {
			t.Fatalf("empty %s: %v", empty, err)
		}
	}
}

func TestLoadConfigAcceptsATracker(t *testing.T) {
	content := strings.Replace(validConfig, `"lenses": [`,
		`"issues": {"url": "https://forge.example", "owner": "o", "repo": "r.x", "tokenFile": "/run/token", "limit": 2}, "lenses": [`, 1)
	c, err := loadConfig(writeConfig(t, content))
	if err != nil {
		t.Fatal(err)
	}
	repo, _ := c.repository("example")
	if repo.Issues == nil || repo.Issues.limit() != 2 {
		t.Fatalf("issues = %+v", repo.Issues)
	}
	repo.Issues.Limit = 0
	if repo.Issues.limit() != defaultIssueLimit {
		t.Fatal("no default limit")
	}
}
