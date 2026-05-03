#!/usr/bin/env bash
# .github/scripts/update-ndrop.sh
# Checks for new commits on Schweber/ndrop main branch and patches
# pkgs/ndrop/default.nix.
# ndrop has no tagged releases — we track the latest commit on main.
# Sets GITHUB_OUTPUT: updated, version.
set -euo pipefail

PKG_FILE="pkgs/ndrop/default.nix"

# ── current rev ────────────────────────────────────────────────────────────────
current_rev=$(grep -m1 'rev = ' "$PKG_FILE" | grep -oP '"\K[^"]+')
echo "Current: $current_rev"

# ── latest commit on main ─────────────────────────────────────────────────────
latest_rev=$(curl -fsSL \
  -H "Accept: application/vnd.github+json" \
  ${GITHUB_TOKEN:+-H "Authorization: Bearer $GITHUB_TOKEN"} \
  "https://api.github.com/repos/Schweber/ndrop/commits/main" | python3 -c "import sys,json; print(json.loads(sys.stdin.read())['sha'])")
echo "Latest:  $latest_rev"

if [[ "$current_rev" == "$latest_rev" ]]; then
  echo "Already up to date — nothing to do."
  echo "updated=false" >> "$GITHUB_OUTPUT"
  exit 0
fi

# ── compute hash ───────────────────────────────────────────────────────────────
echo "  fetching archive..." >&2
hash=$(nix store prefetch-file --unpack --json \
  "https://github.com/Schweber/ndrop/archive/${latest_rev}.tar.gz" 2>/dev/null \
  | python3 -c "import sys,json; print(json.loads(sys.stdin.read())['hash'])")

echo "Hash: $hash"

# ── short rev for version string ──────────────────────────────────────────────
short_rev="${latest_rev:0:7}"

# ── patch default.nix ─────────────────────────────────────────────────────────
NEW_REV="$latest_rev" \
SHORT_REV="$short_rev" \
NEW_HASH="$hash" \
python3 - <<'PYEOF'
import os, re

path      = 'pkgs/ndrop/default.nix'
rev       = os.environ['NEW_REV']
short_rev = os.environ['SHORT_REV']
new_hash  = os.environ['NEW_HASH']

content = open(path).read()

# Update version string (short-rev-unstable)
content = re.sub(
    r'(version = )"[^"]+"',
    rf'\g<1>"{short_rev}-unstable"',
    content,
)

# Update rev
content = re.sub(
    r'(rev = )"[^"]+"',
    rf'\g<1>"{rev}"',
    content,
)

# Update hash
content = re.sub(
    r'(hash = )"[^"]+"',
    rf'\g<1>"{new_hash}"',
    content,
)

open(path, 'w').write(content)
print(f"Patched {path} → {short_rev}-unstable ({rev})")
PYEOF

echo "updated=true"          >> "$GITHUB_OUTPUT"
echo "version=${short_rev}"  >> "$GITHUB_OUTPUT"
