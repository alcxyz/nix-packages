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
  if [[ ${CACHE_BUDGET_FIXTURE:-false} == true ]]; then
    cat <<'JSON'
{
  "/nix/store/existing": {
    "ultimate": true,
    "ca": "fixed:r:sha256:existing",
    "deriver": "/nix/store/existing.drv",
    "references": [],
    "narSize": 1
  },
  "/nix/store/oversized": {
    "ultimate": true,
    "ca": "fixed:r:sha256:oversized",
    "deriver": "/nix/store/oversized.drv",
    "references": [],
    "narSize": 2000000
  },
  "/nix/store/large": {
    "ultimate": true,
    "ca": "fixed:r:sha256:large",
    "deriver": "/nix/store/large.drv",
    "references": [],
    "narSize": 700000
  },
  "/nix/store/medium": {
    "ultimate": true,
    "ca": "fixed:r:sha256:medium",
    "deriver": "/nix/store/medium.drv",
    "references": [],
    "narSize": 400000
  },
  "/nix/store/small": {
    "ultimate": true,
    "ca": "fixed:r:sha256:small",
    "deriver": "/nix/store/small.drv",
    "references": [],
    "narSize": 300000
  },
  "/nix/store/referenced-small": {
    "ultimate": true,
    "ca": "fixed:r:sha256:referenced-small",
    "deriver": "/nix/store/referenced-small.drv",
    "references": ["/nix/store/runtime-dependency"],
    "narSize": 1
  }
}
JSON
  else
  cat <<'JSON'
{
  "/nix/store/existing": {
    "ultimate": true,
    "ca": "fixed:r:sha256:existing",
    "deriver": "/nix/store/existing.drv",
    "references": [],
    "narSize": 1
  },
  "/nix/store/fetched-fixed-output": {
    "ultimate": false,
    "ca": "fixed:r:sha256:fetched",
    "deriver": "/nix/store/fetched.drv",
    "references": [],
    "narSize": 1
  },
  "/nix/store/normal-built-output": {
    "ultimate": true,
    "ca": null,
    "deriver": "/nix/store/normal.drv",
    "references": [],
    "narSize": 1
  },
  "/nix/store/fixed-output-dependency": {
    "ultimate": true,
    "ca": "fixed:r:sha256:dependency",
    "deriver": "/nix/store/dependency.drv",
    "references": [],
    "narSize": 1
  },
  "/nix/store/direct-source": {
    "ultimate": true,
    "ca": "fixed:r:sha256:source",
    "deriver": null,
    "references": [],
    "narSize": 1
  },
  "/nix/store/referenced-content-addressed-output": {
    "ultimate": true,
    "ca": "fixed:r:sha256:referenced",
    "deriver": "/nix/store/referenced.drv",
    "references": ["/nix/store/runtime-dependency"],
    "narSize": 1
  }
}
JSON
  fi
elif [[ " $* " == *" copy "* ]]; then
  printf '%s\n' "$*" >>"$CACHE_TEST_ROOT/copy-calls"
  cache_uri=
  while (($# > 0)); do
    if [[ $1 == --to ]]; then
      cache_uri=$2
      break
    fi
    shift
  done
  [[ -n $cache_uri ]] || exit 97
  cache_path=${cache_uri#file://}
  cache_path=${cache_path%%\?*}
  printf 'nar' >"$cache_path/new.nar.zst"
  if [[ ${FAIL_CACHE_COPY:-false} == true ]]; then
    exit 98
  fi
  printf 'StoreDir: /nix/store\n' >"$cache_path/nix-cache-info"
else
  echo "Unexpected nix invocation: $*" >&2
  exit 99
fi
MOCK
chmod +x "$test_root/bin/nix"

printf 'StoreDir: /nix/store\n' >"$test_root/cache/nix-cache-info"
printf 'stale' >"$test_root/cache/stale.nar.zst"
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
grep -Fqx "extra-substituters = file://$test_root/cache" "$test_root/github-env"
grep -Fqx 'fallback = true' "$test_root/github-env"
grep -Fqx /nix/store/existing "$test_root/runner-temp/nix-build-cache-before.txt"

env "${common_env[@]}" NIX_CI_CACHE_MAX_BYTES=1048576 \
  "$repo_root/scripts/ci/nix-build-cache.sh" save

grep -Fq -- '--no-recursive --to file://' "$test_root/copy-calls"
grep -Fq -- '?compression=zstd&compression-level=1 /nix/store/fixed-output-dependency' "$test_root/copy-calls"
for excluded in \
  existing \
  fetched-fixed-output \
  normal-built-output \
  direct-source \
  referenced-content-addressed-output; do
  if grep -Fq "/nix/store/$excluded" "$test_root/copy-calls"; then
    echo "The cache export included ineligible path: $excluded" >&2
    exit 1
  fi
done
grep -Fqx 'save=true' "$test_root/github-output"
grep -Fqx 'path-count=1' "$test_root/github-output"
test -f "$test_root/cache/new.nar.zst"
test ! -e "$test_root/cache/stale.nar.zst"
if compgen -G "$test_root/cache.new.*" >/dev/null ||
  compgen -G "$test_root/cache.old.*" >/dev/null; then
  echo "Temporary cache directories remain after successful replacement" >&2
  exit 1
fi

: >"$test_root/github-output"
rm -f "$test_root/copy-calls"
env "${common_env[@]}" CACHE_BUDGET_FIXTURE=true NIX_CI_CACHE_MAX_BYTES=1048576 \
  "$repo_root/scripts/ci/nix-build-cache.sh" save
grep -Fq '/nix/store/large /nix/store/small' "$test_root/copy-calls"
for excluded in oversized medium referenced-small; do
  if grep -Fq "/nix/store/$excluded" "$test_root/copy-calls"; then
    echo "The budgeted cache export included excluded path: $excluded" >&2
    exit 1
  fi
done
grep -Fqx 'save=true' "$test_root/github-output"
grep -Fqx 'path-count=2' "$test_root/github-output"

: >"$test_root/github-output"
printf 'restored' >"$test_root/cache/restored-cache-sentinel"
if env "${common_env[@]}" FAIL_CACHE_COPY=true NIX_CI_CACHE_MAX_BYTES=1048576 \
  "$repo_root/scripts/ci/nix-build-cache.sh" save; then
  echo "Cache save unexpectedly succeeded when export failed" >&2
  exit 1
fi
grep -Fqx restored "$test_root/cache/restored-cache-sentinel"
if compgen -G "$test_root/cache.new.*" >/dev/null ||
  compgen -G "$test_root/cache.old.*" >/dev/null; then
  echo "Temporary cache directories remain after failed export" >&2
  exit 1
fi

: >"$test_root/github-output"
env "${common_env[@]}" NIX_CI_CACHE_MAX_BYTES=2 \
  "$repo_root/scripts/ci/nix-build-cache.sh" save
grep -Fqx 'save=false' "$test_root/github-output"
