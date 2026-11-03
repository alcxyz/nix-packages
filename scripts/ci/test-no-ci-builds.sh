#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
check="$repo_root/scripts/ci/check-no-ci-builds.sh"
fixture=$(mktemp -d)
trap 'rm -rf "$fixture"' EXIT

"$check" "$repo_root"

new_fixture() {
  rm -rf "$fixture"
  mkdir -p "$fixture/.forgejo/workflows" "$fixture/scripts/ci" \
    "$fixture/scripts/update-packages" "$fixture/scripts/forgejo"
  printf 'steps:\n  - run: scripts/ci/check.sh\n  - run: nix flake check --no-build\n' \
    >"$fixture/.forgejo/workflows/ci.yml"
  printf '#!/usr/bin/env bash\nnix eval .#packages >/dev/null\n' >"$fixture/scripts/ci/check.sh"
  printf 'nix build --dry-run .#fetch\n' >"$fixture/scripts/update-packages/fod-hash.sh"
  printf 'nix build .#mocked\n' >"$fixture/scripts/ci/test-mock.sh"
}

expect_rejected() {
  local description=$1
  if "$check" "$fixture" >/dev/null 2>&1; then
    echo "Expected the guard to reject ${description}." >&2
    exit 1
  fi
}

new_fixture
"$check" "$fixture" >/dev/null

new_fixture
printf '  - run: nix build .#hello\n' >>"$fixture/.forgejo/workflows/ci.yml"
expect_rejected 'nix build in a workflow'

new_fixture
printf '  - run: nix flake check\n' >>"$fixture/.forgejo/workflows/ci.yml"
expect_rejected 'a building nix flake check'

new_fixture
printf 'nix-build -A hello\n' >>"$fixture/scripts/ci/check.sh"
expect_rejected 'nix-build in a script a workflow runs'

new_fixture
printf 'nix build .#pkg --no-link\n' >"$fixture/scripts/update-packages/update-pkg.sh"
expect_rejected 'nix build in an updater'

new_fixture
printf 'scripts/ci/verify-t3code-providers.sh\n' >"$fixture/scripts/update-packages/update-pkg.sh"
expect_rejected 'a local-only build script in an updater'

new_fixture
mkdir -p "$fixture/scripts/lib"
printf 'nix build .#pkg --no-link\n' >"$fixture/scripts/lib/helper.sh"
# shellcheck disable=SC2016 # literal shell text for the fixture
printf 'source "$(dirname "${BASH_SOURCE[0]}")/../lib/helper.sh"\n' \
  >>"$fixture/scripts/ci/check.sh"
expect_rejected "nix build in a helper reached through dirname"

new_fixture
printf 'nix build .#hello\n' >"$fixture/scripts/ci/build-changed-packages.sh"
"$check" "$fixture" >/dev/null

echo 'CI build guard tests passed.'
