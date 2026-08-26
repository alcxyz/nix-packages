#!/usr/bin/env bash
set -euo pipefail

base_ref="${GITHUB_BASE_REF:-${GITEA_BASE_REF:-${FORGEJO_BASE_REF:-}}}"
if [[ -n "$base_ref" ]]; then
  git fetch origin "$base_ref"
  if ! git diff --name-only "origin/${base_ref}"...HEAD | grep -Eq \
    '^(flake\.(nix|lock)|pkgs/(claude-code|codex-app-server|codex-cli|t3code)/|scripts/ci/verify-t3code-providers\.sh$)'
  then
    echo "No T3 Code provider inputs changed; skipping provider closure verification."
    exit 0
  fi
fi

clean_homeless_shelter() {
  local attempt

  for attempt in {1..10}; do
    rm -rf /homeless-shelter
    sleep 1
    if [[ ! -e /homeless-shelter ]]; then
      sleep 1
      [[ ! -e /homeless-shelter ]] && return 0
    fi
  done

  echo "Unable to keep /homeless-shelter absent before a non-sandboxed Nix build." >&2
  return 1
}

nix_build() {
  local attempt
  local status

  for attempt in 1 2 3; do
    clean_homeless_shelter
    if nix build "$@"; then
      return 0
    else
      status=$?
    fi

    if [[ ! -e /homeless-shelter || "$attempt" -eq 3 ]]; then
      return "$status"
    fi

    echo "Retrying Nix build after /homeless-shelter was recreated (attempt $((attempt + 1))/3)." >&2
  done
}

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

# The Forgejo runner builds without sandboxing. Some provider/resource builds
# create /homeless-shelter, so isolate them and clean between Nix invocations
# before assembling the already-cached T3 Code closure.
nix_build .#claude-code --no-link
nix_build .#codex-cli --no-link
nix_build .#t3code.pnpmDeps --no-link
nix_build .#t3code.resourceMonitor --no-link
t3_out=$(nix_build .#t3code --no-link --print-out-paths)
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
