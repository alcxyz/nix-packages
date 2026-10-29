# ADR-0002: Model-Agnostic LLM Configuration for devlog

**Status:** Accepted
**Date:** 2026-05-02
**Amended:** 2026-10-09 by nix-config [ADR-0085](https://git.alc.xyz/alcxyz/nix-config/src/branch/dev/docs/adr/archive/0085-one-role-vocabulary.md) (since moved to a private repository): devlog reads the `standard` role from the shared LLM config instead of its own config file.
**History:** [replaced text](archive/0002-model-agnostic-llm-config-for-devlog-history.md)
**Applies to:** `tools/devlog/`

## Context

devlog generates daily and weekly entries from systemd timers through one LLM
call site. A model hardcoded in that call site made every run depend on one
provider and model: if it was unavailable (API outage, rate limit, session
expiry), the timers could fail before a devlog entry was produced.

paperless-tools solved the same problem with
[ADR-009](https://git.alc.xyz/alcxyz/paperless-tools/src/branch/dev/docs/adr/ADR-009-model-agnostic-llm-config.md):
provider, model and an optional backup per role, resolved from configuration
rather than code. Role names are now one vocabulary across tools
(the agent role decision, now in a private repository).

## Decision

devlog takes its model from configuration, with an optional backup provider,
and works without any configuration.

### Config file

devlog reads the `standard` role from the shared LLM config:

```
$XDG_CONFIG_HOME/llm/config.toml
```

defaulting to `~/.config/llm/config.toml` via `os.UserConfigDir()`. It has no
config file of its own.

### Schema

devlog uses the one role, since it has only one LLM call site, with an
optional backup for fallback:

```toml
[roles.standard]
provider  = "anthropic"
model     = "opus"
transport = "cli"
effort    = "high"          # optional, CLI transports only

[roles.standard.backup]
provider    = "openai"
model       = "gpt-4.1"
transport   = "api"
api_key_env = "OPENAI_API_KEY"
```

`provider` is `anthropic` (the default when empty) or `openai`. `model` and
`transport` are required. If the file exists but has no `roles.standard`, or
an entry lacks a model or transport or names an unknown transport, devlog
fails with an error naming the file and role. An unknown provider is only
rejected when it is called, so a primary with one falls back to the backup.

### Auth resolution

The transport selects how a provider is called:

- `cli`: the provider's CLI with its logged-in session (`claude -p --model
  <model>`), passing `effort` when set.
- `api`: direct HTTP; requires `api_key_env` to name an environment variable
  (or `<env var>_FILE`) that resolves.
- `prefer-api`: direct HTTP when the key resolves, otherwise the CLI.
- `prefer-cli`: the CLI.

If the primary provider fails and a backup is configured, devlog retries once
with the backup.

### Defaults

If the shared config file does not exist, devlog uses Anthropic `opus` over
the `cli` transport. `opus` is a Claude Code alias, not an API model ID, so the
default must use the CLI. Zero-config must always work.

## Alternatives Considered

- **A devlog-specific config file (`$XDG_CONFIG_HOME/devlog/config.toml`).**
  The original decision, replaced when roles became one vocabulary across
  tools: one shared file avoids repeating provider
  setups per tool.
- **Environment variable only (`DEVLOG_MODEL`).** Simple for model selection
  but doesn't support backup providers or auth configuration.
- **NixOS module-level config.** Would work for the systemd service but not if
  devlog is ever run interactively. XDG config covers both.

## Consequences

- Zero-config still works.
- Adding a backup provider requires only a config change, no code changes.
- Config is user-specific (XDG), not repo-specific: different hosts can have
  different provider setups, which matters for headless systemd-timer hosts
  that may prefer API keys over CLI sessions.
- devlog depends on the shared config's `standard` role; renaming or removing
  that role breaks devlog runs on hosts that have the file.
- `devlog catch-up` covers host or timer outages by scanning a recent date
  window for missing daily entries and generating only the holes. When it
  creates daily entries for a completed week, it refreshes that weekly summary
  so late backfills are reflected in the aggregate view.
