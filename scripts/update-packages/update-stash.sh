#!/usr/bin/env bash
# scripts/update-packages/update-stash.sh
# Checks for a new Stash release, refreshes fixed-output hashes, and patches
# pkgs/stash/version.json.
# Sets GITHUB_OUTPUT: updated, version.
set -euo pipefail
# shellcheck source=scripts/update-packages/fod-hash.sh disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/fod-hash.sh"

VERSION_FILE="pkgs/stash/version.json"
FAKE_HASH=sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=

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

python3 - "$VERSION_FILE" "$latest_version" "$git_hash" "$src_hash" "$FAKE_HASH" <<'PYEOF'
import json
import sys

path, version, git_hash, src_hash, fake_hash = sys.argv[1:]
data = json.load(open(path))
data.update({
    "version": version,
    "gitHash": git_hash,
    "srcHash": src_hash,
    "pnpmHash": fake_hash,
    "pnpmHashV3": fake_hash,
    "vendorHash": fake_hash,
})
open(path, "w").write(json.dumps(data, indent=2) + "\n")
PYEOF

# Only the fixed-output dependency fetches run here. The Go module fetch does
# not depend on the compiled UI (overrideModAttrs in the package), so neither
# collection compiles anything; the native build runs locally (ADR-0003).
# Fetch the source first, so a wrong source hash fails here instead of being
# reported by the dependency fetches below.
realise_fetch_only .#stash.src "$src_hash"
for pair in 'pnpmHash frontendDepsV2' 'pnpmHashV3 frontendDepsV3'; do
  read -r pnpm_field fetcher <<<"$pair"
  pnpm_hash=$(collect_fod_hash ".#stash.$fetcher" "$pnpm_field")
  python3 - "$VERSION_FILE" "$pnpm_hash" "$pnpm_field" <<'PYEOF'
import json
import sys
path, value, field = sys.argv[1:]
data = json.load(open(path))
data[field] = value
open(path, "w").write(json.dumps(data, indent=2) + "\n")
PYEOF
  realise_fetch_only ".#stash.$fetcher" "$pnpm_hash"
done

vendor_hash=$(collect_fod_hash .#stash.goModules vendorHash)
python3 - "$VERSION_FILE" "$vendor_hash" <<'PYEOF'
import json
import sys

path, vendor_hash = sys.argv[1:]
data = json.load(open(path))
data["vendorHash"] = vendor_hash
open(path, "w").write(json.dumps(data, indent=2) + "\n")
PYEOF

nix eval --raw .#stash.drvPath >/dev/null

echo "updated=true" >> "$GITHUB_OUTPUT"
echo "version=$latest_version" >> "$GITHUB_OUTPUT"
