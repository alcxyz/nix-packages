#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
test_root=$(mktemp -d)
trap 'rm -rf "$test_root"' EXIT
mkdir -p "$test_root/bin"
real_rm=$(command -v rm)

test_script="$test_root/build-changed-packages.sh"
sed \
  -e "s|^homeless_shelter=/homeless-shelter$|homeless_shelter=$test_root/homeless-shelter|" \
  -e "s|^container_marker=/.dockerenv$|container_marker=$test_root/.dockerenv|" \
  "$repo_root/scripts/ci/build-changed-packages.sh" >"$test_script"
if cmp -s "$repo_root/scripts/ci/build-changed-packages.sh" "$test_script"; then
  echo "Cleanup fixture did not replace the production paths." >&2
  exit 1
fi

cat >"$test_root/bin/nix" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$MOCK_STATE/calls"
if [[ "$*" == "${MOCK_RECREATE_HOME_ONCE:-unused}" && ! -e "$MOCK_STATE/recreated-home" ]]; then
  touch "$MOCK_STATE/recreated-home"
  mkdir -p "$CLEANUP_TEST_ROOT/homeless-shelter"
  exit 44
fi
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
  'build '*)
    if [[ "${MOCK_RECREATE_HOME_AFTER_BUILD:-false}" == true ]]; then
      mkdir -p "$CLEANUP_TEST_ROOT/homeless-shelter"
    fi
    ;;
  *) echo "Unexpected mock nix invocation: $*" >&2; exit 99 ;;
esac
MOCK
# Never let the runner's existing cleanup helper touch the real host or sleep.
cat >"$test_root/bin/rm" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$MOCK_STATE/rm-calls"
[[ "$*" == "--recursive --force --one-file-system -- $CLEANUP_TEST_ROOT/homeless-shelter" ]] || exit 99
printf 'cleanup\n' >>"$MOCK_STATE/calls"
"$REAL_RM" "$@"
MOCK
cat >"$test_root/bin/sleep" <<'MOCK'
#!/usr/bin/env bash
exit 0
MOCK
chmod +x "$test_root/bin/"*

new_case() {
  case_root="$test_root/$1"
  "$real_rm" -rf "$test_root/homeless-shelter" "$test_root/.dockerenv"
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
  : >rm-calls
  unset NIX_CI_EPHEMERAL_CONTAINER
  unset MOCK_FAIL_EVAL MOCK_FAIL_BUILD MOCK_RECREATE_HOME_ONCE \
    MOCK_RECREATE_HOME_AFTER_BUILD PACKAGE_BUILD_MODE \
    PACKAGE_BUILD_SHARD_COUNT PACKAGE_BUILD_SHARD_INDEX
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
    CLEANUP_TEST_ROOT="$test_root" REAL_RM="$real_rm" \
    bash "$test_script" >output 2>&1 || status=$?
  if [[ "$status" != "$expected" ]]; then
    cat output >&2
    echo "$(basename "$case_root"): expected status $expected, got $status" >&2
    exit 1
  fi
}

assert_cleanup_not_called() {
  if [[ -s rm-calls ]]; then
    echo "Cleanup ran outside the permitted ephemeral container context." >&2
    cat rm-calls >&2
    exit 1
  fi
}

assert_cleanup_called() {
  grep -Fqx -- "--recursive --force --one-file-system -- $test_root/homeless-shelter" rm-calls
}

prepare_cleanup_fixture() {
  mkdir -p "$test_root/homeless-shelter" "$test_root/outside"
  printf 'preserve\n' >"$test_root/homeless-shelter/inside"
  printf 'preserve\n' >"$test_root/outside/sentinel"
}

assert_cleanup_preserved() {
  [[ -f "$test_root/homeless-shelter/inside" ]]
  [[ -f "$test_root/outside/sentinel" ]]
  assert_cleanup_not_called
}

assert_called() {
  if ! grep -Fqx -- "$1" calls; then
    echo "Missing invocation: $1" >&2
    cat calls >&2
    exit 1
  fi
}

new_case cleanup-neither-signal
prepare_cleanup_fixture
run_case 0
assert_cleanup_preserved
assert_called 'build .#agent-sync-check -L'

new_case cleanup-opt-in-only
prepare_cleanup_fixture
export NIX_CI_EPHEMERAL_CONTAINER=1
run_case 0
assert_cleanup_preserved
assert_called 'build .#agent-sync-check -L'

