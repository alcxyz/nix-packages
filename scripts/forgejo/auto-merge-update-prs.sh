#!/usr/bin/env bash
set -euo pipefail

: "${FORGEJO_TOKEN:?FORGEJO_TOKEN is required}"
: "${FORGEJO_URL:?FORGEJO_URL is required}"
: "${FORGEJO_OWNER:?FORGEJO_OWNER is required}"
: "${FORGEJO_REPO:?FORGEJO_REPO is required}"
: "${BASE_BRANCH:?BASE_BRANCH is required}"
: "${REQUIRED_STATUS_CONTEXTS:?REQUIRED_STATUS_CONTEXTS is required}"

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
' "$pulls_json" | while IFS= read -r pr; do
  number="$(jq -r '.number // .index' <<< "$pr")"
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
    echo "  skip: PR is not based on current ${BASE_BRANCH}"
    echo "        merge base: ${merge_base}"
    echo "        base head:  ${base_sha}"
    continue
  fi

  deadline=$((SECONDS + wait_for_status_seconds))
  while true; do
    status_json="$(curl -fsS "${api_auth[@]}" "${api_base}/commits/${head_sha}/status")"
    state="$(jq -r '.state' <<< "$status_json")"

    missing_contexts=()
    for context in "${required_contexts[@]}"; do
      [ -n "$context" ] || continue
      if ! jq -e --arg context "$context" \
        '.statuses[] | select(.context == $context and .status == "success")' \
        >/dev/null <<< "$status_json"; then
        missing_contexts+=("$context")
      fi
    done

    if [ "$state" = "success" ] && [ "${#missing_contexts[@]}" -eq 0 ]; then
      break
    fi

    case "$state" in
      failure|error)
        echo "  skip: combined status is ${state}"
        continue 2
        ;;
    esac

    if [ "$SECONDS" -ge "$deadline" ]; then
      echo "  skip: combined status is ${state}"
      if [ "${#missing_contexts[@]}" -gt 0 ]; then
        echo "        missing successful status contexts:"
        printf '        %s\n' "${missing_contexts[@]}"
      fi
      continue 2
    fi

    echo "  wait: combined status is ${state}; checking again in 30s"
    sleep 30
  done

  jq -n \
    --arg title "Merge pull request '${title}' (#${number}) from ${head_ref} into ${BASE_BRANCH}" \
    '{
      Do: "merge",
      MergeTitleField: $title,
      MergeMessageField: "",
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
