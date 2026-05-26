#!/usr/bin/env bash
# scripts/update-packages/update-codex-app-server.sh
# Checks for a new Codex release on GitHub, computes platform asset hashes,
# and patches pkgs/codex-app-server/default.nix.
# Sets GITHUB_OUTPUT: updated, version.
set -euo pipefail

PKG_FILE="pkgs/codex-app-server/default.nix"

current_version=$(grep -m1 'version = ' "$PKG_FILE" | grep -oP '"\K[^"]+' | head -1)
echo "Current: $current_version"

auth_args=()
if [ -n "${GITHUB_TOKEN:-}" ]; then
  auth_args=(-H "Authorization: Bearer ${GITHUB_TOKEN}")
fi

release_json=$(mktemp)
trap 'rm -f "$release_json"' EXIT

curl -fsSL "${auth_args[@]}" \
  -H "Accept: application/vnd.github+json" \
  "https://api.github.com/repos/openai/codex/releases/latest" \
  -o "$release_json"

latest_version=$(python3 - <<'PYEOF' "$release_json"
import json
import re
import sys

data = json.load(open(sys.argv[1]))
tag = data["tag_name"]
match = re.fullmatch(r"rust-v(.+)", tag)
if not match:
    raise SystemExit(f"unexpected Codex release tag: {tag}")
print(match.group(1))
PYEOF
)
echo "Latest:  $latest_version"

if [[ "$current_version" == "$latest_version" ]]; then
  echo "Already up to date — nothing to do."
  echo "updated=false" >> "$GITHUB_OUTPUT"
  exit 0
fi

tmp_hashes=$(mktemp)
trap 'rm -f "$release_json" "$tmp_hashes"' EXIT

python3 - <<'PYEOF' "$release_json" "$tmp_hashes"
import json
import sys

release_path, output_path = sys.argv[1:]
data = json.load(open(release_path))
assets = {asset["name"]: asset["browser_download_url"] for asset in data["assets"]}
targets = [
    "x86_64-unknown-linux-musl",
    "aarch64-unknown-linux-musl",
    "x86_64-apple-darwin",
    "aarch64-apple-darwin",
]

with open(output_path, "w") as out:
    for target in targets:
        name = f"codex-app-server-package-{target}.tar.gz"
        url = assets.get(name)
        if not url:
            raise SystemExit(f"missing release asset: {name}")
        print(f"{target}\t{url}", file=out)
PYEOF

declare -A target_hashes
while IFS=$'\t' read -r target url; do
  echo "Fetching hash for $target..."
  hash=$(nix store prefetch-file --json "$url" 2>/dev/null \
    | python3 -c "import sys,json; print(json.loads(sys.stdin.read())['hash'])")
  if [ -z "$hash" ]; then
    echo "Computed empty hash for $target" >&2
    exit 1
  fi
  target_hashes["$target"]="$hash"
  echo "$target: $hash"
done < "$tmp_hashes"

NEW_VERSION="$latest_version" \
X86_64_LINUX_HASH="${target_hashes[x86_64-unknown-linux-musl]}" \
AARCH64_LINUX_HASH="${target_hashes[aarch64-unknown-linux-musl]}" \
X86_64_DARWIN_HASH="${target_hashes[x86_64-apple-darwin]}" \
AARCH64_DARWIN_HASH="${target_hashes[aarch64-apple-darwin]}" \
python3 - <<'PYEOF'
import os
import re

path = "pkgs/codex-app-server/default.nix"
content = open(path).read()

content = re.sub(r'(version = )"[^"]+"', rf'\g<1>"{os.environ["NEW_VERSION"]}"', content)

replacements = {
    "x86_64-unknown-linux-musl": os.environ["X86_64_LINUX_HASH"],
    "aarch64-unknown-linux-musl": os.environ["AARCH64_LINUX_HASH"],
    "x86_64-apple-darwin": os.environ["X86_64_DARWIN_HASH"],
    "aarch64-apple-darwin": os.environ["AARCH64_DARWIN_HASH"],
}

for target, hash_value in replacements.items():
    content = re.sub(
        rf'(target = "{re.escape(target)}";\n\s+hash = )"[^"]+"',
        rf'\g<1>"{hash_value}"',
        content,
    )

open(path, "w").write(content)
print(f"Patched {path} -> {os.environ['NEW_VERSION']}")
PYEOF

echo "Validating codex-app-server derivation build..."
rm -rf /homeless-shelter
nix build .#codex-app-server --no-link

echo "updated=true" >> "$GITHUB_OUTPUT"
echo "version=$latest_version" >> "$GITHUB_OUTPUT"