new_case cleanup-identity-only
prepare_cleanup_fixture
touch "$test_root/.dockerenv"
run_case 0
assert_cleanup_preserved
assert_called 'build .#agent-sync-check -L'

new_case cleanup-local-failure
prepare_cleanup_fixture
export MOCK_RECREATE_HOME_ONCE='build .#agent-sync-check -L'
run_case 44
assert_cleanup_preserved
[[ "$(grep -Fxc 'build .#agent-sync-check -L' calls)" == 1 ]]

new_case cleanup-permitted-retry
prepare_cleanup_fixture
touch "$test_root/.dockerenv"
export NIX_CI_EPHEMERAL_CONTAINER=1
export MOCK_RECREATE_HOME_ONCE='build .#agent-sync-check -L'
run_case 0
assert_cleanup_called
[[ "$(grep -Fxc 'build .#agent-sync-check -L' calls)" == 2 ]]
[[ ! -e "$test_root/homeless-shelter" ]]
[[ -f "$test_root/outside/sentinel" ]]

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
for baseline in agent-sync-check forge-mirror nix-deploy zfs-auto-unlock devlog wcap; do
  assert_called "build .#${baseline} -L"
done
assert_called 'build .#packages.x86_64-linux.widget -L'
assert_called 'eval .#packages.aarch64-linux.widget.drvPath'
assert_not_called 'mac-only'
assert_not_called 'build .#packages.aarch64'

new_case tool-contract-change
change_file tools/nix-deploy/test-deploy.py
jq 'map_values(. + ["nix-deploy"])' exports >exports.new
mv exports.new exports
run_case 0
assert_called 'build .#packages.x86_64-linux.nix-deploy -L'
assert_called 'eval .#packages.aarch64-darwin.nix-deploy.drvPath'
assert_not_called '.#packages.x86_64-linux.widget'
assert_not_called '.#packages.x86_64-darwin.mac-only'

for shared in flake.nix flake.lock pkgs/shared.nix lib/packages.nix scripts/ci/check.sh pkgs/claude-code/default.nix; do
  new_case "shared-${shared//\//-}"
  change_file "$shared"
  run_case 0
  assert_called 'build .#packages.x86_64-linux.widget -L'
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

new_case baseline-only
export PACKAGE_BUILD_MODE=baseline
run_case 0
assert_not_called 'eval .#packages'
if [[ "$(grep -c '^build \.#' calls)" != 6 ]]; then
  echo 'Baseline mode did not build exactly the six baseline packages.' >&2
  cat calls >&2
  exit 1
fi

partition_root="$test_root/partition"
mkdir -p "$partition_root"
partition_exports='{"x86_64-linux":["alpha","bravo","charlie","delta","echo","foxtrot","golf","hotel","india","juliet"],"aarch64-linux":["alpha","delta"],"aarch64-darwin":["bravo","echo","hotel"],"x86_64-darwin":["charlie","foxtrot","india"]}'

new_case partition-unsharded
printf '%s\n' "$partition_exports" >exports
change_file flake.nix
export PACKAGE_BUILD_MODE=selected
run_case 0
grep -E '^(eval|build) \.#packages\.' calls | sort >"$partition_root/expected-calls"

for shard in 0 1 2 3; do
  new_case "partition-${shard}"
  printf '%s\n' "$partition_exports" >exports
  change_file flake.nix
  export PACKAGE_BUILD_MODE=selected
  export PACKAGE_BUILD_SHARD_COUNT=4
  export PACKAGE_BUILD_SHARD_INDEX="$shard"
  run_case 0
  assert_not_called 'build .#agent-sync-check'
  grep -E '^(eval|build) \.#packages\.' calls >>"$partition_root/sharded-calls"
  sed -n 's/^::group::changed package /'"$shard"' /p' output >>"$partition_root/assignments"
  sed -n 's/^::group::changed package //p' output >"$partition_root/shard-${shard}"
done

if [[ "$(cut -d' ' -f2 "$partition_root/assignments" | sort | uniq -d)" != '' ]]; then
  echo 'A selected package was assigned to more than one shard.' >&2
  cat "$partition_root/assignments" >&2
  exit 1
fi
jq -r '[.[][]] | unique[]' "$case_root/exports" | sort >"$partition_root/expected"
cut -d' ' -f2 "$partition_root/assignments" | sort >"$partition_root/actual"
diff -u "$partition_root/expected" "$partition_root/actual"
sort "$partition_root/sharded-calls" >"$partition_root/actual-calls"
diff -u "$partition_root/expected-calls" "$partition_root/actual-calls"

