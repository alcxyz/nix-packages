#!/usr/bin/env bash
set -euo pipefail

: "${FORGEJO_TOKEN:?FORGEJO_TOKEN is required}"
: "${FORGEJO_URL:?FORGEJO_URL is required}"
: "${FORGEJO_OWNER:?FORGEJO_OWNER is required}"
: "${FORGEJO_REPO:?FORGEJO_REPO is required}"
: "${BASE_BRANCH:?BASE_BRANCH is required}"
: "${UPDATE_BRANCH:?UPDATE_BRANCH is required}"
: "${COMMIT_MESSAGE:?COMMIT_MESSAGE is required}"
: "${PR_TITLE:?PR_TITLE is required}"
: "${BODY_NAME:?BODY_NAME is required}"
: "${VERSION:?VERSION is required}"
: "${UPSTREAM_URL:?UPSTREAM_URL is required}"

body=$(cat <<EOF
Automated update of **${BODY_NAME}** to \`${VERSION}\`.

Upstream release: ${UPSTREAM_URL}
EOF
)

if git diff --quiet; then
  echo "No worktree changes remain after updater; skipping PR."
  exit 0
fi

git config user.name "forgejo-actions"
git config user.email "forgejo-actions@alc.xyz"
git switch -C "$UPDATE_BRANCH"
git add pkgs
git commit -m "$COMMIT_MESSAGE"

git remote set-url origin "${FORGEJO_URL}/${FORGEJO_OWNER}/${FORGEJO_REPO}.git"
auth_header=$(printf '%s:%s' "$FORGEJO_OWNER" "$FORGEJO_TOKEN" | base64 -w0)
git config --local "http.${FORGEJO_URL}/.extraheader" "AUTHORIZATION: basic ${auth_header}"

lease_args=()
if remote_ref=$(git ls-remote --heads origin "$UPDATE_BRANCH" | awk '{print $1}'); then
  if [ -n "$remote_ref" ]; then
    lease_args=("--force-with-lease=refs/heads/${UPDATE_BRANCH}:${remote_ref}")
  fi
fi

git push "${lease_args[@]}" origin "HEAD:refs/heads/${UPDATE_BRANCH}"

api_auth=$(mktemp)
payload=$(mktemp)
response=$(mktemp)
trap 'rm -f "$api_auth" "$payload" "$response"' EXIT

printf 'header = "Authorization: token %s"\n' "$FORGEJO_TOKEN" > "$api_auth"
printf 'header = "Accept: application/json"\n' >> "$api_auth"
printf 'header = "Content-Type: application/json"\n' >> "$api_auth"

export BASE_BRANCH UPDATE_BRANCH PR_TITLE body
python3 - <<'PY' > "$payload"
import json
import os

print(json.dumps({
    "base": os.environ["BASE_BRANCH"],
    "head": os.environ["UPDATE_BRANCH"],
    "title": os.environ["PR_TITLE"],
    "body": os.environ["body"],
}))
PY

pulls_url="${FORGEJO_URL}/api/v1/repos/${FORGEJO_OWNER}/${FORGEJO_REPO}/pulls"
status=$(curl -sS -o "$response" -w '%{http_code}' -K "$api_auth" \
  --data @"$payload" \
  "$pulls_url")

case "$status" in
  200|201)
    python3 - <<'PY' "$response"
import json
import sys

data = json.load(open(sys.argv[1]))
number = data.get("number", data.get("index"))
print(f"Opened Forgejo PR #{number}: {data['html_url']}")
PY
    ;;
  409|422)
    query_response=$(mktemp)
    patch_payload=$(mktemp)
    trap 'rm -f "$api_auth" "$payload" "$response" "$query_response" "$patch_payload"' EXIT

    curl -sS -K "$api_auth" \
      "${pulls_url}?state=open&base=${BASE_BRANCH}" \
      -o "$query_response"

    pr_number=$(UPDATE_BRANCH="$UPDATE_BRANCH" python3 - <<'PY' "$query_response"
import json
import os
import sys

target = os.environ["UPDATE_BRANCH"]
for pr in json.load(open(sys.argv[1])):
    head = pr.get("head") or {}
    if head.get("ref") == target or head.get("label", "").endswith(":" + target):
        print(pr.get("number", pr.get("index")))
        break
PY
)

    if [ -z "$pr_number" ]; then
      echo "A Forgejo PR may already exist, but it could not be found for ${UPDATE_BRANCH}." >&2
      cat "$response" >&2
      exit 1
    fi

    python3 - <<'PY' > "$patch_payload"
import json
import os

print(json.dumps({
    "title": os.environ["PR_TITLE"],
    "body": os.environ["body"],
}))
PY

    patch_status=$(curl -sS -o "$response" -w '%{http_code}' -K "$api_auth" \
      -X PATCH \
      --data @"$patch_payload" \
      "${pulls_url}/${pr_number}")

    if [ "$patch_status" != "200" ]; then
      echo "Failed to refresh Forgejo PR #${pr_number}; HTTP ${patch_status}" >&2
      cat "$response" >&2
      exit 1
    fi

    echo "Refreshed Forgejo PR #${pr_number} for ${UPDATE_BRANCH}."
    ;;
  *)
    echo "Failed to create Forgejo PR; HTTP ${status}" >&2
    cat "$response" >&2
    exit 1
    ;;
esac
