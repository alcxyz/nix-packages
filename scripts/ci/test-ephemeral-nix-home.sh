#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
test_root=$(mktemp -d)
trap 'rm -rf "$test_root"' EXIT
mkdir -p "$test_root/bin" "$test_root/outside"
real_rm=$(command -v rm)

test_helper="$test_root/ephemeral-nix-home.sh"
sed \
  -e "s|^nix_ci_homeless_shelter=/homeless-shelter$|nix_ci_homeless_shelter=$test_root/homeless-shelter|" \
  -e "s|^nix_ci_container_marker=/.dockerenv$|nix_ci_container_marker=$test_root/.dockerenv|" \
  "$repo_root/scripts/ci/ephemeral-nix-home.sh" >"$test_helper"
if cmp -s "$repo_root/scripts/ci/ephemeral-nix-home.sh" "$test_helper"; then
  echo "Cleanup fixture did not replace the production paths." >&2
  exit 1
fi

cat >"$test_root/bin/rm" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$CLEANUP_TEST_ROOT/rm-calls"
[[ "$*" == "--recursive --force --one-file-system -- $CLEANUP_TEST_ROOT/homeless-shelter" ]] || exit 99
"$REAL_RM" "$@"
MOCK
chmod +x "$test_root/bin/rm"

run_case() {
  local name=$1
  local opt_in=$2
  local marker=$3
  local expect_cleanup=$4

  "$real_rm" -rf "$test_root/homeless-shelter" "$test_root/.dockerenv"
  mkdir -p "$test_root/homeless-shelter"
  printf 'preserve\n' >"$test_root/homeless-shelter/inside"
  printf 'preserve\n' >"$test_root/outside/sentinel"
  : >"$test_root/rm-calls"
  [[ "$marker" == true ]] && touch "$test_root/.dockerenv"

  PATH="$test_root/bin:$PATH" \
    REAL_RM="$real_rm" \
    CLEANUP_TEST_ROOT="$test_root" \
    NIX_CI_EPHEMERAL_CONTAINER="$opt_in" \
    bash -c 'source "$1"; clean_ephemeral_nix_home' "$name" "$test_helper"

  [[ -f "$test_root/outside/sentinel" ]]
  if [[ "$expect_cleanup" == true ]]; then
    [[ ! -e "$test_root/homeless-shelter" ]]
    grep -Fqx -- "--recursive --force --one-file-system -- $test_root/homeless-shelter" \
      "$test_root/rm-calls"
  else
    [[ -f "$test_root/homeless-shelter/inside" ]]
    [[ ! -s "$test_root/rm-calls" ]]
  fi
}

run_case neither-signal 0 false false
run_case opt-in-only 1 false false
run_case identity-only 0 true false
run_case both-signals 1 true true

# The literal source expression is the contract under test.
# shellcheck disable=SC2016
expected_source='source "$(dirname "${BASH_SOURCE[0]}")/../ci/ephemeral-nix-home.sh"'
for updater in update-codex-cli.sh update-codex-app-server.sh update-claude-code.sh; do
  grep -Fq "$expected_source" \
    "$repo_root/scripts/update-packages/$updater"
  grep -Fq 'clean_ephemeral_nix_home' "$repo_root/scripts/update-packages/$updater"
done
grep -Fq 'NIX_CI_EPHEMERAL_CONTAINER: "1"' \
  "$repo_root/.forgejo/workflows/update-packages.yml"
grep -Fq 'source scripts/ci/ephemeral-nix-home.sh' \
  "$repo_root/.forgejo/workflows/update-packages.yml"
grep -Fq 'clean_ephemeral_nix_home' "$repo_root/.forgejo/workflows/update-packages.yml"
grep -A3 -F 'name: Verify T3 Code provider wiring' "$repo_root/.forgejo/workflows/ci.yml" |
  grep -Fq 'NIX_CI_EPHEMERAL_CONTAINER: "1"'

if grep -Eq 'rm[[:space:]]+-[^[:space:]]*r[^[:space:]]*f[^[:space:]]*[[:space:]]+/homeless-shelter' \
  "$repo_root/.forgejo/workflows/update-packages.yml" \
  "$repo_root/scripts/update-packages/update-codex-cli.sh" \
  "$repo_root/scripts/update-packages/update-codex-app-server.sh" \
  "$repo_root/scripts/update-packages/update-claude-code.sh" \
  "$repo_root/scripts/ci/t3code-nix-home.sh"; then
  echo "A package cleanup entrypoint still removes /homeless-shelter directly." >&2
  exit 1
fi
