#!/usr/bin/env bash
# .github/scripts/update-zen-browser.sh
# Checks for a new Zen Browser release, computes the Nix SRI hash, and patches
# pkgs/zen-browser/default.nix.
# Sets GITHUB_OUTPUT: updated, version.
set -euo pipefail

PKG_FILE="pkgs/zen-browser/default.nix"

# ── current version ────────────────────────────────────────────────────────────
current_version=$(grep -m1 'version = ' "$PKG_FILE" | grep -oP '"\K[^"]+')
echo "Current: $current_version"

# ── latest release ─────────────────────────────────────────────────────────────
# Zen has a rolling "twilight" prerelease tag — skip it and any other prereleases.
api_response=$(curl -fsSL \
  -H "Accept: application/vnd.github+json" \
  ${GITHUB_TOKEN:+-H "Authorization: Bearer $GITHUB_TOKEN"} \
  "https://api.github.com/repos/zen-browser/desktop/releases?per_page=20")

latest_tag=$(python3 -c "
import sys, json
releases = json.loads(sys.stdin.read())
for r in releases:
    if not r['prerelease'] and 'twilight' not in r['tag_name']:
        print(r['tag_name'])
        break
" <<< "$api_response")
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

# ── download & hash ────────────────────────────────────────────────────────────
v="$latest_version"
linux_hash=$(compute_sri \
  "https://github.com/zen-browser/desktop/releases/download/${v}/zen.linux-x86_64.tar.xz")

echo "Linux x86_64 hash: $linux_hash"

# ── patch default.nix ─────────────────────────────────────────────────────────
NEW_VERSION="$v" \
LINUX_HASH="$linux_hash" \
python3 - <<'PYEOF'
import os, re, base64, binascii

path    = 'pkgs/zen-browser/default.nix'
version = os.environ['NEW_VERSION']
h       = os.environ['LINUX_HASH']

content = open(path).read()

hex_h = binascii.hexlify(base64.b64decode(h.removeprefix('sha256-'))).decode()

# Update version
content = re.sub(r'(version = )"[^"]+"', rf'\g<1>"{version}"', content)

# Update hex comment
content = re.sub(
    r'(# Upstream \(hex\) SHA256:\s*\n\s*# )[0-9a-f]+',
    rf'\g<1>{hex_h}',
    content,
)

# Update hash value (anchored on the unique URL suffix)
content = re.sub(
    r'(url = "[^"]*zen\.linux-x86_64\.tar\.xz";\s*\n(?:.*\n)*?\s*hash\s*=\s*)"[^"]+"',
    lambda m: m.group(1) + f'"{h}"',
    content,
)

open(path, 'w').write(content)
print(f"Patched {path} → {version}")
PYEOF

echo "updated=true"  >> "$GITHUB_OUTPUT"
echo "version=$v"    >> "$GITHUB_OUTPUT"
