#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
test_root=$(mktemp -d)
trap 'rm -rf "$test_root"' EXIT
mkdir -p "$test_root/bin"

cat >"$test_root/bin/nix" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$MOCK_STATE/calls"
case "$1 $2" in
  'eval .#packages') cat "$MOCK_STATE/exports" ;;
  "eval ${MOCK_FAIL_EVAL:-unused}")
    echo 'original supported-platform evaluation error' >&2
    exit 42
    ;;
  "build ${MOCK_FAIL_BUILD:-unused}")
    echo 'original native build error' >&2
    exit 43
    ;;
  'eval '*.drvPath) echo '"/nix/store/mock.drv"' ;;
  # The original bug would incorrectly succeed through this metadata fallback.
  'eval '*.meta.platforms) echo '["aarch64-darwin"]' ;;
  'build '*) ;;
  *) echo "Unexpected mock nix invocation: $*" >&2; exit 99 ;;
esac
MOCK
# Never let the runner's existing cleanup helper touch the real host or sleep.
cat >"$test_root/bin/rm" <<'MOCK'
#!/usr/bin/env bash
[[ "$*" == '-rf /homeless-shelter' ]] || exit 99
MOCK
cat >"$test_root/bin/sleep" <<'MOCK'
#!/usr/bin/env bash
exit 0
MOCK
chmod +x "$test_root/bin/"*

new_case() {
  case_root="$test_root/$1"
  mkdir -p "$case_root"
  cd "$case_root"
  git init -q
  git config user.name 'CI regression test'
  git config user.email 'ci@example.invalid'
  mkdir -p pkgs/widget pkgs/mac-only pkgs/removed
  touch pkgs/widget/default.nix pkgs/mac-only/default.nix pkgs/removed/default.nix
  git add .
  git commit -qm baseline
  git update-ref refs/remotes/origin/main HEAD
  export MOCK_STATE="$case_root"
  unset MOCK_FAIL_EVAL MOCK_FAIL_BUILD
  cat >exports <<'JSON'
{"x86_64-linux":["widget"],"aarch64-linux":["widget"],"aarch64-darwin":["widget","mac-only"],"x86_64-darwin":["mac-only"]}
JSON
}

change_file() {
  mkdir -p "$(dirname "$1")"
  printf 'change\n' >>"$1"
  git add "$1"
  git commit -qm change
}

run_case() {
  local expected=$1 status=0
  PATH="$test_root/bin:$PATH" \
    GITHUB_BASE_REF='' GITEA_BASE_REF='' FORGEJO_BASE_REF='' \
    GITHUB_REF_NAME=dev GITEA_REF_NAME='' FORGEJO_REF_NAME='' \
    bash "$repo_root/scripts/ci/build-changed-packages.sh" >output 2>&1 || status=$?
  if [[ "$status" != "$expected" ]]; then
    cat output >&2
    echo "$(basename "$case_root"): expected status $expected, got $status" >&2
    exit 1
  fi
}

assert_called() {
  if ! grep -Fqx -- "$1" calls; then
    echo "Missing invocation: $1" >&2
    cat calls >&2
    exit 1
  fi
}

assert_not_called() {
  if grep -Fq -- "$1" calls; then
    echo "Unexpected invocation containing: $1" >&2
    exit 1
  fi
}

new_case linux-eval-failure
change_file pkgs/widget/default.nix
export MOCK_FAIL_EVAL='.#packages.x86_64-linux.widget.drvPath'
run_case 42
grep -q 'original supported-platform evaluation error' output
assert_not_called '.meta.platforms'
assert_not_called 'build .#packages.x86_64-linux.widget'

new_case native-build-failure
change_file pkgs/widget/default.nix
export MOCK_FAIL_BUILD='.#packages.x86_64-linux.widget'
run_case 43
grep -q 'original native build error' output
assert_not_called '.meta.platforms'

new_case darwin-only
change_file pkgs/mac-only/default.nix
run_case 0
assert_called 'eval .#packages.aarch64-darwin.mac-only.drvPath'
assert_called 'eval .#packages.x86_64-darwin.mac-only.drvPath'
assert_not_called 'build .#packages.'
grep -q 'derivation evaluated only; no native build' output

new_case second-darwin-failure
change_file pkgs/mac-only/default.nix
export MOCK_FAIL_EVAL='.#packages.x86_64-darwin.mac-only.drvPath'
run_case 42

new_case targeted-change
change_file pkgs/widget/default.nix
run_case 0
assert_called 'build .#packages.x86_64-linux.widget'
assert_called 'eval .#packages.aarch64-linux.widget.drvPath'
assert_not_called 'mac-only'
assert_not_called 'build .#packages.aarch64'

for shared in flake.nix flake.lock pkgs/shared.nix lib/packages.nix scripts/ci/check.sh pkgs/claude-code/default.nix; do
  new_case "shared-${shared//\//-}"
  change_file "$shared"
  run_case 0
  assert_called 'build .#packages.x86_64-linux.widget'
  assert_called 'eval .#packages.x86_64-darwin.mac-only.drvPath'
done

new_case docs-only
change_file docs/guide.md
run_case 0
assert_not_called 'eval .#packages.'

new_case removed-package
git rm -q pkgs/removed/default.nix
git commit -qm remove
run_case 0
grep -q 'skipping removed package' output

new_case missing-export
change_file pkgs/new-package/default.nix
run_case 1
grep -q 'has source files but no exported package' output

new_case overlay-name
change_file pkgs/openzfs-7_1/default.nix
printf '{"x86_64-linux":["openzfs_7_1"]}\n' >exports
run_case 0
assert_called 'build .#packages.x86_64-linux.openzfs_7_1'

echo 'Changed-package validation regression tests passed.'
