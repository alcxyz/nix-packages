#!/usr/bin/env bash
# .github/scripts/update-claude-code.sh
# Checks for a new claude-code release on npm, regenerates package-lock.json,
# computes Nix SRI hashes, and patches pkgs/claude-code/default.nix.
# Sets GITHUB_OUTPUT: updated, version.
set -euo pipefail

PKG_DIR="pkgs/claude-code"
PKG_FILE="$PKG_DIR/default.nix"

# ── current version ────────────────────────────────────────────────────────────
current_version=$(grep -m1 'version = ' "$PKG_FILE" | grep -oP '"\K[^"]+' | head -1)
echo "Current: $current_version"

# ── latest version from npm ───────────────────────────────────────────────────
latest_version=$(curl -fsSL "https://registry.npmjs.org/@anthropic-ai/claude-code/latest" \
  | python3 -c "import sys,json; print(json.loads(sys.stdin.read())['version'])")
echo "Latest:  $latest_version"

if [[ "$current_version" == "$latest_version" ]]; then
  echo "Already up to date — nothing to do."
  echo "updated=false" >> "$GITHUB_OUTPUT"
  exit 0
fi

# ── compute source hash (fetchzip) ────────────────────────────────────────────
tgz_url="https://registry.npmjs.org/@anthropic-ai/claude-code/-/claude-code-${latest_version}.tgz"
echo "Fetching source hash..."
src_hash=$(nix store prefetch-file --unpack --json "$tgz_url" 2>/dev/null \
  | python3 -c "import sys,json; print(json.loads(sys.stdin.read())['hash'])")
echo "Source hash: $src_hash"

# ── regenerate package-lock.json ──────────────────────────────────────────────
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

curl -fsSL -o "$tmp/package.tgz" "$tgz_url"
tar xzf "$tmp/package.tgz" -C "$tmp"
cd "$tmp/package"
npm install --package-lock-only --ignore-scripts 2>/dev/null
cp package-lock.json "$OLDPWD/$PKG_DIR/package-lock.json"
cd "$OLDPWD"
echo "Regenerated package-lock.json"

# ── compute npmDepsHash ───────────────────────────────────────────────────────
echo "Computing npm deps hash..."
npm_deps_hash=$(nix-shell -p prefetch-npm-deps --run "prefetch-npm-deps $PKG_DIR/package-lock.json 2>/dev/null")
echo "npm deps hash: $npm_deps_hash"

# ── patch default.nix ────────────────────────────────────────────────────────
NEW_VERSION="$latest_version" \
SRC_HASH="$src_hash" \
NPM_DEPS_HASH="$npm_deps_hash" \
python3 - <<'PYEOF'
import os, re

path          = 'pkgs/claude-code/default.nix'
version       = os.environ['NEW_VERSION']
src_hash      = os.environ['SRC_HASH']
npm_deps_hash = os.environ['NPM_DEPS_HASH']

content = open(path).read()

# Update version
content = re.sub(r'(version = )"[^"]+"', rf'\g<1>"{version}"', content)

# Update fetchzip hash
content = re.sub(
    r'(url = "https://registry\.npmjs\.org/@anthropic-ai/claude-code/-/claude-code-\$\{finalAttrs\.version\}\.tgz";\n\s+hash = )"[^"]+"',
    rf'\g<1>"{src_hash}"',
    content,
)

# Update npmDepsHash
content = re.sub(r'(npmDepsHash = )"[^"]+"', rf'\g<1>"{npm_deps_hash}"', content)

open(path, 'w').write(content)
print(f"Patched {path} → {version}")
PYEOF

echo "updated=true"                >> "$GITHUB_OUTPUT"
echo "version=$latest_version"     >> "$GITHUB_OUTPUT"
