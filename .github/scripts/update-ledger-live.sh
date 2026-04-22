#!/usr/bin/env bash
# .github/scripts/update-ledger-live.sh
# Checks for a new Ledger Live release via Ledger's CDN metadata,
# computes Nix SRI hashes for Linux AppImage and macOS DMG, and patches
# pkgs/ledger-live/default.nix.  Sets GITHUB_OUTPUT: updated, version.
set -euo pipefail

PKG_FILE="pkgs/ledger-live/default.nix"

# ── current version ────────────────────────────────────────────────────────────
current_version=$(grep -m1 'version = ' "$PKG_FILE" | grep -oP '"\K[^"]+' | head -1)
echo "Current: $current_version"

# ── latest release (electron-builder metadata on Ledger CDN) ─────────────────
latest_version=$(curl -fsSL "https://download.live.ledger.com/latest-linux.yml" \
  | grep -m1 '^version:' | awk '{print $2}' | tr -d '\r')
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
  if ! curl -fsSL -o "$tmp" "$url" 2>/dev/null; then
    echo "  → asset not found" >&2
    rm -f "$tmp"
    return 1
  fi
  local hex
  hex=$(sha256sum "$tmp" | awk '{print $1}')
  rm -f "$tmp"
  printf 'sha256-%s' "$(python3 -c "import base64,binascii; print(base64.b64encode(binascii.unhexlify('$hex')).decode(), end='')")"
}

# ── download & hash both platform assets ─────────────────────────────────────
v="$latest_version"

hash_linux_x86=$(compute_sri \
  "https://download.live.ledger.com/ledger-live-desktop-${v}-linux-x86_64.AppImage")
hash_darwin=$(compute_sri \
  "https://download.live.ledger.com/ledger-live-desktop-${v}-mac.dmg")

echo "Linux x86_64 hash: $hash_linux_x86"
echo "Darwin hash:       $hash_darwin"

# ── patch default.nix ─────────────────────────────────────────────────────────
NEW_VERSION="$v" \
HASH_LINUX_X86="$hash_linux_x86" \
HASH_DARWIN="$hash_darwin" \
python3 - <<'PYEOF'
import os, re, base64, binascii

path = 'pkgs/ledger-live/default.nix'
content = open(path).read()

version        = os.environ['NEW_VERSION']
hash_linux_x86 = os.environ['HASH_LINUX_X86']
hash_darwin    = os.environ['HASH_DARWIN']

def sri_to_hex(sri: str) -> str:
    raw = base64.b64decode(sri.removeprefix('sha256-'))
    return binascii.hexlify(raw).decode()

# Update version
content = re.sub(r'(version = )"[^"]+"', rf'\g<1>"{version}"', content)

# Each (url_suffix, hash_value) pair
platforms = [
    ('x86_64.AppImage', hash_linux_x86),
    ('mac.dmg',         hash_darwin),
]

for suffix, h in platforms:
    hex_h = sri_to_hex(h)

    # 1. Update the hex SHA256 comment
    content = re.sub(
        rf'(url\s*=\s*\n?\s*"[^"]*{re.escape(suffix)}";\s*\n\s*\n\s*# Upstream \(hex\) SHA256:\s*\n\s*# )[0-9a-f]+',
        rf'\g<1>{hex_h}',
        content,
    )

    # 2. Update the hash value
    content = re.sub(
        rf'(url\s*=\s*\n?\s*"[^"]*{re.escape(suffix)}";[\s\S]*?hash\s*=\s*)"[^"]*"',
        rf'\g<1>"{h}"',
        content,
    )

open(path, 'w').write(content)
print(f"Patched {path} → {version}")
PYEOF

echo "updated=true"  >> "$GITHUB_OUTPUT"
echo "version=$v"    >> "$GITHUB_OUTPUT"
