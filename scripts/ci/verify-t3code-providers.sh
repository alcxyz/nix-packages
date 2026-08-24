#!/usr/bin/env bash
set -euo pipefail

claude_version=$(nix eval --raw .#claude-code.version)
codex_version=$(nix eval --raw .#codex-cli.version)
app_server_version=$(nix eval --raw .#codex-app-server.version)
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

t3_out=$(nix build .#t3code --no-link --print-out-paths)
references=$(nix-store -q --references "$t3_out")

if ! grep -Eq -- "-claude-code-${claude_version}$" <<<"$references"; then
  echo "T3 Code closure does not directly reference claude-code-${claude_version}" >&2
  exit 1
fi

if ! grep -Eq -- "-codex-cli-${codex_version}$" <<<"$references"; then
  echo "T3 Code closure does not directly reference codex-cli-${codex_version}" >&2
  exit 1
fi

if grep -Eq -- '-codex-app-server-' <<<"$references"; then
  echo "T3 Code unexpectedly references the standalone codex-app-server package" >&2
  exit 1
fi

printf 'T3 Code provider contract verified:\n'
printf '  Claude Code:      %s\n' "$claude_version"
printf '  Codex CLI:        %s (embedded app-server)\n' "$codex_version"
printf '  Codex app server: %s (standalone only)\n' "$app_server_version"
