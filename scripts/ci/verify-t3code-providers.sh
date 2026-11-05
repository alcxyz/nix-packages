#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
base_ref="${GITHUB_BASE_REF:-${GITEA_BASE_REF:-${FORGEJO_BASE_REF:-}}}"
flavors=(t3code)
standalone_selected=false
if [[ -n "${PACKAGE_BUILD_SELECTED_FILE:-}" ]]; then
  [[ -f "$PACKAGE_BUILD_SELECTED_FILE" ]] || {
    echo "Selected package list is missing: $PACKAGE_BUILD_SELECTED_FILE" >&2
    exit 2
  }
  grep -Fqx t3code "$PACKAGE_BUILD_SELECTED_FILE" || flavors=()
  if grep -Fqx codex-app-server "$PACKAGE_BUILD_SELECTED_FILE"; then standalone_selected=true; fi
elif [[ -n "$base_ref" && "${T3CODE_VERIFY_ALWAYS:-false}" != true ]]; then
  plan_file=$(mktemp)
  trap 'rm -f "$plan_file"' EXIT
  PACKAGE_BUILD_MODE=selected \
    PACKAGE_BUILD_SHARD_COUNT=1 \
    PACKAGE_BUILD_SHARD_INDEX=0 \
    PACKAGE_BUILD_PLAN_ONLY=1 \
    PACKAGE_BUILD_PLAN_FILE="$plan_file" \
    "$script_dir/build-changed-packages.sh"

  grep -Fqx -e '*' -e t3code "$plan_file" || flavors=()
  if grep -Fqx -e '*' -e codex-app-server "$plan_file"; then standalone_selected=true; fi
fi
if ((${#flavors[@]} == 0)) && [[ "$standalone_selected" != true ]]; then
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
claude_out=$(nix_build .#claude-code --no-link --print-out-paths)
codex_out=$(nix_build .#codex-cli --no-link --print-out-paths)
app_server_out=$(nix_build .#codex-app-server --no-link --print-out-paths)

# Run installed executables with no access to the operator's home, settings or
# credentials. env -i also excludes credential-bearing inherited variables.
verify_provider() {
  local executable=$1 expected=$2 label=$3 runtime isolated
  isolated=$(mktemp -d)
  mkdir -p "$isolated/config" "$isolated/cache" "$isolated/data" "$isolated/state"
  if ! runtime=$(env -i HOME="$isolated" XDG_CONFIG_HOME="$isolated/config" \
    XDG_CACHE_HOME="$isolated/cache" XDG_DATA_HOME="$isolated/data" \
    XDG_STATE_HOME="$isolated/state" PATH="$PATH" "$executable" --version); then
    rm -rf "$isolated"
    echo "$label runtime version check failed" >&2
    return 1
  fi
  rm -rf "$isolated"
  # Compare a whole whitespace-delimited version token, never a regex prefix.
  local token
  for token in $runtime; do
    [[ "$token" == "$expected" ]] && return 0
  done
  echo "$label reports $runtime; expected $expected" >&2
  return 1
}
verify_provider "$claude_out/bin/claude" "$claude_version" 'Claude Code'
verify_provider "$codex_out/bin/codex" "$codex_version" 'Codex CLI'
verify_provider "$app_server_out/bin/codex-app-server" "$app_server_version" 'Codex app server'

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
