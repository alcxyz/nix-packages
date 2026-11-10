#!/usr/bin/env bash
# Evaluation-only T3 Code provider check, cheap enough for the shared CI
# runners (ADR-0003): every provider derivation must evaluate, and T3 Code's
# embeddedProviderVersions metadata must name the packaged Claude Code and
# Codex CLI versions. The flake passes the exported provider packages to T3
# Code and the metadata reads their versions back, so the comparison only
# catches a change to that wiring; it does not prove what the built application
# embeds. Only a native build shows that: the local provider verification
# script builds T3 Code and checks the runtime versions and closure.
set -euo pipefail

claude_version=$(nix eval --raw .#claude-code.version)
codex_version=$(nix eval --raw .#codex-cli.version)
embedded_claude=$(nix eval --raw .#t3code.embeddedProviderVersions.claudeCode)
embedded_codex=$(nix eval --raw .#t3code.embeddedProviderVersions.codexCli)

if [[ "$embedded_claude" != "$claude_version" ]]; then
  echo "T3 Code embeds Claude Code ${embedded_claude}; package is ${claude_version}" >&2
  exit 1
fi
if [[ "$embedded_codex" != "$codex_version" ]]; then
  echo "T3 Code embeds Codex CLI ${embedded_codex}; package is ${codex_version}" >&2
  exit 1
fi

for attr in t3code t3code.pnpmDeps t3code.resourceMonitor claude-code codex-cli codex-app-server; do
  nix eval --raw ".#${attr}.drvPath" >/dev/null
done

echo "T3 Code provider versions evaluated: Claude Code ${claude_version}, Codex CLI ${codex_version}."
