#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
base_ref="${GITHUB_BASE_REF:-${GITEA_BASE_REF:-${FORGEJO_BASE_REF:-}}}"
flavors=(t3code t3code-fork)
if [[ -n "${PACKAGE_BUILD_SELECTED_FILE:-}" ]]; then
  [[ -f "$PACKAGE_BUILD_SELECTED_FILE" ]] || {
    echo "Selected package list is missing: $PACKAGE_BUILD_SELECTED_FILE" >&2
    exit 2
  }
  flavors=()
  for flavor in t3code t3code-fork; do
    if grep -Fqx "$flavor" "$PACKAGE_BUILD_SELECTED_FILE"; then
      flavors+=("$flavor")
    fi
  done
elif [[ -n "$base_ref" && "${T3CODE_VERIFY_ALWAYS:-false}" != true ]]; then
  plan_file=$(mktemp)
  trap 'rm -f "$plan_file"' EXIT
  PACKAGE_BUILD_MODE=selected \
    PACKAGE_BUILD_SHARD_COUNT=1 \
    PACKAGE_BUILD_SHARD_INDEX=0 \
    PACKAGE_BUILD_PLAN_ONLY=1 \
    PACKAGE_BUILD_PLAN_FILE="$plan_file" \
    "$script_dir/build-changed-packages.sh"

  flavors=()
  if grep -Fqx '*' "$plan_file"; then
    flavors=(t3code t3code-fork)
  else
    grep -Fqx t3code "$plan_file" && flavors+=(t3code)
    grep -Fqx t3code-fork "$plan_file" && flavors+=(t3code-fork)
  fi

fi
if ((${#flavors[@]} == 0)); then
  echo "No T3 Code provider inputs changed; skipping provider closure verification."
  exit 0
fi

# shellcheck source=scripts/ci/t3code-nix-home.sh disable=SC1091
source "$script_dir/t3code-nix-home.sh"

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
for flavor in "${flavors[@]}"; do
  embedded_claude=$(nix eval --raw ".#${flavor}.embeddedProviderVersions.claudeCode")
  embedded_codex=$(nix eval --raw ".#${flavor}.embeddedProviderVersions.codexCli")

  if [[ "$embedded_claude" != "$claude_version" ]]; then
    echo "T3 Code embeds Claude Code ${embedded_claude}; package is ${claude_version}" >&2
    exit 1
  fi

  if [[ "$embedded_codex" != "$codex_version" ]]; then
    echo "T3 Code embeds Codex CLI ${embedded_codex}; package is ${codex_version}" >&2
    exit 1
  fi

  nix_build ".#${flavor}.pnpmDeps" --no-link
  nix_build ".#${flavor}.resourceMonitor" --no-link
  t3_out=$(nix_build ".#${flavor}" --no-link --print-out-paths)
  expected_version=$(nix eval --raw ".#${flavor}.version")
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
