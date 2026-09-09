#!/usr/bin/env bash
# Track the latest published nightly and validate both flavors from one source pin.
# T3CODE_VERSION may explicitly select a stable or nightly version (without v).
set -euo pipefail
# shellcheck source=scripts/ci/t3code-nix-home.sh
source "$(dirname "${BASH_SOURCE[0]}")/../ci/t3code-nix-home.sh"

PIN_FILE="pkgs/t3code/source.json"
HELPER="scripts/update-packages/t3code-source.py"
FAKE_HASH="sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

current=$(python3 "$HELPER" read "$PIN_FILE")
target=$(python3 "$HELPER" target)
current_version=$(head -n1 <<<"$current")
target_version=$(head -n1 <<<"$target")
target_revision=$(tail -n1 <<<"$target")
echo "Current: $current_version"
echo "Target:  $target_version ($target_revision)"

if [[ "$current" == "$target" && "${FORCE_UPDATE:-false}" != "true" ]]; then
  echo "Already up to date."
  echo "updated=false" >>"${GITHUB_OUTPUT:-/dev/null}"
  exit 0
fi

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

src_url="https://github.com/pingdotgg/t3code/archive/${target_revision}.tar.gz"
clean_homeless_shelter
src_hash=$(nix store prefetch-file --unpack --json "$src_url" |
  python3 -c 'import json,sys; print(json.load(sys.stdin)["hash"])')

patch_package() {
  python3 "$HELPER" write "$PIN_FILE" "$target_version" "$target_revision" "$src_hash" "$1" "$2"
}

collect_hash() {
  local attr="$1"
  local label="$2"
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

patch_package "$FAKE_HASH" "$FAKE_HASH"
echo "Computing resource monitor Cargo hash..."
cargo_hash=$(collect_hash .#t3code.resourceMonitor cargoHash)
patch_package "$cargo_hash" "$FAKE_HASH"
echo "Computing pnpm dependency hash..."
pnpm_hash=$(collect_hash .#t3code.pnpmDeps pnpmDeps)
patch_package "$cargo_hash" "$pnpm_hash"

echo "Validating both T3 Code flavors and embedded provider closures..."
T3CODE_VERIFY_ALWAYS=true scripts/ci/verify-t3code-providers.sh
success=true
echo "updated=true" >>"${GITHUB_OUTPUT:-/dev/null}"
echo "version=$target_version" >>"${GITHUB_OUTPUT:-/dev/null}"
