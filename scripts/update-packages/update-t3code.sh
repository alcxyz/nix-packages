#!/usr/bin/env bash
# Tracks the latest stable T3 Code GitHub release, refreshes all fixed-output
# hashes, and validates the complete package with its embedded providers.
# Sets GITHUB_OUTPUT: updated, version.
set -euo pipefail

PKG_FILE="pkgs/t3code/default.nix"
FAKE_HASH="sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

clean_homeless_shelter() {
  for _ in {1..10}; do
    rm -rf /homeless-shelter
    sleep 1
    if [[ ! -e /homeless-shelter ]]; then
      sleep 1
      [[ ! -e /homeless-shelter ]] && return 0
    fi
  done

  echo "Unable to keep /homeless-shelter absent before a non-sandboxed Nix command." >&2
  return 1
}

current_version=$(grep -m1 'version = ' "$PKG_FILE" | grep -oP '"\K[^"]+' | head -1)
echo "Current: $current_version"

auth_args=()
if [[ -n "${GITHUB_TOKEN:-}" ]]; then
  auth_args=(-H "Authorization: Bearer ${GITHUB_TOKEN}")
fi

release_json=$(mktemp)
trap 'rm -f "$release_json"' EXIT

curl -fsSL "${auth_args[@]}" \
  -H "Accept: application/vnd.github+json" \
  "https://api.github.com/repos/pingdotgg/t3code/releases/latest" \
  -o "$release_json"

latest_version=$(
  python3 - "$release_json" <<'PYEOF'
import json
import re
import sys

tag = json.load(open(sys.argv[1]))["tag_name"]
match = re.fullmatch(r"v(\d+\.\d+\.\d+)", tag)
if not match:
    raise SystemExit(f"unexpected stable T3 Code release tag: {tag}")
print(match.group(1))
PYEOF
)

target_version=${T3CODE_VERSION:-$latest_version}
echo "Latest stable: $latest_version"
echo "Target:        $target_version"

if [[ "$current_version" == "$target_version" && "${FORCE_UPDATE:-false}" != "true" ]]; then
  echo "Already up to date — nothing to do."
  echo "updated=false" >>"$GITHUB_OUTPUT"
  exit 0
fi

src_url="https://github.com/pingdotgg/t3code/archive/refs/tags/v${target_version}.tar.gz"
echo "Fetching source hash..."
clean_homeless_shelter
src_hash=$(nix store prefetch-file --unpack --json "$src_url" 2>/dev/null |
  python3 -c 'import json,sys; print(json.load(sys.stdin)["hash"])')
echo "Source hash: $src_hash"

patch_package() {
  NEW_VERSION="$target_version" \
    SRC_HASH="$src_hash" \
    CARGO_HASH="$1" \
    PNPM_HASH="$2" \
    python3 - <<'PYEOF'
import os
import re

path = "pkgs/t3code/default.nix"
content = open(path).read()

content = re.sub(r'(version = )"[^"]+"', rf'\g<1>"{os.environ["NEW_VERSION"]}"', content, count=1)
content = re.sub(
    r'(tag = "v\$\{version\}";\n\s+hash = )"[^"]+"',
    rf'\g<1>"{os.environ["SRC_HASH"]}"',
    content,
    count=1,
)
content = re.sub(r'(cargoHash = )"[^"]+"', rf'\g<1>"{os.environ["CARGO_HASH"]}"', content, count=1)
content = re.sub(
    r'(fetcherVersion = 4;\n\s+hash = )"[^"]+"',
    rf'\g<1>"{os.environ["PNPM_HASH"]}"',
    content,
    count=1,
)

open(path, "w").write(content)
PYEOF
}

collect_hash() {
  local attr="$1"
  local label="$2"
  local log
  local hash

  log=$(mktemp)
  clean_homeless_shelter
  if nix build "$attr" --no-link 2>"$log"; then
    cat "$log" >&2
    rm -f "$log"
    echo "Expected ${label} build to fail with the placeholder hash" >&2
    exit 1
  fi

  cat "$log" >&2
  hash=$(sed -nE 's/^[[:space:]]*got:[[:space:]]+(sha256-[^[:space:]]+).*/\1/p' "$log" | tail -n1)
  rm -f "$log"

  if [[ ! "$hash" =~ ^sha256-.+ ]]; then
    echo "Unable to collect ${label} hash" >&2
    exit 1
  fi

  printf '%s\n' "$hash"
}

patch_package "$FAKE_HASH" "$FAKE_HASH"

echo "Computing resource monitor Cargo hash..."
cargo_hash=$(collect_hash .#t3code.resourceMonitor cargoHash)
echo "Cargo hash: $cargo_hash"
patch_package "$cargo_hash" "$FAKE_HASH"

echo "Computing pnpm dependency hash..."
pnpm_hash=$(collect_hash .#t3code.pnpmDeps pnpmDeps)
echo "pnpm hash: $pnpm_hash"
patch_package "$cargo_hash" "$pnpm_hash"

echo "Validating T3 Code and embedded provider closure..."
scripts/ci/verify-t3code-providers.sh

echo "updated=true" >>"$GITHUB_OUTPUT"
echo "version=$target_version" >>"$GITHUB_OUTPUT"
