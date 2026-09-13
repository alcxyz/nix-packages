#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
test_root=$(mktemp -d)
trap 'rm -rf "$test_root"' EXIT
mkdir -p "$test_root/bin" "$test_root/cache" "$test_root/runner-temp"

cat >"$test_root/bin/nix" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail

if [[ "$*" == "path-info --all" ]]; then
  printf '%s\n' /nix/store/existing
elif [[ "$*" == "path-info --json --all" ]]; then
  cat <<'JSON'
{
  "/nix/store/existing": {"ultimate": true},
  "/nix/store/fetched": {"ultimate": false},
  "/nix/store/locally-built": {"ultimate": true}
}
JSON
elif [[ " $* " == *" copy "* ]]; then
  printf '%s\n' "$*" >>"$CACHE_TEST_ROOT/copy-calls"
  printf 'nar' >"$CACHE_TEST_ROOT/cache/new.nar.zst"
else
  echo "Unexpected nix invocation: $*" >&2
  exit 99
fi
MOCK
chmod +x "$test_root/bin/nix"

printf 'StoreDir: /nix/store\n' >"$test_root/cache/nix-cache-info"
: >"$test_root/github-env"
: >"$test_root/github-output"
: >"$test_root/copy-calls"

common_env=(
  PATH="$test_root/bin:$PATH"
  CACHE_TEST_ROOT="$test_root"
  RUNNER_TEMP="$test_root/runner-temp"
  NIX_CI_CACHE_DIR="$test_root/cache"
  GITHUB_ENV="$test_root/github-env"
  GITHUB_OUTPUT="$test_root/github-output"
)

env "${common_env[@]}" \
  NIX_CONFIG='experimental-features = nix-command flakes' \
  "$repo_root/scripts/ci/nix-build-cache.sh" prepare

grep -Eq '^NIX_CONFIG<<NIX_CI_CACHE_CONFIG_[0-9]+$' "$test_root/github-env"
grep -Fqx 'experimental-features = nix-command flakes' "$test_root/github-env"
grep -Fqx "extra-substituters = file://$test_root/cache?trusted=true" "$test_root/github-env"
grep -Fqx 'fallback = true' "$test_root/github-env"
grep -Fqx /nix/store/existing "$test_root/runner-temp/nix-build-cache-before.txt"

env "${common_env[@]}" NIX_CI_CACHE_MAX_BYTES=1048576 \
  "$repo_root/scripts/ci/nix-build-cache.sh" save

grep -Fq -- '--to file://' "$test_root/copy-calls"
if grep -Fq -- '--no-recursive' "$test_root/copy-calls"; then
  echo "The cache export would create roots with missing references." >&2
  exit 1
fi
grep -Fq -- '?compression=zstd&compression-level=1 /nix/store/locally-built' "$test_root/copy-calls"
if grep -Fq '/nix/store/existing' "$test_root/copy-calls" ||
  grep -Fq '/nix/store/fetched' "$test_root/copy-calls"; then
  echo "The cache export included a pre-existing or substituted store path." >&2
  exit 1
fi
grep -Fqx 'save=true' "$test_root/github-output"
grep -Fqx 'path-count=1' "$test_root/github-output"

: >"$test_root/github-output"
env "${common_env[@]}" NIX_CI_CACHE_MAX_BYTES=1 \
  "$repo_root/scripts/ci/nix-build-cache.sh" save
grep -Fqx 'save=false' "$test_root/github-output"
