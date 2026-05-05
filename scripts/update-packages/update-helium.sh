#!/usr/bin/env bash
# .github/scripts/update-helium.sh
# Checks for a new helium release (helium-linux is the source of truth),
# computes Nix SRI hashes for all four platform assets, and patches
# pkgs/helium/default.nix.  Sets GITHUB_OUTPUT: updated, version.
#
# Assets that do not exist on a given release return lib.fakeHash so the
# nix file stays valid; real hashes replace fakeHash as soon as upstream
# publishes the asset.
set -euo pipefail

PKG_FILE="pkgs/helium/default.nix"

# ── current version ────────────────────────────────────────────────────────────
current_version=$(grep -m1 'version = ' "$PKG_FILE" | grep -oP '"\K[^"]+')
echo "Current: $current_version"

# ── latest release (helium-linux is canonical) ────────────────────────────────
api_response=$(curl -fsSL \
  -H "Accept: application/vnd.github+json" \
  ${GITHUB_TOKEN:+-H "Authorization: Bearer $GITHUB_TOKEN"} \
  "https://api.github.com/repos/imputnet/helium-linux/releases/latest")

latest_tag=$(python3 -c "import sys,json; print(json.loads(sys.stdin.read())['tag_name'])" <<< "$api_response")
# helium tags are bare version strings (no leading "v")
latest_version="${latest_tag#v}"
echo "Latest:  $latest_version"

if [[ "$current_version" == "$latest_version" ]]; then
  echo "Already up to date — nothing to do."
  echo "updated=false" >> "$GITHUB_OUTPUT"
  exit 0
fi

# ── hash helper ────────────────────────────────────────────────────────────────
# Returns the Nix SRI hash for a URL, or the literal string "lib.fakeHash" if
# the asset does not exist (404 / any non-200 response).
compute_sri() {
  local url="$1"
  local tmp
  tmp=$(mktemp)
  echo "  fetching: $url" >&2
  if ! curl -fsSL -o "$tmp" "$url" 2>/dev/null; then
    echo "  → asset not found, will use lib.fakeHash" >&2
    rm -f "$tmp"
    printf 'lib.fakeHash'
    return
  fi
  local hex
  hex=$(sha256sum "$tmp" | awk '{print $1}')
  rm -f "$tmp"
  printf 'sha256-%s' "$(printf '%s' "$hex" | xxd -r -p | base64 -w0)"
}

validate_sri() {
  local name="$1"
  local hash="$2"
  if [[ "$hash" == "lib.fakeHash" ]]; then
    return
  fi
  if [[ "$hash" == "sha256-" || ! "$hash" =~ ^sha256-.+ ]]; then
    echo "Invalid SRI hash for ${name}: ${hash}" >&2
    exit 1
  fi
}

# ── download & hash all four platform assets ──────────────────────────────────
v="$latest_version"

hash_linux_x86=$(compute_sri \
  "https://github.com/imputnet/helium-linux/releases/download/${v}/helium-${v}-x86_64.AppImage")
hash_linux_arm=$(compute_sri \
  "https://github.com/imputnet/helium-linux/releases/download/${v}/helium_${v}_arm64.AppImage")
hash_darwin_arm=$(compute_sri \
  "https://github.com/imputnet/helium-macos/releases/download/${v}/helium_${v}_arm64-macos.dmg")
hash_darwin_x86=$(compute_sri \
  "https://github.com/imputnet/helium-macos/releases/download/${v}/helium_${v}_x86_64-macos.dmg")

validate_sri "helium linux x86_64" "$hash_linux_x86"
validate_sri "helium linux aarch64" "$hash_linux_arm"
validate_sri "helium darwin aarch64" "$hash_darwin_arm"
validate_sri "helium darwin x86_64" "$hash_darwin_x86"

echo "Linux x86_64  hash: $hash_linux_x86"
echo "Linux aarch64 hash: $hash_linux_arm"
echo "Darwin arm64  hash: $hash_darwin_arm"
echo "Darwin x86_64 hash: $hash_darwin_x86"

# ── patch default.nix ─────────────────────────────────────────────────────────
# helium/default.nix has a two-line url attribute followed by a blank line,
# two comment lines (hex SHA256), then the hash.  We use a non-greedy dotall
# match anchored on the unique URL suffix to reach the correct hash line.
NEW_VERSION="$v" \
HASH_LINUX_X86="$hash_linux_x86" \
HASH_LINUX_ARM="$hash_linux_arm" \
HASH_DARWIN_ARM="$hash_darwin_arm" \
HASH_DARWIN_X86="$hash_darwin_x86" \
python3 - <<'PYEOF'
import os, re, base64, binascii

path = 'pkgs/helium/default.nix'
content = open(path).read()

version         = os.environ['NEW_VERSION']
hash_linux_x86  = os.environ['HASH_LINUX_X86']
hash_linux_arm  = os.environ['HASH_LINUX_ARM']
hash_darwin_arm = os.environ['HASH_DARWIN_ARM']
hash_darwin_x86 = os.environ['HASH_DARWIN_X86']

def sri_to_hex(sri: str) -> str:
    """Convert sha256-<b64> SRI hash to lowercase hex, or 'unknown'."""
    if sri == 'lib.fakeHash':
        return 'unknown'
    raw = base64.b64decode(sri.removeprefix('sha256-'))
    return binascii.hexlify(raw).decode()

def nix_hash_expr(h: str) -> str:
    """Return the Nix expression for a hash value."""
    return 'lib.fakeHash' if h == 'lib.fakeHash' else f'"{h}"'

# Update version
content = re.sub(r'(version = )"[^"]+"', rf'\g<1>"{version}"', content)

# Each (url_suffix, hash_value) pair
platforms = [
    ('x86_64.AppImage',  hash_linux_x86),
    ('arm64.AppImage',   hash_linux_arm),
    ('arm64-macos.dmg',  hash_darwin_arm),
    ('x86_64-macos.dmg', hash_darwin_x86),
]

for suffix, h in platforms:
    hex_h     = sri_to_hex(h)
    hash_expr = nix_hash_expr(h)

    # 1. Update the "# <hex>" comment line that sits before the hash
    content = re.sub(
        rf'(url\s*=\s*\n?\s*"[^"]*{re.escape(suffix)}";\s*\n\s*\n\s*# Upstream \(hex\) SHA256:\s*\n\s*# )[0-9a-f]+',
        rf'\g<1>{hex_h}',
        content,
    )

    # 2. Update the hash value itself (handles both quoted SRI and lib.fakeHash)
    def make_replacer(group1_suffix):
        def replacer(m):
            return m.group(1) + group1_suffix
        return replacer

    content = re.sub(
        rf'(url\s*=\s*\n?\s*"[^"]*{re.escape(suffix)}";[\s\S]*?hash\s*=\s*)(?:"[^"]*"|lib\.fakeHash)',
        make_replacer(hash_expr),
        content,
    )

open(path, 'w').write(content)
print(f"Patched {path} → {version}")
PYEOF

echo "updated=true"  >> "$GITHUB_OUTPUT"
echo "version=$v"    >> "$GITHUB_OUTPUT"
