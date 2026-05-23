#!/usr/bin/env bash
set -euo pipefail

: "${FORGEJO_TOKEN:?FORGEJO_TOKEN is required}"
: "${FORGEJO_URL:?FORGEJO_URL is required}"
: "${FORGEJO_OWNER:?FORGEJO_OWNER is required}"
: "${FORGEJO_REPO:?FORGEJO_REPO is required}"
: "${BASE_BRANCH:?BASE_BRANCH is required}"
: "${REQUIRED_STATUS_CONTEXTS:?REQUIRED_STATUS_CONTEXTS is required}"

if [ "$BASE_BRANCH" != "dev" ]; then
  echo "Refusing to auto-merge package updates into ${BASE_BRANCH}; expected dev." >&2
  exit 1
fi

wait_for_status_seconds="${WAIT_FOR_STATUS_SECONDS:-0}"

api_auth=(
  -H "Authorization: token ${FORGEJO_TOKEN}"
  -H "Accept: application/json"
  -H "Content-Type: application/json"
)

api_base="${FORGEJO_URL}/api/v1/repos/${FORGEJO_OWNER}/${FORGEJO_REPO}"
pulls_json="$(mktemp)"
response="$(mktemp)"
merge_payload="$(mktemp)"
trap 'rm -f "$pulls_json" "$response" "$merge_payload"' EXIT

curl -fsS "${api_auth[@]}" \
  "${api_base}/pulls?state=open&base=${BASE_BRANCH}&limit=100" \
  -o "$pulls_json"

mapfile -t required_contexts <<< "$REQUIRED_STATUS_CONTEXTS"

jq -c --arg base "$BASE_BRANCH" '
  .[]
  | select(.base.ref == $base)
  | select(.head.ref | startswith("update/"))
' "$pulls_json" | jq -r '.number // .index' | while IFS= read -r number; do
  pr="$(curl -fsS "${api_auth[@]}" "${api_base}/pulls/${number}")"
  title="$(jq -r '.title' <<< "$pr")"
  head_ref="$(jq -r '.head.ref' <<< "$pr")"
  head_sha="$(jq -r '.head.sha' <<< "$pr")"
  base_sha="$(jq -r '.base.sha' <<< "$pr")"
  merge_base="$(jq -r '.merge_base // ""' <<< "$pr")"
  mergeable="$(jq -r '.mergeable' <<< "$pr")"

  echo "Checking PR #${number} (${head_ref}): ${title}"

  if [ "$mergeable" != "true" ]; then
    echo "  skip: PR is not currently mergeable"
    continue
  fi

  if [ "$merge_base" != "$base_sha" ]; then
    echo "  rebase: PR is not based on current ${BASE_BRANCH}"
    echo "        merge base: ${merge_base}"
    echo "        base head:  ${base_sha}"
    update_status="$(curl -sS -o "$response" -w '%{http_code}' "${api_auth[@]}" \
      -X POST \
      "${api_base}/pulls/${number}/update?style=rebase")"

    case "$update_status" in
      200|201|204)
        echo "  rebased; refetching PR and waiting for refreshed pull_request checks"
        pr="$(curl -fsS "${api_auth[@]}" "${api_base}/pulls/${number}")"
        head_sha="$(jq -r '.head.sha' <<< "$pr")"
        base_sha="$(jq -r '.base.sha' <<< "$pr")"
        merge_base="$(jq -r '.merge_base // ""' <<< "$pr")"
        mergeable="$(jq -r '.mergeable' <<< "$pr")"

        if [ "$mergeable" != "true" ]; then
          echo "  skip: PR is not currently mergeable after rebase"
          continue
        fi

        if [ "$merge_base" != "$base_sha" ]; then
          echo "  skip: PR is still not based on current ${BASE_BRANCH} after rebase"
          echo "        merge base: ${merge_base}"
          echo "        base head:  ${base_sha}"
          continue
        fi
        ;;
      409)
        echo "  skip: rebase update reported a conflict"
        cat "$response"
        continue
        ;;
      *)
        echo "  rebase update failed: HTTP ${update_status}" >&2
        cat "$response" >&2
        exit 1
        ;;
    esac
  fi

  deadline=$((SECONDS + wait_for_status_seconds))
  while true; do
    status_json="$(curl -fsS "${api_auth[@]}" "${api_base}/commits/${head_sha}/status")"
    state="$(jq -r '.state' <<< "$status_json")"

    missing_contexts=()
    failed_contexts=()
    for context in "${required_contexts[@]}"; do
      [ -n "$context" ] || continue
      context_status="$(jq -r --arg context "$context" '
        [.statuses[] | select(.context == $context)] | sort_by(.updated_at) | last.status // "missing"
      ' <<< "$status_json")"

      case "$context_status" in
        success)
          ;;
        failure|error)
          failed_contexts+=("${context}: ${context_status}")
          ;;
        *)
          missing_contexts+=("${context}: ${context_status}")
          ;;
      esac
    done

    if [ "${#failed_contexts[@]}" -gt 0 ]; then
      echo "  skip: required status context failed"
      printf '        %s\n' "${failed_contexts[@]}"
      continue 2
    fi

    if [ "${#missing_contexts[@]}" -eq 0 ]; then
      break
    fi

    if [ "$SECONDS" -ge "$deadline" ]; then
      echo "  skip: required status contexts are not successful"
      echo "        combined status: ${state}"
      printf '        %s\n' "${missing_contexts[@]}"
      continue 2
    fi

    echo "  wait: required status contexts are not successful; combined status is ${state}; checking again in 30s"
    sleep 30
  done

  jq -n \
    --arg title "${title} (#${number})" \
    --arg message "" \
    --arg head_sha "$head_sha" \
    '{
      Do: "squash",
      MergeTitleField: $title,
      MergeMessageField: $message,
      head_commit_id: $head_sha,
      delete_branch_after_merge: true
    }' > "$merge_payload"

  status="$(curl -sS -o "$response" -w '%{http_code}' "${api_auth[@]}" \
    -X POST \
    --data @"$merge_payload" \
    "${api_base}/pulls/${number}/merge")"

  case "$status" in
    200|201|204)
      echo "  merged"
      ;;
    *)
      echo "  merge failed: HTTP ${status}" >&2
      cat "$response" >&2
      exit 1
      ;;
  esac
done
