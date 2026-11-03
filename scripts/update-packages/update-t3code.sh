#!/usr/bin/env bash
# Update the published upstream T3 Code nightly.
set -euo pipefail
# shellcheck source=scripts/ci/t3code-nix-home.sh disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/../ci/t3code-nix-home.sh"
# shellcheck source=scripts/update-packages/fod-hash.sh disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/fod-hash.sh"

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
stage
# Only the fixed-output dependency fetches run here; the native build and
# runtime checks run locally before hosts follow the revision (ADR-0003).
# cargoDeps itself is an ordinary derivation that links the vendored crates;
# its vendorStaging dependency is the fixed-output fetch that cargoHash pins.
cargo_hash=$(collect_fod_hash .#t3code.resourceMonitor.cargoDeps.vendorStaging cargoHash)
set_hash cargoHash "$cargo_hash"
pnpm_deps_hash=$(collect_fod_hash .#t3code.pnpmDeps pnpmDepsHash)
set_hash pnpmDepsHash "$pnpm_deps_hash"
scripts/ci/check-t3code-provider-versions.sh
success=true
version=$(jq -r .version "$work/target.json")
printf 'updated=true\nversion=%s\n' "$version" >>"${GITHUB_OUTPUT:-/dev/null}"
