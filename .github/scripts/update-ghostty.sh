#!/usr/bin/env bash
# .github/scripts/update-ghostty.sh
# Checks for a new Ghostty release and computes the Nix SRI hash for the
# macOS universal DMG, then patches pkgs/ghostty/default.nix.
# Sets GITHUB_OUTPUT: updated, version.
set -euo pipefail

PKG_FILE="pkgs/ghostty/default.nix"

# ── current version ────────────────────────────────────────────────────────────
current_version=$(grep -m1 'version = ' "$PKG_FILE" | grep -oP '"\K[^"]+')
echo "Current: $current_version"

# ── latest release ─────────────────────────────────────────────────────────────
api_response=$(curl -fsSL \
  -H "Accept: application/vnd.github+json" \
  ${GITHUB_TOKEN:+-H "Authorization: Bearer $GITHUB_TOKEN"} \
  "https://api.github.com/repos/ghostty-org/ghostty/releases/latest")

latest_tag=$(python3 -c "import sys,json; print(json.loads(sys.stdin.read())['tag_name'])" <<< "$api_response")
latest_version="${latest_tag#v}"
echo "Latest:  $latest_version"

if [[ "$current_version" == "$latest_version" ]]; then
  echo "Already up to date — nothing to do."
  echo "updated=false" >> "$GITHUB_OUTPUT"
  exit 0
fi

# ── download & hash ────────────────────────────────────────────────────────────
v="$latest_version"
url="https://release.files.ghostty.org/${v}/Ghostty.dmg"

echo "  fetching: $url"
tmp=$(mktemp)
curl -fsSL -o "$tmp" "$url"
hex=$(sha256sum "$tmp" | awk '{print $1}')
rm -f "$tmp"
hash_darwin="sha256-$(printf '%s' "$hex" | xxd -r -p | base64 -w0)"

echo "macOS universal hash: $hash_darwin"

# ── patch default.nix ─────────────────────────────────────────────────────────
NEW_VERSION="$v" \
HASH_DARWIN="$hash_darwin" \
python3 - <<'PYEOF'
import os, re, base64, binascii

path    = 'pkgs/ghostty/default.nix'
version = os.environ['NEW_VERSION']
h       = os.environ['HASH_DARWIN']

content = open(path).read()

hex_h = binascii.hexlify(base64.b64decode(h.removeprefix('sha256-'))).decode()

content = re.sub(r'(version = )"[^"]+"', rf'\g<1>"{version}"', content)

# Update hex comment
content = re.sub(
    r'(# Upstream \(hex\) SHA256:\s*\n\s*# )[0-9a-f]+',
    rf'\g<1>{hex_h}',
    content,
)

# Update hash value
content = re.sub(
    r'(url = "[^"]*Ghostty\.dmg";\s*\n(?:.*\n)*?\s*hash\s*=\s*)"[^"]+"',
    lambda m: m.group(1) + f'"{h}"',
    content,
)

open(path, 'w').write(content)
print(f"Patched {path} → {version}")
PYEOF

echo "updated=true"  >> "$GITHUB_OUTPUT"
echo "version=$v"    >> "$GITHUB_OUTPUT"
