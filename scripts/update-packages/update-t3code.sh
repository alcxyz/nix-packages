#!/usr/bin/env bash
# Update the published upstream T3 Code nightly.
set -euo pipefail
# shellcheck source=scripts/ci/t3code-nix-home.sh disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/../ci/t3code-nix-home.sh"

PIN_FILE=pkgs/t3code/source.json
HELPER=scripts/update-packages/t3code-source.py
FAKE_HASH=sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=
work=$(mktemp -d)
success=false
cleanup() {
  if [[ "$success" != true && -f "$work/original.json" ]]; then
    cp "$work/original.json" "${PIN_FILE}.restore"
    mv "${PIN_FILE}.restore" "$PIN_FILE"
  fi
  rm -rf -- "$work"
}
trap cleanup EXIT

python3 "$HELPER" target >"$work/target.json"
should_update=$(python3 "$HELPER" decide "$PIN_FILE" "$work/target.json")
if [[ "$should_update" == false && "${FORCE_UPDATE:-false}" != true ]]; then
  echo 'Already up to date.'
  echo 'updated=false' >>"${GITHUB_OUTPUT:-/dev/null}"
  success=true
  exit 0
fi
cp "$PIN_FILE" "$work/original.json"

clean_homeless_shelter
source_hash=$(
  nix store prefetch-file --unpack --json \
    "https://github.com/pingdotgg/t3code/archive/$(jq -r .revision "$work/target.json").tar.gz" |
    python3 -c 'import json,sys; print(json.load(sys.stdin)["hash"])'
)
jq -n --arg hash "$source_hash" --arg fake "$FAKE_HASH" \
  '{hash: $hash, cargoHash: $fake, pnpmDepsHash: $fake}' >"$work/hashes.json"

stage() {
  python3 "$HELPER" materialize "$PIN_FILE" "$work/target.json" "$work/hashes.json"
}
set_hash() {
  jq --arg field "$1" --arg value "$2" '.[$field] = $value' "$work/hashes.json" >"$work/hashes.next"
  mv "$work/hashes.next" "$work/hashes.json"
  stage
}
collect_hash() {
  local attr=$1 label=$2 log hash
  log=$(mktemp)
  clean_homeless_shelter
  if nix build -L "$attr" --no-link 2>"$log"; then
    cat "$log" >&2
    rm -f "$log"
    echo "Expected ${label} build to fail with the placeholder hash" >&2
    return 1
  fi
  cat "$log" >&2
  hash=$(sed -nE 's/^[[:space:]]*got:[[:space:]]+(sha256-[^[:space:]]+).*/\1/p' "$log" | tail -n1)
  rm -f "$log"
  if [[ ! "$hash" =~ ^sha256-.+ ]]; then
    echo "Unable to collect ${label} hash" >&2
    return 1
  fi
  printf '%s\n' "$hash"
}

stage
set_hash cargoHash "$(collect_hash .#t3code.resourceMonitor cargoHash)"
set_hash pnpmDepsHash "$(collect_hash .#t3code.pnpmDeps pnpmDepsHash)"
T3CODE_VERIFY_ALWAYS=true scripts/ci/verify-t3code-providers.sh
success=true
version=$(jq -r .version "$work/target.json")
printf 'updated=true\nversion=%s\n' "$version" >>"${GITHUB_OUTPUT:-/dev/null}"
