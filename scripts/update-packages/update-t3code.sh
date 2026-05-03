#!/usr/bin/env bash
# .github/scripts/update-t3code.sh
# Checks for a new t3code release, computes Nix SRI hashes, and patches
# pkgs/t3code/default.nix.  Sets GITHUB_OUTPUT: updated, version.
set -euo pipefail

PKG_FILE="pkgs/t3code/default.nix"

# ── current version ────────────────────────────────────────────────────────────
current_version=$(grep -m1 'version = ' "$PKG_FILE" | grep -oP '"\K[^"]+')
echo "Current: $current_version"

# ── latest release ─────────────────────────────────────────────────────────────
api_response=$(curl -fsSL \
  -H "Accept: application/vnd.github+json" \
  ${GITHUB_TOKEN:+-H "Authorization: Bearer $GITHUB_TOKEN"} \
  "https://api.github.com/repos/pingdotgg/t3code/releases/latest")

latest_tag=$(python3 -c "import sys,json; print(json.loads(sys.stdin.read())['tag_name'])" <<< "$api_response")
latest_version="${latest_tag#v}"
echo "Latest:  $latest_version"

if [[ "$current_version" == "$latest_version" ]]; then
  echo "Already up to date — nothing to do."
  echo "updated=false" >> "$GITHUB_OUTPUT"
  exit 0
fi

# ── hash helper ────────────────────────────────────────────────────────────────
# Downloads a URL to a temp file and returns the Nix SRI sha256 hash.
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

# ── download & hash ────────────────────────────────────────────────────────────
v="$latest_version"
linux_hash=$(compute_sri \
  "https://github.com/pingdotgg/t3code/releases/download/v${v}/T3-Code-${v}-x86_64.AppImage")
darwin_hash=$(compute_sri \
  "https://github.com/pingdotgg/t3code/releases/download/v${v}/T3-Code-${v}-arm64.dmg")

echo "Linux hash:  $linux_hash"
echo "Darwin hash: $darwin_hash"

# ── patch default.nix ─────────────────────────────────────────────────────────
# In t3code/default.nix the url and hash are on consecutive lines inside each
# fetchurl block, so the regex can anchor on the unique URL suffix.
NEW_VERSION="$v" \
LINUX_HASH="$linux_hash" \
DARWIN_HASH="$darwin_hash" \
python3 - <<'PYEOF'
import os, re

path        = 'pkgs/t3code/default.nix'
version     = os.environ['NEW_VERSION']
linux_hash  = os.environ['LINUX_HASH']
darwin_hash = os.environ['DARWIN_HASH']

content = open(path).read()

# version
content = re.sub(r'(version = )"[^"]+"', rf'\g<1>"{version}"', content)

def replace_hash(url_suffix, new_hash, text):
    """Replace the hash on the line immediately after the url containing suffix."""
    def sub(m):
        return m.group(1) + f'"{new_hash}"'
    return re.sub(
        rf'(url = "[^"]*{re.escape(url_suffix)}";\n\s+hash = )"[^"]+"',
        sub,
        text,
    )

content = replace_hash('x86_64.AppImage', linux_hash,  content)
content = replace_hash('arm64.dmg',       darwin_hash, content)

open(path, 'w').write(content)
print(f"Patched {path} → {version}")
PYEOF

echo "updated=true"        >> "$GITHUB_OUTPUT"
echo "version=$v"          >> "$GITHUB_OUTPUT"
