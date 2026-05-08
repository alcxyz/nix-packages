#!/usr/bin/env bash
# scripts/update-packages/update-stash.sh
# Checks for a new Stash release, refreshes fixed-output hashes, and patches
# pkgs/stash/version.json.
# Sets GITHUB_OUTPUT: updated, version.
set -euo pipefail

VERSION_FILE="pkgs/stash/version.json"

current_version=$(python3 -c 'import json; print(json.load(open("'"$VERSION_FILE"'"))["version"])')
echo "Current: $current_version"

api_response=$(curl -fsSL \
  -H "Accept: application/vnd.github+json" \
  ${GITHUB_TOKEN:+-H "Authorization: Bearer $GITHUB_TOKEN"} \
  "https://api.github.com/repos/stashapp/stash/releases/latest")

latest_tag=$(python3 -c 'import json,sys; print(json.load(sys.stdin)["tag_name"])' <<< "$api_response")
latest_version="${latest_tag#v}"
echo "Latest:  $latest_version"

if [[ "$current_version" == "$latest_version" ]]; then
  echo "Already up to date - nothing to do."
  echo "updated=false" >> "$GITHUB_OUTPUT"
  exit 0
fi

git_hash=$(git ls-remote https://github.com/stashapp/stash.git "refs/tags/v${latest_version}" | awk '{print substr($1, 1, 8)}')
if [[ -z "$git_hash" ]]; then
  echo "Unable to resolve git hash for v${latest_version}" >&2
  exit 1
fi

src_hash=$(nix-prefetch-url --unpack \
  "https://github.com/stashapp/stash/archive/refs/tags/v${latest_version}.tar.gz")
src_hash=$(nix hash convert --hash-algo sha256 --to sri "$src_hash")

python3 - "$VERSION_FILE" "$latest_version" "$git_hash" "$src_hash" <<'PYEOF'
import json
import sys

path, version, git_hash, src_hash = sys.argv[1:]
data = json.load(open(path))
data.update({
    "version": version,
    "gitHash": git_hash,
    "srcHash": src_hash,
    "pnpmHash": "",
    "vendorHash": "",
})
open(path, "w").write(json.dumps(data, indent=2) + "\n")
PYEOF

collect_hash() {
  local needle="$1"
  local log
  log=$(mktemp)
  if nix build .#stash --no-link --keep-going 2>"$log"; then
    cat "$log" >&2
    rm -f "$log"
    echo "Expected nix build to fail while collecting ${needle}" >&2
    exit 1
  fi
  local hash
  hash=$(grep -E 'got:[[:space:]]+sha256-' "$log" | tail -n1 | sed -E 's/.*got:[[:space:]]+(sha256-[^[:space:]]+).*/\1/')
  cat "$log" >&2
  rm -f "$log"
  if [[ -z "$hash" ]]; then
    echo "Unable to collect ${needle}" >&2
    exit 1
  fi
  printf '%s\n' "$hash"
}

pnpm_hash=$(collect_hash pnpmHash)
python3 - "$VERSION_FILE" "$pnpm_hash" <<'PYEOF'
import json
import sys

path, pnpm_hash = sys.argv[1:]
data = json.load(open(path))
data["pnpmHash"] = pnpm_hash
open(path, "w").write(json.dumps(data, indent=2) + "\n")
PYEOF

vendor_hash=$(collect_hash vendorHash)
python3 - "$VERSION_FILE" "$vendor_hash" <<'PYEOF'
import json
import sys

path, vendor_hash = sys.argv[1:]
data = json.load(open(path))
data["vendorHash"] = vendor_hash
open(path, "w").write(json.dumps(data, indent=2) + "\n")
PYEOF

nix build .#stash --no-link

echo "updated=true" >> "$GITHUB_OUTPUT"
echo "version=$latest_version" >> "$GITHUB_OUTPUT"
