#!/usr/bin/env bash
set -euo pipefail

base_ref="${GITHUB_BASE_REF:-${GITEA_BASE_REF:-${FORGEJO_BASE_REF:-}}}"
if [[ -n "$base_ref" && "${T3CODE_VERIFY_ALWAYS:-false}" != true ]]; then
  git fetch origin "$base_ref"
  if ! git diff --name-only "origin/${base_ref}"...HEAD | grep -Eq \
    '^(flake\.(nix|lock)|pkgs/(claude-code|codex-app-server|codex-cli|t3code)/|scripts/ci/(verify-t3code-providers|t3code-nix-home|ephemeral-nix-home)\.sh$)'
  then
    echo "No T3 Code provider inputs changed; skipping provider closure verification."
    exit 0
  fi
fi

# shellcheck source=scripts/ci/t3code-nix-home.sh
source "$(dirname "${BASH_SOURCE[0]}")/t3code-nix-home.sh"

nix_build() {
  local attempt
  local status

  for attempt in 1 2 3; do
    clean_homeless_shelter
    if nix build -L "$@"; then
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
# The Forgejo runner builds without sandboxing. Some provider/resource builds
# create /homeless-shelter, so isolate them and clean between Nix invocations
# before assembling the already-cached T3 Code closure.
nix_build .#claude-code --no-link
nix_build .#codex-cli --no-link
for flavor in t3code t3code-fork; do
  embedded_claude=$(nix eval --raw .#${flavor}.embeddedProviderVersions.claudeCode)
  embedded_codex=$(nix eval --raw .#${flavor}.embeddedProviderVersions.codexCli)

  if [[ "$embedded_claude" != "$claude_version" ]]; then
    echo "T3 Code embeds Claude Code ${embedded_claude}; package is ${claude_version}" >&2
    exit 1
  fi

  if [[ "$embedded_codex" != "$codex_version" ]]; then
    echo "T3 Code embeds Codex CLI ${embedded_codex}; package is ${codex_version}" >&2
    exit 1
  fi

  nix_build .#${flavor}.pnpmDeps --no-link
  nix_build .#${flavor}.resourceMonitor --no-link
  t3_out=$(nix_build .#${flavor} --no-link --print-out-paths)
  expected_version=$(nix eval --raw .#${flavor}.version)
  runtime_version=$("$t3_out/bin/t3" --version)
  if [[ "$runtime_version" != "t3 v$expected_version" ]]; then
    echo "$flavor reports $runtime_version; package version is $expected_version" >&2
    exit 1
  fi
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

  printf '%s provider contract verified:\n' "$flavor"
  printf '  Claude Code:      %s\n' "$claude_version"
  printf '  Codex CLI:        %s (embedded app-server)\n' "$codex_version"
  printf '  Codex app server: %s (standalone only)\n' "$app_server_version"
done
