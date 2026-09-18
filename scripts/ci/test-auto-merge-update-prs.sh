#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
test_root=$(mktemp -d)
trap 'rm -rf "$test_root"' EXIT

mock_bin="$test_root/bin"
state="$test_root/state"
mkdir -p "$mock_bin" "$state"
printf 'base0\n' >"$state/base"
touch "$state/open-1" "$state/open-2"

cat >"$mock_bin/curl" <<'MOCK_CURL'
#!/usr/bin/env bash
set -euo pipefail

output_file=""
write_format=""
method=GET
url=""
data_file=""
while (($# > 0)); do
  case "$1" in
    -o)
      output_file=$2
      shift 2
      ;;
    -w)
      write_format=$2
      shift 2
      ;;
    -X)
      method=$2
      shift 2
      ;;
    --data)
      data_file=${2#@}
      shift 2
      ;;
    -H|-K)
      shift 2
      ;;
    -*)
      shift
      ;;
    *)
      url=$1
      shift
      ;;
  esac
done

emit() {
  local body=$1
  local status=${2:-200}
  if [[ -n "$output_file" ]]; then
    printf '%s' "$body" >"$output_file"
  else
    printf '%s' "$body"
  fi
  if [[ -n "$write_format" ]]; then
    printf '%s' "$status"
  fi
}

pr_json() {
  local number=$1
  local base
  local merge_base=base0
  local head="head-${number}"
  local mergeable=true
  base=$(<"$MOCK_STATE_DIR/base")

  if [[ "$number" == 1 && "${MOCK_QUEUE_CASE:-default}" == stale_first ]]; then
    merge_base=older-base
  fi

  if [[ "$number" == 1 && -e "$MOCK_STATE_DIR/rebased-1" ]]; then
    merge_base=$(<"$MOCK_STATE_DIR/rebased-1")
    head=head-1-rebased
    reads=0
    [[ -r "$MOCK_STATE_DIR/rebased-reads" ]] && reads=$(<"$MOCK_STATE_DIR/rebased-reads")
    reads=$((reads + 1))
    printf '%s\n' "$reads" >"$MOCK_STATE_DIR/rebased-reads"
    if ((reads == 1)); then
      mergeable=false
    fi
  elif [[ "$number" == 1 && "$base" != base0 ]]; then
    # Forgejo temporarily reports a stale PR as non-mergeable immediately
    # after another PR changes the target branch.
    mergeable=false
  fi

  printf '{"number":%s,"title":"update %s","head":{"ref":"update/pkg-%s","sha":"%s"},"base":{"ref":"dev","sha":"%s"},"merge_base":"%s","mergeable":%s}' \
    "$number" "$number" "$number" "$head" "$base" "$merge_base" "$mergeable"
}

check_merge_payload() {
  local number=$1
  local expected_head="head-${number}"
  if [[ "$number" == 1 && -e "$MOCK_STATE_DIR/rebased-1" ]]; then
    expected_head=head-1-rebased
  fi
  jq -e --arg head "$expected_head" '
    .head_commit_id == $head and .Do == "squash" and .delete_branch_after_merge == true
  ' "$data_file" >/dev/null
  [[ -e "$MOCK_STATE_DIR/checked-${expected_head}" ]]
  cp "$data_file" "$MOCK_STATE_DIR/merge-payload-${number}"
}

