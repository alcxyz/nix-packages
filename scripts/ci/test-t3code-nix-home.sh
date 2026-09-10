#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
test_root=$(mktemp -d)
real_rm=$(command -v rm)
trap '"$real_rm" -rf "$test_root"' EXIT
mkdir -p "$test_root/bin"
touch "$test_root/.dockerenv"

cat >"$test_root/bin/rm" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$CLEANUP_TEST_ROOT/rm-calls"
[[ "$*" == "--recursive --force --one-file-system -- $CLEANUP_TEST_ROOT/homeless-shelter" ]] || exit 99
"$REAL_RM" "$@"
MOCK

cat >"$test_root/bin/sleep" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
[[ "$1" == 1 ]] || exit 99
count=$(($(cat "$CLEANUP_TEST_ROOT/sleep-calls") + 1))
printf '%s\n' "$count" >"$CLEANUP_TEST_ROOT/sleep-calls"
case "$SLEEP_MODE:$count" in
  retry:2|exhaust:*) mkdir -p "$CLEANUP_TEST_ROOT/homeless-shelter" ;;
esac
MOCK
chmod +x "$test_root/bin/rm" "$test_root/bin/sleep"

reset_case() {
  "$real_rm" -rf "$test_root/homeless-shelter"
  mkdir -p "$test_root/homeless-shelter"
  : >"$test_root/rm-calls"
  printf '0\n' >"$test_root/sleep-calls"
}

run_cleanup() {
  PATH="$test_root/bin:$PATH" \
    REAL_RM="$real_rm" \
    CLEANUP_TEST_ROOT="$test_root" \
    T3CODE_CI_CLEAN_HOME=true \
    NIX_CI_EPHEMERAL_CONTAINER=1 \
    SLEEP_MODE="$1" \
    bash -c '
      source "$1"
      nix_ci_homeless_shelter=$2
      nix_ci_container_marker=$3
      clean_homeless_shelter
    ' test \
      "$repo_root/scripts/ci/t3code-nix-home.sh" \
      "$test_root/homeless-shelter" \
      "$test_root/.dockerenv"
}

# A path recreated during the stability wait is removed on the next attempt.
reset_case
run_cleanup retry
[[ ! -e "$test_root/homeless-shelter" ]]
[[ $(wc -l <"$test_root/rm-calls") -eq 2 ]]
[[ $(cat "$test_root/sleep-calls") -eq 4 ]]

# Continual recreation exhausts all attempts and fails loudly.
reset_case
if run_cleanup exhaust 2>"$test_root/exhaust-stderr"; then
  echo "Cleanup unexpectedly succeeded after exhausting retries." >&2
  exit 1
fi
[[ -d "$test_root/homeless-shelter" ]]
[[ $(wc -l <"$test_root/rm-calls") -eq 10 ]]
[[ $(cat "$test_root/sleep-calls") -eq 10 ]]
grep -Fq "Unable to keep $test_root/homeless-shelter absent" "$test_root/exhaust-stderr"

# Even with both opt-ins, a symlink is refused without invoking cleanup or waits.
reset_case
"$real_rm" -rf "$test_root/homeless-shelter"
mkdir "$test_root/symlink-target"
ln -s "$test_root/symlink-target" "$test_root/homeless-shelter"
if run_cleanup symlink 2>"$test_root/symlink-stderr"; then
  echo "Cleanup unexpectedly accepted a symlink." >&2
  exit 1
fi
[[ -L "$test_root/homeless-shelter" ]]
[[ ! -s "$test_root/rm-calls" ]]
[[ $(cat "$test_root/sleep-calls") -eq 0 ]]
grep -Fq "Refusing T3 Code home cleanup" "$test_root/symlink-stderr"
