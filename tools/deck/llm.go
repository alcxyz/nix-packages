package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// summaryRole is the agent role deck requests from the shared LLM config.
const summaryRole = "light"

// ModelConfig is one entry of the shared LLM config's roles table, the same
// shape devlog reads.
type ModelConfig struct {
	Provider  string       `toml:"provider"`
	Model     string       `toml:"model"`
	Transport string       `toml:"transport"`
	Effort    string       `toml:"effort"`
	APIKeyEnv string       `toml:"api_key_env"`
	Backup    *ModelConfig `toml:"backup"`
}

type sharedConfig struct {
	Roles map[string]ModelConfig `toml:"roles"`
}

func (m ModelConfig) label() string {
	return m.Provider + "/" + m.Model
}

func sharedConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "llm", "config.toml"), nil
}

// loadRole reads a role from the shared LLM config. deck only runs the CLI
// transports, so an api-only role is refused.
func loadRole(path, role string) (ModelConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ModelConfig{}, fmt.Errorf("read shared llm config: %w", err)
	}
	var cfg sharedConfig
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return ModelConfig{}, fmt.Errorf("parse shared llm config %s: %w", path, err)
	}
	rc, ok := cfg.Roles[role]
	if !ok {
		return ModelConfig{}, fmt.Errorf("shared llm config %s is missing roles.%s", path, role)
	}
	if err := normalizeModelConfig(&rc); err != nil {
		return ModelConfig{}, fmt.Errorf("invalid shared llm config %s role %s: %w", path, role, err)
	}
	return rc, nil
}

func normalizeModelConfig(cfg *ModelConfig) error {
	if cfg.Provider == "" {
		cfg.Provider = "anthropic"
	}
	if cfg.Provider != "anthropic" && cfg.Provider != "openai" {
		return fmt.Errorf("unsupported provider %q", cfg.Provider)
	}
	if cfg.Model == "" {
		return fmt.Errorf("missing model for provider %s", cfg.Provider)
	}
	switch cfg.Transport {
	case "cli", "prefer-cli", "prefer-api", "api":
		// deck has no API client: the prefer transports use the CLI, and
		// summaryModels skips api entries.
	case "":
		return fmt.Errorf("missing transport for provider %s", cfg.Provider)
	default:
		return fmt.Errorf("unsupported transport %q", cfg.Transport)
	}
	if cfg.Backup != nil {
		if err := normalizeModelConfig(cfg.Backup); err != nil {
			return fmt.Errorf("backup: %w", err)
		}
	}
	return nil
}

// claudeArgs runs Claude Code once, non-interactively, with no tools, no MCP
// servers, no hooks, skills or user instructions, and no saved session.
func claudeArgs(model, effort string) []string {
	args := []string{"-p", "--model", model}
	if effort != "" {
		args = append(args, "--effort", effort)
	}
	return append(args,
		"--safe-mode", "--restricted", "--tools", "",
		"--strict-mcp-config", "--disable-slash-commands",
		"--no-session-persistence", "--permission-mode", "dontAsk",
		"--output-format", "text",
	)
}

// summaryModels lists the entries of a role, primary first and then each
// backup, that can run a summary with no tools at all. Only Claude Code can
// (--tools ""): codex exec keeps tools such as its patch and image viewer
// even with its shell features off, so Codex entries are skipped, as are
// api-only entries, each with the reason.
func summaryModels(role ModelConfig) ([]ModelConfig, []string) {
	var models []ModelConfig
	var skipped []string
	for m := &role; m != nil; m = m.Backup {
		entry := *m
		entry.Backup = nil
		switch {
		case entry.Transport == "api":
			skipped = append(skipped, entry.label()+": deck has no API transport")
		case entry.Provider == "anthropic":
			models = append(models, entry)
		default:
			skipped = append(skipped, entry.label()+": its CLI cannot run without tools")
		}
	}
	return models, skipped
}

// modelEnvKeys are the only variables a model CLI gets: enough to find
// itself and its own login, never the service's environment.
var modelEnvKeys = []string{"PATH", "HOME", "CLAUDE_CONFIG_DIR", "TMPDIR", "LANG"}

func modelEnv(environ []string) []string {
	var env []string
	for _, kv := range environ {
		key, _, ok := strings.Cut(kv, "=")
		if ok && oneOf(key, modelEnvKeys) {
			env = append(env, kv)
		}
	}
	return env
}

// modelRunner runs one model call; tests replace it.
type modelRunner func(ctx context.Context, m ModelConfig, prompt string) ([]byte, error)

func runModelCLI(ctx context.Context, m ModelConfig, prompt string) ([]byte, error) {
	if m.Provider != "anthropic" {
		return nil, fmt.Errorf("provider %q cannot run without tools", m.Provider)
	}
	dir, err := os.MkdirTemp("", "deck-model-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	cmd := exec.CommandContext(ctx, "claude", claudeArgs(m.Model, m.Effort)...)
	cmd.Dir = dir
	cmd.Env = modelEnv(os.Environ())
	cmd.Stdin = strings.NewReader(prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := runGroup(cmd); err != nil {
		return nil, fmt.Errorf("%s: %w%s", cmd.Args[0], err, stderrHint(stderr.Bytes()))
	}
	return stdout.Bytes(), nil
}

// callModel runs the first model and, while one fails, the next. Each entry
// has its own modelTimeout.
func callModel(ctx context.Context, run modelRunner, models []ModelConfig, prompt string, allowCall func() bool) ([]byte, string, error) {
	if len(models) == 0 {
		return nil, "", errors.New("no model can run summaries")
	}
	var failures []string
	for _, m := range models {
		if !allowCall() {
			if len(failures) == 0 {
				return nil, "", errBudget
			}
			return nil, m.label(), fmt.Errorf("%s; no budget left for %s", strings.Join(failures, "; "), m.label())
		}
		callCtx, cancel := context.WithTimeout(ctx, modelTimeout)
		out, err := run(callCtx, m, prompt)
		cancel()
		if err == nil {
			return out, m.label(), nil
		}
		failures = append(failures, m.label()+": "+err.Error())
		if ctx.Err() != nil {
			break
		}
	}
	return nil, models[len(models)-1].label(), errors.New(strings.Join(failures, "; "))
}