case "$method $url" in
  "GET "*'/pulls?state=open&base=dev&limit=100')
    items=()
    order=(2 1)
    [[ "${MOCK_QUEUE_CASE:-default}" == stale_first ]] && order=(1 2)
    for number in "${order[@]}"; do
      [[ -e "$MOCK_STATE_DIR/open-${number}" ]] && items+=("$(pr_json "$number")")
    done
    body="[$(IFS=,; printf '%s' "${items[*]}")]"
    emit "$body"
    ;;
  "POST "*'/pulls/1/update?style=rebase')
    if [[ "${MOCK_REBASE_CASE:-success}" == success ]]; then
      cp "$MOCK_STATE_DIR/base" "$MOCK_STATE_DIR/rebased-1"
    fi
    printf 'rebase-1:%s\n' "$(<"$MOCK_STATE_DIR/base")" >>"$MOCK_STATE_DIR/events"
    emit '{}'
    ;;
  "POST "*'/pulls/2/merge')
    check_merge_payload 2
    rm "$MOCK_STATE_DIR/open-2"
    printf 'base2\n' >"$MOCK_STATE_DIR/base"
    printf '2\n' >>"$MOCK_STATE_DIR/merged"
    printf 'merge-2\n' >>"$MOCK_STATE_DIR/events"
    emit '{}'
    ;;
  "POST "*'/pulls/1/merge')
    check_merge_payload 1
    rm "$MOCK_STATE_DIR/open-1"
    printf 'base1\n' >"$MOCK_STATE_DIR/base"
    printf '1\n' >>"$MOCK_STATE_DIR/merged"
    printf 'merge-1\n' >>"$MOCK_STATE_DIR/events"
    emit '{}'
    ;;
  "GET "*'/pulls/1')
    emit "$(pr_json 1)"
    ;;
  "GET "*'/pulls/2')
    emit "$(pr_json 2)"
    ;;
  "GET "*'/commits/'*'/status')
    if [[ "$url" == *'/commits/head-1-rebased/status' && "${MOCK_REBASED_STATUS:-success}" == pending ]]; then
      emit '{"state":"pending","statuses":[{"context":"Validate changes / Go tool tests (pull_request)","status":"pending"},{"context":"Validate changes / Package build validation (pull_request)","status":"pending"}]}'
      exit 0
    fi
    if [[ "$url" == *'/commits/head-2/status' && "${MOCK_STATUS_CASE:-success}" != success ]]; then
      case "$MOCK_STATUS_CASE" in
        null) emit '{"state":"","statuses":null}' ;;
        absent) emit '{"state":""}' ;;
        empty) emit '{"state":"","statuses":[]}' ;;
        pending) emit '{"state":"pending","statuses":[{"context":"Validate changes / Go tool tests (pull_request)","status":"pending"}]}' ;;
        failure) emit '{"state":"failure","statuses":[{"context":"Validate changes / Go tool tests (pull_request)","status":"failure"}]}' ;;
      esac
      exit 0
    fi
    checked_head=${url%/status}
    checked_head=${checked_head##*/}
    touch "$MOCK_STATE_DIR/checked-${checked_head}"
    emit '{"state":"success","statuses":[{"context":"Validate changes / Go tool tests (pull_request)","status":"success","updated_at":"2026-01-01T00:00:00Z"},{"context":"Validate changes / Package build validation (pull_request)","status":"success","updated_at":"2026-01-01T00:00:00Z"}]}'
    ;;
  *)
    echo "Unexpected mock curl request: $method $url" >&2
    exit 1
    ;;
esac
MOCK_CURL
chmod +x "$mock_bin/curl"

run_queue() {
  PATH="$mock_bin:$PATH" \
    MOCK_STATE_DIR="$state" \
    FORGEJO_TOKEN=test-token \
    FORGEJO_URL=https://forge.example \
    FORGEJO_OWNER=example \
    FORGEJO_REPO=packages \
    BASE_BRANCH=dev \
    POLL_SECONDS=0 \
    WAIT_FOR_MERGEABLE_SECONDS="${MOCK_WAIT_FOR_MERGEABLE_SECONDS:-5}" \
    WAIT_FOR_STATUS_SECONDS=0 \
    REQUIRED_STATUS_CONTEXTS=$'Validate changes / Go tool tests (pull_request)\nValidate changes / Package build validation (pull_request)' \
    "$repo_root/scripts/forgejo/auto-merge-update-prs.sh"
}

if output=$(run_queue 2>&1); then
  echo "Queue unexpectedly merged a freshly rebased candidate in the same pass" >&2
  exit 1
fi

grep -Fq 'waiting for Forgejo to recompute mergeability' <<<"$output"
[[ "$(cat "$state/merged")" == 2 ]]
[[ -e "$state/open-1" && ! -e "$state/open-2" ]]
[[ ! -e "$state/merge-payload-1" ]]
output=$(run_queue)
grep -Fq 'Package update queue is drained.' <<<"$output"
[[ "$(tr '\n' ' ' <"$state/merged")" == "2 1 " ]]
[[ ! -e "$state/open-1" && ! -e "$state/open-2" ]]

