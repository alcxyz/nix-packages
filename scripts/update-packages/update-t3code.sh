#!/usr/bin/env bash
# Track the latest published upstream nightly and the tested fork promotion.
# T3CODE_VERSION may explicitly select an upstream stable or nightly version.
set -euo pipefail
# shellcheck source=scripts/ci/t3code-nix-home.sh disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/../ci/t3code-nix-home.sh"

PIN_FILE="pkgs/t3code/source.json"
HELPER="scripts/update-packages/t3code-source.py"
FAKE_HASH="sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
PREFLIGHT_REPORT="${T3CODE_PREFLIGHT_REPORT:-${RUNNER_TEMP:-${TMPDIR:-/tmp}}/t3code-patch-preflight.json}"
rm -f -- "$PREFLIGHT_REPORT"

current_output=$(python3 "$HELPER" read "$PIN_FILE")
target_output=$(python3 "$HELPER" target)
mapfile -t current <<<"$current_output"
mapfile -t target <<<"$target_output"
target_version="${target[0]}"
target_revision="${target[1]}"
target_fork_version="${target[2]}"
target_fork_revision="${target[3]}"
target_fork_upstream_revision="${target[4]}"
echo "Current upstream: ${current[0]} (${current[1]})"
echo "Target upstream:  $target_version ($target_revision)"
echo "Current fork:     ${current[2]} (${current[3]}, baseline ${current[4]})"
echo "Target fork:      $target_fork_version ($target_fork_revision, baseline $target_fork_upstream_revision)"

if [[ "${current[*]}" == "${target[*]}" && "${FORCE_UPDATE:-false}" != "true" ]]; then
  echo "Already up to date."
  echo "updated=false" >>"${GITHUB_OUTPUT:-/dev/null}"
  exit 0
fi

target_fork_patch_revision=$(
  python3 "$HELPER" fork-patch-revision "$PIN_FILE" \
    "$target_fork_version" "$target_fork_revision" "$target_fork_upstream_revision"
)

# Restore the original pin on any discovery/build failure. Each intermediate
# pin is validated and replaced atomically, so Nix never reads partial JSON.
original=$(mktemp)
cp "$PIN_FILE" "$original"
success=false
cleanup() {
  if [[ "$success" != true ]]; then
    cp "$original" "${PIN_FILE}.restore"
    mv "${PIN_FILE}.restore" "$PIN_FILE"
  fi
  rm -f "$original"
}
trap cleanup EXIT

prefetch_source() {
  local owner=$1
  local revision=$2
  local url="https://github.com/${owner}/t3code/archive/${revision}.tar.gz"
  clean_homeless_shelter
  nix store prefetch-file --unpack --json "$url" |
    python3 -c 'import json,sys; print(json.load(sys.stdin)["hash"])'
}

src_hash=$(prefetch_source pingdotgg "$target_revision")
fork_src_hash=$(prefetch_source alcxyz "$target_fork_revision")

patch_package() {
  python3 "$HELPER" write "$PIN_FILE" \
    "$target_version" "$target_revision" "$src_hash" "$1" "$2" \
    "$target_fork_version" "$target_fork_revision" "$target_fork_upstream_revision" \
    "$fork_src_hash" "$3" "$4" "$target_fork_patch_revision"
}

write_preflight_report() {
  local status=$1
  local exit_code=$2
  mkdir -p "$(dirname "$PREFLIGHT_REPORT")"
  python3 - "$PREFLIGHT_REPORT" "$target_fork_version" "$target_fork_revision" \
    "$target_fork_upstream_revision" "$status" "$exit_code" <<'PY'
import json
from pathlib import Path
import sys
import tempfile

path = Path(sys.argv[1])
report = {
    "schemaVersion": 1,
    "check": "t3code-fork-quota-patch-application",
    "scope": "quota-patch-application-only",
    "target": {
        "version": sys.argv[2],
        "revision": sys.argv[3],
        "upstreamRevision": sys.argv[4],
    },
    "status": sys.argv[5],
    "exitCode": int(sys.argv[6]),
    "fullBuildValidated": False,
}
with tempfile.NamedTemporaryFile(mode="w", dir=path.parent, delete=False) as output:
    temporary = Path(output.name)
    json.dump(report, output, indent=2)
    output.write("\n")
temporary.replace(path)
PY
}

preflight_quota_patch() {
  local log
  local status

  log=$(mktemp)
  clean_homeless_shelter
  if nix build -L .#t3code-fork.src --no-link 2>"$log"; then
    cat "$log" >&2
    rm -f "$log"
    write_preflight_report passed 0
    return 0
  else
    status=$?
  fi

  cat "$log" >&2
  rm -f "$log"
  write_preflight_report failed "$status"
  echo "T3 Code fork quota patch failed for ${target_fork_revision}; stopping before dependency hash builds." >&2
  return "$status"
}

collect_hash() {
  local attr=$1
  local label=$2
  local log
  local hash

  log=$(mktemp)
  clean_homeless_shelter
  if nix build -L "$attr" --no-link 2>"$log"; then
    cat "$log" >&2
    rm -f "$log"
    echo "Expected ${label} build to fail with the placeholder hash" >&2
    exit 1
  fi

  cat "$log" >&2
  hash=$(sed -nE 's/^[[:space:]]*got:[[:space:]]+(sha256-[^[:space:]]+).*/\1/p' "$log" | tail -n1)
  rm -f "$log"

  if [[ ! "$hash" =~ ^sha256-.+ ]]; then
    echo "Unable to collect ${label} hash" >&2
    exit 1
  fi

  printf '%s\n' "$hash"
}

patch_package "$FAKE_HASH" "$FAKE_HASH" "$FAKE_HASH" "$FAKE_HASH"
echo "Checking T3 Code fork quota patch compatibility..."
preflight_quota_patch

echo "Computing upstream resource monitor Cargo hash..."
cargo_hash=$(collect_hash .#t3code.resourceMonitor cargoHash)
patch_package "$cargo_hash" "$FAKE_HASH" "$FAKE_HASH" "$FAKE_HASH"

echo "Computing fork resource monitor Cargo hash..."
fork_cargo_hash=$(collect_hash .#t3code-fork.resourceMonitor forkCargoHash)
patch_package "$cargo_hash" "$FAKE_HASH" "$fork_cargo_hash" "$FAKE_HASH"

echo "Computing upstream pnpm dependency hash..."
pnpm_hash=$(collect_hash .#t3code.pnpmDeps pnpmDeps)
patch_package "$cargo_hash" "$pnpm_hash" "$fork_cargo_hash" "$FAKE_HASH"

echo "Computing fork pnpm dependency hash..."
fork_pnpm_hash=$(collect_hash .#t3code-fork.pnpmDeps forkPnpmDeps)
patch_package "$cargo_hash" "$pnpm_hash" "$fork_cargo_hash" "$fork_pnpm_hash"

echo "Validating both T3 Code flavors and embedded provider closures..."
T3CODE_VERIFY_ALWAYS=true scripts/ci/verify-t3code-providers.sh
success=true
version_label="${target_version} + fork ${target_fork_version}-fork.${target_fork_patch_revision}"
source_urls="https://github.com/pingdotgg/t3code/releases/tag/v${target_version} and fork promotion https://github.com/alcxyz/t3code/commit/${target_fork_revision}"
{
  echo "updated=true"
  echo "version=$version_label"
  echo "fork_version=${target_fork_version}-fork.${target_fork_patch_revision}"
  echo "upstream_url=$source_urls"
} >>"${GITHUB_OUTPUT:-/dev/null}"
