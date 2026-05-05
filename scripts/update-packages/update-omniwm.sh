#!/usr/bin/env bash
# .github/scripts/update-omniwm.sh
# Checks for a new OmniWM release, computes Nix SRI hash, and patches
# pkgs/omniwm/default.nix.  Sets GITHUB_OUTPUT: updated, version.
set -euo pipefail

PKG_FILE="pkgs/omniwm/default.nix"

# ── current version ────────────────────────────────────────────────────────────
current_version=$(grep -m1 'version = ' "$PKG_FILE" | grep -oP '"\K[^"]+')
echo "Current: $current_version"

# ── latest release ─────────────────────────────────────────────────────────────
api_response=$(curl -fsSL \
  -H "Accept: application/vnd.github+json" \
  ${GITHUB_TOKEN:+-H "Authorization: Bearer $GITHUB_TOKEN"} \
  "https://api.github.com/repos/BarutSRB/OmniWM/releases/latest")

latest_tag=$(python3 -c "import sys,json; print(json.loads(sys.stdin.read())['tag_name'])" <<< "$api_response")
latest_version="${latest_tag#v}"
echo "Latest:  $latest_version"

if [[ "$current_version" == "$latest_version" ]]; then
  echo "Already up to date — nothing to do."
  echo "updated=false" >> "$GITHUB_OUTPUT"
  exit 0
fi

# ── hash helper ────────────────────────────────────────────────────────────────
compute_sri() {
  local url="$1"
  local tmp
  tmp=$(mktemp)
  echo "  fetching: $url" >&2
  curl -fsSL -o "$tmp" "$url"
  local hex
  hex=$(sha256sum "$tmp" | awk '{print $1}')
  rm -f "$tmp"
  printf 'sha256-%s' "$(printf '%s' "$hex" | xxd -r -p | base64 -w0)"
}

validate_sri() {
  local name="$1"
  local hash="$2"
  if [[ "$hash" == "sha256-" || ! "$hash" =~ ^sha256-.+ ]]; then
    echo "Invalid SRI hash for ${name}: ${hash}" >&2
    exit 1
  fi
}

# ── download & hash ────────────────────────────────────────────────────────────
v="$latest_version"
darwin_hash=$(compute_sri \
  "https://github.com/BarutSRB/OmniWM/releases/download/v${v}/OmniWM-v${v}.zip")
validate_sri "OmniWM" "$darwin_hash"

echo "Darwin hash: $darwin_hash"

# ── patch default.nix ─────────────────────────────────────────────────────────
NEW_VERSION="$v" \
DARWIN_HASH="$darwin_hash" \
python3 - <<'PYEOF'
import os, re

path        = 'pkgs/omniwm/default.nix'
version     = os.environ['NEW_VERSION']
darwin_hash = os.environ['DARWIN_HASH']

content = open(path).read()

# version
content = re.sub(r'(version = )"[^"]+"', rf'\g<1>"{version}"', content)

# hash
content = re.sub(r'(hash = )"[^"]+"', rf'\g<1>"{darwin_hash}"', content)

open(path, 'w').write(content)
print(f"Patched {path} → {version}")
PYEOF

echo "updated=true"        >> "$GITHUB_OUTPUT"
echo "version=$v"          >> "$GITHUB_OUTPUT"