echo "Verified merge queue waits after rebases and defers refreshed candidates to the next pass."

for status_case in null absent empty pending failure; do
  rm -f "$state"/*
  printf 'base0\n' >"$state/base"
  touch "$state/open-1" "$state/open-2"
  export MOCK_STATUS_CASE="$status_case"
  if output=$(run_queue 2>&1); then
    echo "Queue unexpectedly succeeded with ${status_case} checks" >&2
    exit 1
  fi
  # The blocked PR stays open, but it must not prevent the next green PR
  # from being checked and merged. No unchecked head may reach the merge API.
  [[ -e "$state/open-2" && ! -e "$state/open-1" ]]
  [[ "$(cat "$state/merged")" == 1 ]]
  grep -Fq 'Blocked package update PRs remain:' <<<"$output"
  if [[ "$status_case" == failure ]]; then
    grep -Fq '#2: required status failed' <<<"$output"
  else
    grep -Fq '#2: required status missing or pending' <<<"$output"
  fi
done

echo "Verified null, absent, empty, pending, and failed checks block only the affected PR."

# The stale candidate appears first, with green checks on its old head. A later
# current candidate can merge immediately. Rebase only after that merge so the
# stale candidate's next validation uses the final base from this pass.
rm -f "$state"/*
printf 'base0\n' >"$state/base"
touch "$state/open-1" "$state/open-2"
export MOCK_STATUS_CASE=success MOCK_QUEUE_CASE=stale_first MOCK_REBASED_STATUS=pending
if output=$(run_queue 2>&1); then
  echo "Queue unexpectedly succeeded with pending checks on the rebased head" >&2
  exit 1
fi
[[ -e "$state/open-1" && ! -e "$state/open-2" ]]
[[ ! -e "$state/merge-payload-1" ]]
[[ "$(cat "$state/merged")" == 2 ]]
[[ "$(cat "$state/rebased-1")" == base2 ]]
[[ "$(tr '\n' ' ' <"$state/events")" == "merge-2 rebase-1:base2 " ]]
grep -Fq 'Blocked package update PRs remain:' <<<"$output"

# A later pass must continue to block the current rebased head while its checks
# are pending, even though the old head's checks were green.
if output=$(run_queue 2>&1); then
  echo "Queue unexpectedly merged a rebased candidate with pending checks" >&2
  exit 1
fi
[[ -e "$state/open-1" && ! -e "$state/merge-payload-1" ]]
[[ "$(tr '\n' ' ' <"$state/events")" == "merge-2 rebase-1:base2 " ]]
grep -Fq '#1: required status missing or pending' <<<"$output"

# Once checks complete, the next invocation drains the queue without another
# rebase or any unchecked merge.
export MOCK_REBASED_STATUS=success
output=$(run_queue)
[[ ! -e "$state/open-1" && ! -e "$state/open-2" ]]
[[ "$(tr '\n' ' ' <"$state/merged")" == "2 1 " ]]
[[ "$(tr '\n' ' ' <"$state/events")" == "merge-2 rebase-1:base2 merge-1 " ]]
jq -e '.head_commit_id == "head-1-rebased"' "$state/merge-payload-1" >/dev/null
grep -Fq 'Package update queue is drained.' <<<"$output"

echo "Verified stale candidates rebase after ready merges, wait for their new checks, and drain on the next pass with the checked head."

# A successful update API response is insufficient when Forgejo leaves the
# candidate's merge base unchanged. Bound the wait and leave that PR blocked.
rm -f "$state"/*
printf 'base0\n' >"$state/base"
touch "$state/open-1" "$state/open-2"
export MOCK_REBASE_CASE=stalled MOCK_WAIT_FOR_MERGEABLE_SECONDS=0
if output=$(run_queue 2>&1); then
  echo "Queue unexpectedly succeeded when the rebase never updated the merge base" >&2
  exit 1
fi
[[ -e "$state/open-1" && ! -e "$state/open-2" ]]
[[ ! -e "$state/rebased-1" && ! -e "$state/merge-payload-1" ]]
[[ "$(tr '\n' ' ' <"$state/events")" == "merge-2 rebase-1:base2 " ]]
grep -Fq '#1: not mergeable after rebase' <<<"$output"

echo "Verified an accepted rebase with an unchanged merge base stays blocked."
