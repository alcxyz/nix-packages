#!/usr/bin/env bash
# upsert-update-pr.sh must push without writing its credential into the
# checkout's Git config, so a run outside CI leaves none behind, and must send
# only that credential even when actions/checkout has persisted its own.
set -euo pipefail

script="$PWD/scripts/forgejo/upsert-update-pr.sh"
# Ignore the runner's own Git config, such as commit signing or a header.
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null
unset GIT_CONFIG_COUNT
tmp=$(mktemp -d)
server_pid=""
trap '[ -z "$server_pid" ] || kill "$server_pid" 2>/dev/null; rm -rf -- "$tmp"' EXIT

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

git init -q --bare -b dev "$tmp/owner/repo.git"
git clone -q "$tmp/owner/repo.git" "$tmp/work" 2>/dev/null
cd "$tmp/work"
mkdir pkgs
echo old >pkgs/example
git add pkgs
git -c user.name=test -c user.email=test@invalid commit -q -m init
git push -q origin HEAD:dev
echo new >pkgs/example
git clone -q "$tmp/owner/repo.git" "$tmp/http-work" 2>/dev/null

run_upsert() {
  FORGEJO_TOKEN=test-token-value FORGEJO_URL="$1" FORGEJO_OWNER=owner \
    FORGEJO_REPO=repo BASE_BRANCH=dev UPDATE_BRANCH=update/example \
    COMMIT_MESSAGE="chore: update example" PR_TITLE="Update example" \
    BODY_NAME=example VERSION=1 UPSTREAM_URL=https://example.invalid \
    GIT_TERMINAL_PROMPT=0 bash "$script" >/dev/null 2>"$tmp/err"
}

# A file:// forge takes the push; the PR API call after it then fails, which
# this test does not cover.
status=0
run_upsert "file://$tmp" || status=$?
[[ "$status" != 0 ]] || fail "expected the PR API call to fail against file://"
git --git-dir="$tmp/owner/repo.git" rev-parse -q --verify refs/heads/update/example >/dev/null ||
  fail "update branch was not pushed: $(cat "$tmp/err")"
if git config --local --get-regexp 'extraheader' >/dev/null; then
  fail "an extraheader was written to the checkout's config"
fi
if grep -rq 'test-token-value' .git/config; then
  fail "the token was written to the checkout's config"
fi

# Over HTTP, with a checkout-style job token persisted for the whole forge,
# every request must carry exactly one Authorization header: the update
# credential. The forge answers 404, so the push itself fails.
python3 - "$tmp/port" "$tmp/requests" <<'PY' &
import http.server, json, sys

port_file, log_file = sys.argv[1], sys.argv[2]

class Handler(http.server.BaseHTTPRequestHandler):
    def record(self):
        with open(log_file, "a") as log:
            log.write(json.dumps(self.headers.get_all("Authorization") or []) + "\n")
        self.send_response(404)
        self.send_header("Content-Length", "0")
        self.end_headers()

    do_GET = do_POST = record

    def log_message(self, *args):
        pass

server = http.server.HTTPServer(("127.0.0.1", 0), Handler)
with open(port_file + ".tmp", "w") as f:
    f.write(str(server.server_port))
import os
os.rename(port_file + ".tmp", port_file)
server.serve_forever()
PY
server_pid=$!
for _ in $(seq 50); do [ -s "$tmp/port" ] && break; sleep 0.1; done
[ -s "$tmp/port" ] || fail "test forge did not start"
forge="http://127.0.0.1:$(cat "$tmp/port")"
cd "$tmp/http-work"
echo new >pkgs/example
git config --local "http.${forge}/.extraheader" "AUTHORIZATION: basic $(printf 'checkout:job-token' | base64 -w0)"
run_upsert "$forge" && fail "expected the push to the 404 forge to fail"
want="[\"basic $(printf 'owner:test-token-value' | base64 -w0)\"]"
[ -s "$tmp/requests" ] || fail "no request reached the test forge: $(cat "$tmp/err")"
while IFS= read -r seen; do
  [ "$seen" = "$want" ] || fail "request sent Authorization $seen, want $want"
done <"$tmp/requests"

echo "upsert-update-pr tests passed"
