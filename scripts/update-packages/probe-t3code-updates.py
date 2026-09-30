#!/usr/bin/env python3
"""Skip unchanged T3 candidates before installing Nix or downloading sources."""
import base64
import importlib.util
import json
import os
from pathlib import Path
import re
import sys
import urllib.parse
import urllib.error
import urllib.request

sys.dont_write_bytecode = True
spec = importlib.util.spec_from_file_location("t3code_source", Path(__file__).with_name("t3code-source.py"))
source = importlib.util.module_from_spec(spec)
spec.loader.exec_module(source)
BRANCH = "update/t3code"
PIN_PATH = "pkgs/t3code/source.json"


def forgejo_api(path):
    base = os.environ["FORGEJO_URL"].rstrip("/")
    owner = urllib.parse.quote(os.environ["FORGEJO_OWNER"], safe="")
    repo = urllib.parse.quote(os.environ["FORGEJO_REPO"], safe="")
    return f"{base}/api/v1/repos/{owner}/{repo}/{path}"


def fetch_json(url):
    token = os.environ.get("FORGEJO_TOKEN")
    if not token:
        raise ValueError("FORGEJO_TOKEN is required for T3 update PR lookup")
    request = urllib.request.Request(url, headers={
        "Accept": "application/json", "User-Agent": "nix-packages-t3-update-probe",
        "Authorization": "token " + token,
    })
    with urllib.request.build_opener(NoRedirect()).open(request, timeout=15) as response:
        return json.load(response)


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        raise urllib.error.HTTPError(req.full_url, code, "T3 probe refuses redirects", headers, fp)


def open_update_pr(get=fetch_json):
    matches = []
    page = 1
    while True:
        pulls = get(forgejo_api(f"pulls?state=open&base=dev&limit=100&page={page}"))
        if not isinstance(pulls, list):
            raise ValueError("Forgejo pull request list is not an array")
        matches.extend(pr for pr in pulls if pr.get("state") == "open"
                       and (pr.get("base") or {}).get("ref") == "dev"
                       and (pr.get("head") or {}).get("ref") == BRANCH)
        if len(pulls) < 100:
            break
        page += 1
    if len(matches) > 1:
        raise ValueError("multiple open T3 update PRs found")
    return matches[0] if matches else None


def pending_pin(pr, get=fetch_json):
    sha = (pr.get("head") or {}).get("sha")
    if not isinstance(sha, str) or not re.fullmatch(r"[0-9a-f]{40}", sha):
        raise ValueError("open T3 update PR has no valid head SHA")
    path = urllib.parse.quote(PIN_PATH, safe="/")
    data = get(forgejo_api(f"contents/{path}?ref={sha}"))
    if data.get("encoding") != "base64" or not isinstance(data.get("content"), str):
        raise ValueError("unexpected T3 PR head pin encoding")
    try:
        pin = json.loads(base64.b64decode("".join(data["content"].split()), validate=True))
    except (ValueError, TypeError) as error:
        raise ValueError("invalid T3 PR head pin") from error
    return source.validate_pin(pin)


def main():
    installed = source.validate_pin(json.loads(Path(PIN_PATH).read_text()))
    target = source.discover_target()
    pr = open_update_pr()
    pending = pending_pin(pr) if pr else None
    should_run = source.decide(installed, target, pending)
    print(f"T3 candidate: upstream {target['version']}, fork nightly "
          f"{target['forks']['nightly']['version']}, stable {target['forks']['stable']['version']}; "
          f"pending PR: {pr is not None}; update required: {should_run}")
    with open(os.environ["GITHUB_OUTPUT"], "a") as output:
        print(f"should_run={str(should_run).lower()}", file=output)
        print(f"existing_update_pr={str(pr is not None).lower()}", file=output)


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, KeyError, TypeError) as error:
        print(f"T3 update probe failed: {error}", file=sys.stderr)
        sys.exit(1)
