# ADR-0002: Model-Agnostic LLM Configuration for devlog

**Status:** Accepted
**Date:** 2026-05-02
**Applies to:** `tools/devlog/`

## Context

The devlog tool hardcodes `claude-sonnet-4-6` in its `runClaude()` helper (`main.go:402`):

```go
cmd := exec.Command("claude", "-p", "--model", "claude-sonnet-4-6")
```

Both daily and weekly generation flow through this single call site. If Claude is unavailable (API outage, rate limit, session expiry), the systemd timers fail silently and no devlog entry is produced.

paperless-tools solved the same problem with [ADR-009](../../../src/tools/paperless-tools/docs/adr/ADR-009-model-agnostic-llm-config.md), introducing a role-based config at `$XDG_CONFIG_HOME/paperweight/config.toml` with provider, model, and optional backup per role. That approach keeps config scoped to the tool, simple, and independent of other tools.

## Decision

Follow the same pattern as paperless-tools ADR-009, scoped to devlog.

### Config file

devlog reads its LLM settings from:

```
$XDG_CONFIG_HOME/devlog/config.toml
```

defaulting to `~/.config/devlog/config.toml` via `os.UserConfigDir()`.

### Schema

devlog uses a single role (`model`) since it has only one LLM call site. Optional backup for fallback:

```toml
[model]
provider = "anthropic"
model    = "sonnet"

[model.backup]
provider = "openai"
model    = "gpt-4.1"
api_key_env = "OPENAI_API_KEY"
```

### Auth resolution

Same as paperless-tools ADR-009:

1. If `api_key_env` is set and the named env var (or `<env var>_FILE`) resolves — direct HTTP.
2. Otherwise — CLI with logged-in session (`claude -p --model <model>`).

### Defaults

If no config file exists, devlog behaves exactly as today: Anthropic CLI, model `claude-sonnet-4-6`. Zero-config must always work.

### Migration

1. Add config loading to `tools/devlog/` — a small TOML parse using `BurntSushi/toml` (already used elsewhere in the infra).
2. Update `runClaude()` to resolve provider and model from config, falling back to current hardcoded values.
3. Optionally deploy config file via nix-config home-manager.
4. Add backup provider support (retry with backup entry on primary failure).

## Alternatives Considered

- **Shared config across all tools (`$XDG_CONFIG_HOME/llm/config.toml`).** Rejected — adds implicit coupling between tools that don't otherwise depend on each other. Each tool has different LLM calling conventions, lives in a different repo, and should be independently configurable.
- **Environment variable only (`DEVLOG_MODEL`).** Simple for model selection but doesn't support backup providers or auth configuration.
- **NixOS module-level config.** Would work for the systemd service but not if devlog is ever run interactively. XDG config covers both.

## Consequences

- Zero-config still works — existing deployments see no change.
- Adding a backup provider requires only a config file, no code changes.
- Config is user-specific (XDG), not repo-specific — different hosts can have different provider setups, which matters for headless systemd-timer hosts that may prefer API keys over CLI sessions.
- Other tools (leantime-tidy, future nix-packages tools) follow the same pattern independently with their own XDG config, tracked by their own ADRs.
