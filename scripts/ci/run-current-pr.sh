#!/usr/bin/env bash
set -euo pipefail

pr_number=${CI_PULL_REQUEST_NUMBER:-}
event_head=${CI_PULL_REQUEST_HEAD_SHA:-}
poll_seconds=${CI_PULL_REQUEST_POLL_SECONDS:-30}

usage() {
  echo "Usage: $0 command [arg ...]" >&2
}

fail() {
  echo "run-current-pr: $*" >&2
  exit 2
}

(($# > 0)) || {
  usage
  exit 2
}

if [[ -z "$pr_number" && -z "$event_head" ]]; then
  exec "$@"
fi

[[ "$pr_number" =~ ^[1-9][0-9]*$ ]] ||
  fail "CI_PULL_REQUEST_NUMBER must be a positive integer"
[[ "$event_head" =~ ^[0-9a-fA-F]{40}$ ]] ||
  fail "CI_PULL_REQUEST_HEAD_SHA must be a 40-character hexadecimal commit ID"
[[ "$poll_seconds" =~ ^([1-9][0-9]*|0\.[0-9]*[1-9][0-9]*)$ ]] ||
  fail "CI_PULL_REQUEST_POLL_SECONDS must be greater than zero"

event_head=${event_head,,}
checked_out=$(git rev-parse HEAD 2>/dev/null) ||
  fail "cannot determine the checked-out commit"
checked_out=${checked_out,,}
[[ "$checked_out" == "$event_head" ]] ||
  fail "the checked-out commit does not match the pull-request event head"

stale_marker=$(mktemp "${RUNNER_TEMP:-/tmp}/current-pr-stale.XXXXXX")
child_pid=
monitor_pid=

terminate_child_group() {
  [[ -n "$child_pid" ]] || return 0
  kill -0 -- "-${child_pid}" 2>/dev/null || return 0
  kill -TERM -- "-${child_pid}" 2>/dev/null || true
  for _ in {1..100}; do
    kill -0 -- "-${child_pid}" 2>/dev/null || return 0
    sleep 0.1
  done
  kill -KILL -- "-${child_pid}" 2>/dev/null || true
}

stop_monitor() {
  [[ -n "$monitor_pid" ]] || return 0
  kill -TERM "$monitor_pid" 2>/dev/null || true
  wait "$monitor_pid" 2>/dev/null || true
  monitor_pid=
}

# shellcheck disable=SC2329 # Invoked by the EXIT trap.
cleanup() {
  stop_monitor
  terminate_child_group
  rm -f -- "$stale_marker"
}

# shellcheck disable=SC2329 # Invoked by the signal traps.
handle_signal() {
  local status=$1

  stop_monitor
  terminate_child_group
  exit "$status"
}

trap cleanup EXIT
trap 'handle_signal 130' INT
trap 'handle_signal 143' TERM

setsid -- "$@" &
child_pid=$!

monitor_head() {
  local remote_output remote_sha sleep_pid=

  trap '[[ -z "$sleep_pid" ]] || kill "$sleep_pid" 2>/dev/null; exit 0' TERM
  while kill -0 "$child_pid" 2>/dev/null; do
    sleep "$poll_seconds" &
    sleep_pid=$!
    wait "$sleep_pid" || return 0
    sleep_pid=

    if ! remote_output=$(GIT_TERMINAL_PROMPT=0 timeout 10 \
      git ls-remote origin "refs/pull/${pr_number}/head" 2>/dev/null); then
      continue
    fi
    read -r remote_sha _ <<<"$remote_output"
    [[ "$remote_sha" =~ ^[0-9a-fA-F]{40}$ ]] || continue
    if [[ "${remote_sha,,}" != "$event_head" ]]; then
      printf 'stale\n' >"$stale_marker"
      echo "run-current-pr: PR #${pr_number} advanced; stopping obsolete CI work." >&2
      terminate_child_group
      return 0
    fi
  done
}

monitor_head &
monitor_pid=$!

child_status=0
wait "$child_pid" || child_status=$?

if [[ -s "$stale_marker" ]]; then
  # Let the monitor finish its process-group grace period before cleanup clears
  # the leader PID; descendants may outlive a cooperative command shell.
  wait "$monitor_pid" 2>/dev/null || true
  monitor_pid=
  child_pid=
  exit 75
fi
child_pid=
stop_monitor
exit "$child_status"
