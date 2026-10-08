#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
test_root=$(mktemp -d)
real_rm=$(command -v rm)
trap '"$real_rm" -rf "$test_root"' EXIT
mkdir -p "$test_root/bin" "$test_root/out-t3code/bin"

cat >"$test_root/bin/git" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
printf 'git %s\n' "$*" >>"$MOCK_STATE/calls"
case "$1" in
  fetch) ;;
  diff) printf '%s\n' "$MOCK_CHANGED" ;;
  *) echo "Unexpected git invocation: $*" >&2; exit 91 ;;
esac
MOCK

cat >"$test_root/bin/nix" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
printf 'nix %s\n' "$*" >>"$MOCK_STATE/calls"
if [[ -n "${MOCK_FAIL_NIX:-}" && "$*" == "$MOCK_FAIL_NIX" ]]; then
  echo 'mocked provider build failure' >&2
  exit 47
fi
case "$*" in
  'eval --raw .#claude-code.version') printf '1.2.3\n' ;;
  'eval --raw .#codex-cli.version') printf '4.5.6\n' ;;
  'eval --raw .#codex-app-server.version') printf '7.8.9\n' ;;
  'eval --raw .#t3code.embeddedProviderVersions.claudeCode') printf '1.2.3\n' ;;
  'eval --raw .#t3code.embeddedProviderVersions.codexCli') printf '4.5.6\n' ;;
  'eval --raw .#t3code.version') printf '0.1.0\n' ;;
  'build -L .#t3code --no-link --print-out-paths') printf '%s/out-t3code\n' "$MOCK_STATE" ;;
  'build -L '*) ;;
  *) echo "Unexpected nix invocation: $*" >&2; exit 92 ;;
esac
MOCK

cat >"$test_root/bin/nix-store" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
printf 'nix-store %s\n' "$*" >>"$MOCK_STATE/calls"
[[ "$1 $2" == '-q --references' ]] || exit 93
printf '%s\n' \
  /nix/store/mock-claude-code-1.2.3 \
  /nix/store/mock-codex-cli-4.5.6
MOCK

cat >"$test_root/out-t3code/bin/t3" <<'MOCK'
#!/usr/bin/env bash
printf 't3 v0.1.0\n'
MOCK
chmod +x "$test_root/bin/"* "$test_root/out-t3code/bin/t3"

new_case() {
  : >"$test_root/calls"
  : >"$test_root/output"
  export MOCK_STATE="$test_root"
  export MOCK_CHANGED="$1"
  export CASE_BASE_REF=dev
  unset MOCK_FAIL_NIX T3CODE_VERIFY_ALWAYS PACKAGE_BUILD_SELECTED_FILE
}

run_case() {
  local expected=$1 status=0
  (cd "$repo_root" && PATH="$test_root/bin:$PATH" \
    GITHUB_BASE_REF="$CASE_BASE_REF" GITEA_BASE_REF='' FORGEJO_BASE_REF='' \
    GITHUB_REF_NAME=feature GITEA_REF_NAME='' FORGEJO_REF_NAME='' \
    bash "$repo_root/scripts/ci/verify-t3code-providers.sh") >"$test_root/output" 2>&1 || status=$?
  if [[ "$status" != "$expected" ]]; then
    cat "$test_root/output" >&2
    echo "Expected status $expected, got $status" >&2
    exit 1
  fi
}

assert_called() {
  if ! grep -Fqx -- "$1" "$test_root/calls"; then
    echo "Missing invocation: $1" >&2
    cat "$test_root/calls" >&2
    exit 1
  fi
}

assert_not_called() {
  if grep -Fq -- "$1" "$test_root/calls"; then
    echo "Unexpected invocation containing: $1" >&2
    cat "$test_root/calls" >&2
    exit 1
  fi
}

# The T3 source pin and recipe select the T3 closure.
new_case 'pkgs/t3code/source.json'
run_case 0
assert_called 'nix build -L .#t3code --no-link --print-out-paths'

# A packaged provider feeds the T3 wrapper, so its closure remains required.
new_case 'pkgs/claude-code/default.nix'
run_case 0
assert_called 'nix build -L .#t3code --no-link --print-out-paths'

# A global package input produces the full-matrix marker and validates T3.
new_case 'flake.lock'
run_case 0
assert_called 'nix build -L .#t3code --no-link --print-out-paths'

# Full-matrix shards verify T3 only in the job that built it.
new_case 'flake.lock'
export PACKAGE_BUILD_SELECTED_FILE="$test_root/shard-selected"
printf 't3code\n' >"$PACKAGE_BUILD_SELECTED_FILE"
run_case 0
assert_called 'nix build -L .#t3code --no-link --print-out-paths'

new_case 'flake.lock'
export PACKAGE_BUILD_SELECTED_FILE="$test_root/shard-selected"
printf 'kdash\n' >"$PACKAGE_BUILD_SELECTED_FILE"
run_case 0
assert_not_called 'nix '

new_case 'flake.lock'
export PACKAGE_BUILD_SELECTED_FILE="$test_root/missing-selected"
rm -f "$PACKAGE_BUILD_SELECTED_FILE"
run_case 2
assert_not_called 'nix '

# Unrelated package changes do not invoke Nix from provider verification.
new_case 'pkgs/kdash/default.nix'
run_case 0
assert_not_called 'nix '
grep -Fq 'skipping provider closure verification' "$test_root/output"

# The updater's explicit bypass always validates T3.
new_case 'docs/guide.md'
export T3CODE_VERIFY_ALWAYS=true
run_case 0
assert_not_called 'git '
assert_called 'nix build -L .#t3code --no-link --print-out-paths'

# Main/local invocations without a pull-request base also validate T3.
new_case 'docs/guide.md'
export CASE_BASE_REF=''
run_case 0
assert_not_called 'git '
assert_called 'nix build -L .#t3code --no-link --print-out-paths'

# A Nix failure is returned unchanged.
new_case 'pkgs/t3code/default.nix'
export MOCK_FAIL_NIX='build -L .#t3code.resourceMonitor --no-link'
run_case 47
grep -Fq 'mocked provider build failure' "$test_root/output"
assert_not_called 'nix build -L .#t3code --no-link --print-out-paths'

echo 'T3 Code provider selection regression tests passed.'
