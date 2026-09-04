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
    -H|--data)
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

  if [[ "$number" == 1 && -e "$MOCK_STATE_DIR/rebased-1" ]]; then
    merge_base=$base
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

case "$method $url" in
  "GET "*'/pulls?state=open&base=dev&limit=100')
    items=()
    [[ -e "$MOCK_STATE_DIR/open-2" ]] && items+=("$(pr_json 2)")
    [[ -e "$MOCK_STATE_DIR/open-1" ]] && items+=("$(pr_json 1)")
    body="[$(IFS=,; printf '%s' "${items[*]}")]"
    emit "$body"
    ;;
  "POST "*'/pulls/1/update?style=rebase')
    touch "$MOCK_STATE_DIR/rebased-1"
    emit '{}'
    ;;
  "POST "*'/pulls/2/merge')
    rm "$MOCK_STATE_DIR/open-2"
    printf 'base2\n' >"$MOCK_STATE_DIR/base"
    printf '2\n' >>"$MOCK_STATE_DIR/merged"
    emit '{}'
    ;;
  "POST "*'/pulls/1/merge')
    rm "$MOCK_STATE_DIR/open-1"
    printf 'base1\n' >"$MOCK_STATE_DIR/base"
    printf '1\n' >>"$MOCK_STATE_DIR/merged"
    emit '{}'
    ;;
  "GET "*'/pulls/1')
    emit "$(pr_json 1)"
    ;;
  "GET "*'/pulls/2')
    emit "$(pr_json 2)"
    ;;
  "GET "*'/commits/'*'/status')
    emit '{"state":"success","statuses":[{"context":"Validate changes / Go tool tests (pull_request)","status":"success","updated_at":"2026-01-01T00:00:00Z"},{"context":"Validate changes / Package build validation (pull_request)","status":"success","updated_at":"2026-01-01T00:00:00Z"}]}'
    ;;
  *)
    echo "Unexpected mock curl request: $method $url" >&2
    exit 1
    ;;
esac
MOCK_CURL
chmod +x "$mock_bin/curl"

output=$(
  PATH="$mock_bin:$PATH" \
    MOCK_STATE_DIR="$state" \
    FORGEJO_TOKEN=test-token \
    FORGEJO_URL=https://forge.example \
    FORGEJO_OWNER=example \
    FORGEJO_REPO=packages \
    BASE_BRANCH=dev \
    POLL_SECONDS=0 \
    WAIT_FOR_MERGEABLE_SECONDS=5 \
    WAIT_FOR_STATUS_SECONDS=5 \
    REQUIRED_STATUS_CONTEXTS=$'Validate changes / Go tool tests (pull_request)\nValidate changes / Package build validation (pull_request)' \
    "$repo_root/scripts/forgejo/auto-merge-update-prs.sh"
)

grep -Fq 'waiting for Forgejo to recompute mergeability' <<<"$output"
grep -Fq 'Package update queue is drained.' <<<"$output"
[[ "$(tr '\n' ' ' <"$state/merged")" == "2 1 " ]]
[[ ! -e "$state/open-1" && ! -e "$state/open-2" ]]

echo "Verified merge queue waits after rebases and drains every update PR."
