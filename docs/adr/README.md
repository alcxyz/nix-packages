# Architecture Decision Records

Non-obvious decisions in this repo are documented here. Before changing tool design, dependency choices, build strategy, or any area with a clear trade-off — read the relevant ADR first.

Each ADR listed here states the current decision. When a decision changes, the
ADR is rewritten to say what is true now, and the replaced text moves to a
history file in [`archive/`](archive/README.md), which also holds superseded
and retired ADRs. ADR numbers are never reused.

| ADR | Title | Area |
|-----|-------|------|
| [ADR-0000](0000-adr-template.md) | Template | — |
| [ADR-0001](0001-wcap-go-cli-window-capture.md) | wcap as a Go CLI wrapping gpu-screen-recorder | `tools/wcap` |
| [ADR-0002](0002-model-agnostic-llm-config-for-devlog.md) | Model-Agnostic LLM Configuration for devlog | `tools/devlog/` |
| [ADR-0003](0003-fail-loud-automated-package-updates.md) | Fail-Loud Automated Package Updates | package update CI |
| [ADR-0005](0005-patched-t3code-source-builds.md) | T3 Code Source Builds | `pkgs/t3code/`, `pkgs/codex-cli/` |
| [ADR-0006](0006-runtime-configured-nix-deploy.md) | Runtime-configured nix-deploy | `tools/nix-deploy/` |
| [ADR-0007](0007-continuous-provider-updates.md) | Continuous grouped provider updates | AI provider automation |
| [ADR-0008](0008-nix-deploy-in-go.md) | nix-deploy in Go, with evaluate, plan, build and activate phases | `tools/nix-deploy/` |
| [ADR-0009](0009-devlog-attribution-and-gap-fill.md) | Devlog attribution and gap-fill deduplication | `tools/devlog/` |
| [ADR-0010](0010-fleet-reboot-policies.md) | Fleet reboots by policy and constraint group | `tools/nix-deploy/` |
| [ADR-0011](0011-revkit-agent-review-toolkit.md) | revkit, an agent review toolkit with isolated reviewers | `tools/revkit/` |
| [ADR-0012](0012-deck-t3-thread-dashboard.md) | deck, a live T3 thread dashboard | `tools/deck/` |
| [ADR-0013](0013-deck-page-collections.md) | Operator-authored page collections in Deck | `tools/deck/` |
| [ADR-0014](0014-deck-backlog-view.md) | deck's backlog view, a live backlog overview | `tools/deck/`, `tools/gh-app/` |
| [ADR-0015](0015-deck-design-system.md) | One design for deck's views and page bundles, chosen by purpose | `tools/deck/web/`, page bundles |
| [ADR-0016](0016-nixfleet-triage.md) | nixfleet triage: run records and an agent playbook for failed deploys and warnings | `tools/nix-deploy/` |
