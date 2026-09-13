#!/usr/bin/env bash
# shellcheck disable=SC2016 # Single-quoted fixture commands expand in child shells.
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
test_root=$(mktemp -d)
real_rm=$(command -v rm)
trap '"$real_rm" -rf "$test_root"' EXIT
mkdir -p "$test_root/bin" "$test_root/runner-temp"

head_sha=1111111111111111111111111111111111111111
new_sha=2222222222222222222222222222222222222222

cat >"$test_root/bin/git" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$SUPERVISOR_TEST_ROOT/git-calls"
case "$1" in
  rev-parse)
    printf '%s\n' "$MOCK_CHECKED_HEAD"
    ;;
  ls-remote)
    case "$MOCK_REMOTE_MODE" in
      same) remote_sha=$MOCK_CHECKED_HEAD ;;
      different)
        if [[ -e "$SUPERVISOR_TEST_ROOT/command-ready" ]]; then
          remote_sha=$MOCK_NEW_HEAD
        else
          remote_sha=$MOCK_CHECKED_HEAD
        fi
        ;;
      error) exit 1 ;;
      invalid) printf 'not-a-commit\t%s\n' "$3"; exit 0 ;;
      *) exit 94 ;;
    esac
    printf '%s\t%s\n' "$remote_sha" "$3"
    ;;
  *) echo "Unexpected git invocation: $*" >&2; exit 95 ;;
esac
MOCK

cat >"$test_root/long-command.sh" <<'COMMAND'
#!/usr/bin/env bash
set -euo pipefail
trap 'touch "$SUPERVISOR_TEST_ROOT/child-terminated"; exit 0' TERM
bash -c '
  trap '\''touch "$SUPERVISOR_TEST_ROOT/grandchild-terminated"; exit 0'\'' TERM
  while :; do sleep 1; done
' &
printf '%s\n' "$!" >"$SUPERVISOR_TEST_ROOT/grandchild-pid"
touch "$SUPERVISOR_TEST_ROOT/command-ready"
while :; do sleep 1; done
COMMAND
chmod +x "$test_root/bin/git" "$test_root/long-command.sh"

reset_case() {
  : >"$test_root/git-calls"
  : >"$test_root/output"
  "$real_rm" -f \
    "$test_root/child-terminated" \
    "$test_root/command-ready" \
    "$test_root/grandchild-pid" \
    "$test_root/grandchild-terminated" \
    "$test_root/ran"
  export MOCK_CHECKED_HEAD=$head_sha
  export MOCK_NEW_HEAD=$new_sha
  export MOCK_REMOTE_MODE=same
}

run_pr_case() {
  local expected=$1
  shift
  local status=0

  PATH="$test_root/bin:$PATH" \
    RUNNER_TEMP="$test_root/runner-temp" \
    SUPERVISOR_TEST_ROOT="$test_root" \
    CI_PULL_REQUEST_NUMBER=42 \
    CI_PULL_REQUEST_HEAD_SHA="$head_sha" \
    CI_PULL_REQUEST_POLL_SECONDS=0.02 \
    "$repo_root/scripts/ci/run-current-pr.sh" "$@" >"$test_root/output" 2>&1 || status=$?
  if [[ "$status" != "$expected" ]]; then
    cat "$test_root/output" >&2
    echo "Expected status $expected, got $status" >&2
    exit 1
  fi
}

# Non-PR calls are transparent, including argument boundaries and exit status.
reset_case
status=0
PATH="$test_root/bin:$PATH" \
  SUPERVISOR_TEST_ROOT="$test_root" \
  env -u CI_PULL_REQUEST_NUMBER -u CI_PULL_REQUEST_HEAD_SHA \
  "$repo_root/scripts/ci/run-current-pr.sh" \
  bash -c 'printf "%s\n" "$1"; exit 23' command 'argument with spaces' \
  >"$test_root/output" 2>&1 || status=$?
[[ "$status" == 23 ]]
grep -Fqx 'argument with spaces' "$test_root/output"
[[ ! -s "$test_root/git-calls" ]]

# Genuine command failures are preserved while the PR head stays current.
reset_case
run_pr_case 37 bash -c 'sleep 0.08; exit 37'
grep -Fqx 'ls-remote origin refs/pull/42/head' "$test_root/git-calls"

# Temporary remote lookup failures do not terminate valid work.
reset_case
export MOCK_REMOTE_MODE=error
run_pr_case 0 bash -c 'sleep 0.08; touch "$SUPERVISOR_TEST_ROOT/ran"'
[[ -e "$test_root/ran" ]]

# Invalid remote output is also ignored rather than treated as a new head.
reset_case
export MOCK_REMOTE_MODE=invalid
run_pr_case 0 bash -c 'sleep 0.08; touch "$SUPERVISOR_TEST_ROOT/ran"'
[[ -e "$test_root/ran" ]]

# A changed valid head terminates the complete owned process group and exits 75.
reset_case
export MOCK_REMOTE_MODE=different
run_pr_case 75 "$test_root/long-command.sh"
grep -Fq 'PR #42 advanced; stopping obsolete CI work' "$test_root/output"
[[ -e "$test_root/child-terminated" ]]
[[ -e "$test_root/grandchild-terminated" ]]
grandchild_pid=$(cat "$test_root/grandchild-pid")
# Minimal container PID 1 processes may leave terminated orphans as zombies.
# A zombie is no longer running; kill -0 alone cannot distinguish it.
for _ in {1..20}; do
  grandchild_state=$(ps -o stat= -p "$grandchild_pid" || true)
  case "$grandchild_state" in ''|Z*) break ;; esac
  sleep 0.01
done
case "$grandchild_state" in
  '') ;;
  Z*) echo "Terminated grandchild awaits container reaping (state $grandchild_state)." ;;
  *)
    echo "Grandchild survived stale-run termination (state $grandchild_state)." >&2
    exit 1
    ;;
esac

# A mismatched checkout is rejected before the command starts.
reset_case
export MOCK_CHECKED_HEAD=$new_sha
run_pr_case 2 bash -c 'touch "$SUPERVISOR_TEST_ROOT/ran"'
[[ ! -e "$test_root/ran" ]]
grep -Fq 'checked-out commit does not match' "$test_root/output"

# Partial or malformed PR identity cannot influence the remote ref argument.
reset_case
status=0
PATH="$test_root/bin:$PATH" \
  CI_PULL_REQUEST_NUMBER='42;unexpected' \
  CI_PULL_REQUEST_HEAD_SHA="$head_sha" \
  "$repo_root/scripts/ci/run-current-pr.sh" true >"$test_root/output" 2>&1 || status=$?
[[ "$status" == 2 ]]
[[ ! -s "$test_root/git-calls" ]]

echo 'Current pull-request supervisor regression tests passed.'
