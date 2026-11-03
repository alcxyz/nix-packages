#!/usr/bin/env bash
# Evaluation-only T3 Code provider contract, cheap enough for the shared CI
# runners (ADR-0003): T3 Code must embed the packaged Claude Code and Codex CLI
# versions, and every provider derivation must evaluate. The native build,
# runtime version and closure checks are in verify-t3code-providers.sh, which
# runs locally.
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