# A repeated shard calculation must select the same sorted attributes.
run_case 0
sed -n 's/^::group::changed package //p' output >"$partition_root/shard-3-repeat"
diff -u "$partition_root/shard-3" "$partition_root/shard-3-repeat"

for invalid in \
  'selected 0 0' \
  'selected 4 4' \
  'selected four 0' \
  'selected 04 0' \
  'selected 4 00' \
  'selected 4 08' \
  'selected 999999999999999999999999 0' \
  'selected 4 999999999999999999999999' \
  'all 4 0' \
  'unknown 1 0'; do
  read -r mode count index <<<"$invalid"
  new_case "invalid-${mode}-${count}-${index}"
  export PACKAGE_BUILD_MODE="$mode"
  export PACKAGE_BUILD_SHARD_COUNT="$count"
  export PACKAGE_BUILD_SHARD_INDEX="$index"
  run_case 2
done

new_case sharded-platform-failure
printf '%s\n' "$partition_exports" >exports
change_file flake.nix
export PACKAGE_BUILD_MODE=selected PACKAGE_BUILD_SHARD_COUNT=4 PACKAGE_BUILD_SHARD_INDEX=2
export MOCK_FAIL_EVAL='.#packages.x86_64-darwin.charlie.drvPath'
run_case 42
grep -q 'original supported-platform evaluation error' output

new_case sharded-native-build-failure
printf '%s\n' "$partition_exports" >exports
change_file flake.nix
export PACKAGE_BUILD_MODE=selected PACKAGE_BUILD_SHARD_COUNT=4 PACKAGE_BUILD_SHARD_INDEX=3
export MOCK_FAIL_BUILD='.#packages.x86_64-linux.delta'
run_case 43
grep -q 'original native build error' output

new_case sharded-missing-export
change_file pkgs/new-package/default.nix
export PACKAGE_BUILD_MODE=selected PACKAGE_BUILD_SHARD_COUNT=4 PACKAGE_BUILD_SHARD_INDEX=0
run_case 1
grep -q 'has source files but no exported package' output

new_case sharded-removed-package
git rm -q pkgs/removed/default.nix
git commit -qm remove
export PACKAGE_BUILD_MODE=selected PACKAGE_BUILD_SHARD_COUNT=4 PACKAGE_BUILD_SHARD_INDEX=0
run_case 0
grep -q 'skipping removed package' output

new_case overlay-name
change_file pkgs/openzfs-7_1/default.nix
printf '{"x86_64-linux":["openzfs_7_1"]}\n' >exports
run_case 0
assert_called 'build .#packages.x86_64-linux.openzfs_7_1 -L'

new_case t3-shared-source
prepare_cleanup_fixture
touch "$test_root/.dockerenv"
export NIX_CI_EPHEMERAL_CONTAINER=1
export MOCK_RECREATE_HOME_AFTER_BUILD=true
change_file pkgs/t3code/source.json
printf '{"x86_64-linux":["t3code","t3code-fork"],"aarch64-darwin":["t3code","t3code-fork"]}\n' >exports
run_case 0
python3 - <<'PYTEST'
from pathlib import Path
calls = Path("calls").read_text().splitlines()
expected = [
    f"build .#packages.x86_64-linux.{flavor}{suffix} -L"
    for flavor in ("t3code", "t3code-fork")
    for suffix in (".pnpmDeps --no-link", ".resourceMonitor --no-link", "")
]
actual = [call for call in calls if call.startswith("build .#packages.x86_64-linux.t3code")]
assert actual == expected, actual
for call in expected:
    index = calls.index(call)
    assert calls[index - 1] == "cleanup", (call, calls[index - 1])
PYTEST
assert_called 'eval .#packages.aarch64-darwin.t3code.drvPath'
assert_called 'eval .#packages.aarch64-darwin.t3code-fork.drvPath'
assert_not_called 'build .#packages.aarch64-darwin.'

new_case t3-dependency-failure
change_file pkgs/t3code/default.nix
printf '{"x86_64-linux":["t3code","t3code-fork"]}\n' >exports
export MOCK_FAIL_BUILD='.#packages.x86_64-linux.t3code.resourceMonitor'
run_case 43
assert_called 'build .#packages.x86_64-linux.t3code.pnpmDeps --no-link -L'
assert_not_called 'build .#packages.x86_64-linux.t3code -L'
assert_not_called 'build .#packages.x86_64-linux.t3code-fork'

echo 'Changed-package validation regression tests passed.'
