#!/usr/bin/env python3
"""Cheap release check before the grouped AI package updater installs Nix."""

import base64
import binascii
import json
import os
from pathlib import Path
import re
import sys
import urllib.error
import urllib.parse
import urllib.request


PACKAGES = {
    "claude-code": ("pkgs/claude-code/default.nix", "https://registry.npmjs.org/@anthropic-ai/claude-code/latest"),
    "codex-cli": ("pkgs/codex-cli/default.nix", "https://registry.npmjs.org/@openai/codex/latest"),
    "codex-app-server": ("pkgs/codex-app-server/default.nix", "https://api.github.com/repos/openai/codex/releases/latest"),
}
VERSION = re.compile(r"(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\Z")
PIN = re.compile(r'^\s*version\s*=\s*"([^"]+)"\s*;', re.MULTILINE)
BRANCH = "update/ai-tools"


def semver(value):
    if not isinstance(value, str) or not VERSION.fullmatch(value):
        raise ValueError(f"invalid stable release version: {value!r}")
    return tuple(map(int, value.split(".")))


def pin_version(content, name):
    matches = PIN.findall(content)
    if len(matches) != 1:
        raise ValueError(f"expected one version pin in {name}, found {len(matches)}")
    semver(matches[0])
    return matches[0]


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        raise urllib.error.HTTPError(req.full_url, code, "release probe refuses redirects", headers, fp)


def fetch_json(url):
    headers = {"Accept": "application/json", "User-Agent": "nix-packages-ai-update-probe"}
    if url.startswith("https://api.github.com/") and os.environ.get("GITHUB_TOKEN"):
        headers["Authorization"] = "Bearer " + os.environ["GITHUB_TOKEN"]
    if url.startswith(os.environ.get("FORGEJO_URL", "\0") + "/"):
        token = os.environ.get("FORGEJO_TOKEN")
        if not token:
            raise ValueError("FORGEJO_TOKEN is required for the update PR lookup")
        headers["Authorization"] = "token " + token
    opener = urllib.request.build_opener(NoRedirect())
    with opener.open(urllib.request.Request(url, headers=headers), timeout=10) as response:
        return json.load(response)


def forgejo_api(path):
    base = os.environ["FORGEJO_URL"].rstrip("/")
    owner = urllib.parse.quote(os.environ["FORGEJO_OWNER"], safe="")
    repo = urllib.parse.quote(os.environ["FORGEJO_REPO"], safe="")
    return f"{base}/api/v1/repos/{owner}/{repo}/{path}"


def release_versions(get=fetch_json):
    versions = {}
    for name, (_, url) in PACKAGES.items():
        data = get(url)
        if name == "codex-app-server":
            if data.get("draft") is not False or data.get("prerelease") is not False:
                raise ValueError("Codex GitHub latest release is draft or prerelease")
            tag = data.get("tag_name")
            if not isinstance(tag, str) or not tag.startswith("rust-v"):
                raise ValueError(f"unexpected Codex GitHub release tag: {tag!r}")
            version = tag.removeprefix("rust-v")
        else:
            version = data.get("version")
        semver(version)
        versions[name] = version
    return versions


def local_versions(root):
    return {
        name: pin_version((root / path).read_text(), path)
        for name, (path, _) in PACKAGES.items()
    }


def open_bundle_pr(get=fetch_json):
    page = 1
    matches = []
    while True:
        pulls = get(forgejo_api(f"pulls?state=open&base=dev&limit=100&page={page}"))
        if not isinstance(pulls, list):
            raise ValueError("Forgejo pull request list is not an array")
        matches.extend(
            pr for pr in pulls
            if pr.get("state") == "open"
            and (pr.get("base") or {}).get("ref") == "dev"
            and (pr.get("head") or {}).get("ref") == BRANCH
        )
        if len(pulls) < 100:
            break
        page += 1
    if len(matches) > 1:
        raise ValueError("multiple open AI update PRs found")
    return matches[0] if matches else None


def pending_versions(pr, get=fetch_json):
    sha = (pr.get("head") or {}).get("sha")
    if not isinstance(sha, str) or not re.fullmatch(r"[a-fA-F0-9]{40}", sha):
        raise ValueError("open AI update PR has no valid head SHA")
    versions = {}
    for name, (path, _) in PACKAGES.items():
        encoded_path = urllib.parse.quote(path, safe="/")
        data = get(forgejo_api(f"contents/{encoded_path}?ref={sha}"))
        if data.get("encoding") != "base64":
            raise ValueError(f"unexpected content encoding for {path}")
        try:
            content = base64.b64decode(data["content"], validate=False).decode()
        except (KeyError, TypeError, ValueError, UnicodeDecodeError, binascii.Error) as error:
            raise ValueError(f"invalid PR head content for {path}") from error
        versions[name] = pin_version(content, path)
    return versions


def decide(installed, upstream, pending=None):
    # The grouped updater reads latest releases itself. Running it during a
    # rollback could overwrite a newer pin already on dev or an open PR.
    for name in PACKAGES:
        floor = max(semver(installed[name]), semver(pending[name]) if pending else (0, 0, 0))
        if semver(upstream[name]) < floor:
            raise ValueError(
                f"{name} upstream {upstream[name]} is older than installed/pending version; "
                "refusing grouped update to avoid a downgrade"
            )
    if pending and all(upstream[name] == pending[name] for name in PACKAGES):
        return False
    return any(semver(upstream[name]) > semver(installed[name]) for name in PACKAGES)


def main():
    installed = local_versions(Path.cwd())
    upstream = release_versions()
    pr = open_bundle_pr()
    pending = pending_versions(pr) if pr else None
    should_run = decide(installed, upstream, pending)
    print(f"Installed: {installed}; upstream: {upstream}; pending: {pending}")
    output = os.environ["GITHUB_OUTPUT"]
    with open(output, "a") as stream:
        print(f"should_run={str(should_run).lower()}", file=stream)
        print(f"existing_update_pr={str(pr is not None).lower()}", file=stream)


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, KeyError, TypeError, urllib.error.URLError) as error:
        print(f"AI update probe failed: {error}", file=sys.stderr)
        sys.exit(1)
