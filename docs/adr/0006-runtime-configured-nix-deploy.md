# ADR-0006: Runtime-configured nix-deploy

**Status:** Accepted
**Date:** 2026-09-09
**Applies to:** `tools/nix-deploy/`

## Context

The reusable deploy command previously contained a stale built-in host list,
while its actively maintained copy lived in a consumer repository because it
needed that repository's inventory. Keeping two command implementations caused
their behavior to diverge.

## Decision

`nix-deploy` is the canonical generic command. It requires a versioned JSON
inventory selected with `--config FILE` or `NIX_DEPLOY_CONFIG`. Version 1
contains the operator host, Home Manager user and output prefix, host sets,
aliases, SSH endpoints and users, remote-sudo hosts, activation modes, and
optional display colors.

The command parses the document with `jq`, validates types and cross-references,
and rejects values that could become shell command fragments. It does not ship
personal host defaults. Consumer repositories generate their own inventory and
may wrap the command with a default configuration path. Fleet child commands
receive the selected path explicitly.

The package supplies Bash, core utilities, hostname, jq, grep, sed, OpenSSH, and
GNU parallel on `PATH`. Deployment front ends remain runtime requirements:
`nix`, `nixos-rebuild` (or the existing `nix run` fallback), `home-manager`,
`sudo`, and nix-darwin's system rebuild command.

### JSON interface version 1

The required fields are:

| Field | Type | Meaning |
| --- | --- | --- |
| `schemaVersion` | integer | Must be `1` |
| `operatorHost` | string | Host allowed to run the canonical `--all` workflow |
| `homeManagerUser` | string | SSH user for remote Home Manager activation |
| `homeOutputPrefix` | string | Prefix joined with a host to select a Home Manager flake output |
| `knownHosts` | unique string array | Canonical host names |
| `homeManagerHosts` | unique string array | Hosts eligible for explicit Home Manager deployment |
| `remoteHosts` | unique string array | Fleet targets used by `--here` away from the operator host |
| `deployAllHosts` | unique string array | Ordered canonical fleet targets |
| `aliases` | string map | User-facing alias to canonical host |
| `sshHosts` | string map | Canonical host to SSH hostname or IPv4 address |
| `systemSshUsers` | string map | Canonical host to non-default system SSH user |
| `systemRemoteSudoHosts` | unique string array | Hosts whose rebuild requires `--ask-sudo-password` |
| `systemActivationModes` | string map | Canonical host to `switch` or `boot` |
| `hostColors` | string map, optional | Canonical host to an `R;G;B` display color |

Every host reference must resolve through `knownHosts`; aliases must not shadow
canonical names. Validation and loading use one in-memory snapshot of the file. Names, users, prefixes,
and endpoints accept only the characters needed by the current command
interface; whitespace and shell metacharacters are rejected before planning.

## Alternatives Considered

- **Keep the consumer-specific implementation** — rejected because fixes and
  tests would continue to bypass the reusable package.
- **Generate shell declarations and evaluate them** — rejected because it
  would turn inventory data into executable input.
- **Add generic policy abstractions** — deferred. The schema represents the
  existing command inputs and does not define broader deployment policy.

## Consequences

- One package owns CLI behavior and black-box contract tests. The contract lives
  next to the executable under `tools/nix-deploy`, so a tool-specific test
  change selects its owning package in CI. Shared CI/flake changes still
  select the full native matrix, and all exported platforms are always evaluated.
- Missing, malformed, or unsupported inventory fails before deployment work.
- Schema changes require a new version or backward-compatible optional fields.
- Fleet system and Home Manager phases derive independent candidates from the
  selected fleet. The Home Manager phase considers only `homeManagerHosts`,
  including an eligible local host, so a system endpoint failure does not
  suppress a separately reachable Home Manager endpoint. `--fail-unreachable`
  still makes an unreachable candidate fail its phase instead of being
  skipped.
- Fleet mode starts the local sudo keepalive only when the reachable system
  phase contains a local rebuild job. Remote-only orchestration does not prompt
  for or refresh local sudo credentials.
