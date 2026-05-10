#!/usr/bin/env bash
# Checks for a new kdash release, computes Nix SRI hashes for all packaged
# upstream binary assets, and patches pkgs/kdash/default.nix.
# Sets GITHUB_OUTPUT: updated, version.
set -euo pipefail

PKG_FILE="pkgs/kdash/default.nix"

current_version=$(sed -n 's/.*version = "\([^"]*\)".*/\1/p' "$PKG_FILE" | head -n1)
echo "Current: $current_version"

api_response=$(curl -fsSL \
  -H "Accept: application/vnd.github+json" \
  ${GITHUB_TOKEN:+-H "Authorization: Bearer $GITHUB_TOKEN"} \
  "https://api.github.com/repos/kdash-rs/kdash/releases/latest")

latest_tag=$(python3 -c "import sys,json; print(json.loads(sys.stdin.read())['tag_name'])" <<< "$api_response")
latest_version="${latest_tag#v}"
echo "Latest:  $latest_version"

if [[ "$current_version" == "$latest_version" ]]; then
  echo "Already up to date — nothing to do."
  echo "updated=false" >> "$GITHUB_OUTPUT"
  exit 0
fi

compute_sri() {
  local url="$1"
  local tmp
  tmp=$(mktemp)
  echo "  fetching: $url" >&2
  curl -fsSL -o "$tmp" "$url"
  python3 - "$tmp" <<'PYEOF'
import base64
import hashlib
import sys

with open(sys.argv[1], "rb") as f:
    print("sha256-" + base64.b64encode(hashlib.sha256(f.read()).digest()).decode())
PYEOF
  rm -f "$tmp"
}

validate_sri() {
  local name="$1"
  local hash="$2"
  if [[ "$hash" == "sha256-" || ! "$hash" =~ ^sha256-.+ ]]; then
    echo "Invalid SRI hash for ${name}: ${hash}" >&2
    exit 1
  fi
}

v="$latest_version"

hash_linux_x86=$(compute_sri \
  "https://github.com/kdash-rs/kdash/releases/download/v${v}/kdash-linux-musl.tar.gz")
hash_linux_arm=$(compute_sri \
  "https://github.com/kdash-rs/kdash/releases/download/v${v}/kdash-aarch64-musl.tar.gz")
hash_darwin_x86=$(compute_sri \
  "https://github.com/kdash-rs/kdash/releases/download/v${v}/kdash-macos.tar.gz")
hash_darwin_arm=$(compute_sri \
  "https://github.com/kdash-rs/kdash/releases/download/v${v}/kdash-macos-arm64.tar.gz")

validate_sri "kdash linux x86_64" "$hash_linux_x86"
validate_sri "kdash linux aarch64" "$hash_linux_arm"
validate_sri "kdash darwin x86_64" "$hash_darwin_x86"
validate_sri "kdash darwin aarch64" "$hash_darwin_arm"

echo "Linux x86_64  hash: $hash_linux_x86"
echo "Linux aarch64 hash: $hash_linux_arm"
echo "Darwin x86_64 hash: $hash_darwin_x86"
echo "Darwin arm64  hash: $hash_darwin_arm"

NEW_VERSION="$v" \
HASH_LINUX_X86="$hash_linux_x86" \
HASH_LINUX_ARM="$hash_linux_arm" \
HASH_DARWIN_X86="$hash_darwin_x86" \
HASH_DARWIN_ARM="$hash_darwin_arm" \
python3 - <<'PYEOF'
import os
import re

path = "pkgs/kdash/default.nix"
content = open(path).read()

version = os.environ["NEW_VERSION"]
hashes = {
    "kdash-linux-musl": os.environ["HASH_LINUX_X86"],
    "kdash-aarch64-musl": os.environ["HASH_LINUX_ARM"],
    "kdash-macos": os.environ["HASH_DARWIN_X86"],
    "kdash-macos-arm64": os.environ["HASH_DARWIN_ARM"],
}

content = re.sub(r'(version = )"[^"]+"', rf'\g<1>"{version}"', content)

for artifact, hash_value in hashes.items():
    content = re.sub(
        rf'(artifact = "{re.escape(artifact)}";\s*\n\s*hash = )"[^"]+"',
        rf'\g<1>"{hash_value}"',
        content,
    )

open(path, "w").write(content)
print(f"Patched {path} -> {version}")
PYEOF

echo "updated=true" >> "$GITHUB_OUTPUT"
echo "version=$v" >> "$GITHUB_OUTPUT"
