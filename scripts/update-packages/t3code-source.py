#!/usr/bin/env python3
"""Select published T3 nightlies and maintain the shared, immutable source pin."""
import base64
import datetime
import json
import os
from pathlib import Path
import re
import sys
import tempfile
import urllib.request

VERSION = r"(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)"
NIGHTLY = VERSION + r"-nightly\.(\d{8})\.(\d+)"
FIELDS = {"version", "revision", "hash", "cargoHash", "pnpmDepsHash"}


def validate_version(version, nightly=False):
    pattern = NIGHTLY if nightly else VERSION + r"(?:-nightly\.(\d{8})\.(\d+))?"
    match = re.fullmatch(pattern, version)
    if not match:
        raise ValueError("invalid T3 version")
    if match.groups() and match.group(1):
        datetime.datetime.strptime(match.group(1), "%Y%m%d")
    return version


def select_nightly(releases):
    candidates = []
    for release in releases:
        tag = release.get("tag_name", "")
        if release.get("draft") or not isinstance(tag, str) or not tag.startswith("v"):
            continue
        try:
            validate_version(tag[1:], nightly=True)
            published = datetime.datetime.fromisoformat(release["published_at"].replace("Z", "+00:00"))
            if published.tzinfo is None:
                raise ValueError("missing timezone")
        except (ValueError, TypeError, KeyError, AttributeError):
            continue
        candidates.append((published, tag))
    if not candidates:
        raise ValueError("no published T3 nightly release found")
    return max(candidates)[1][1:]


def validate_pin(pin):
    if set(pin) != FIELDS or not all(isinstance(value, str) for value in pin.values()):
        raise ValueError("unexpected T3 source pin fields")
    validate_version(pin["version"])
    if not re.fullmatch(r"[0-9a-f]{40}", pin["revision"]):
        raise ValueError("source revision must be a full commit SHA")
    for field in ("hash", "cargoHash", "pnpmDepsHash"):
        value = pin[field]
        if not re.fullmatch(r"sha256-[A-Za-z0-9+/]{43}=", value):
            raise ValueError(f"invalid {field}")
        raw = base64.b64decode(value[7:], validate=True)
        if len(raw) != 32 or base64.b64encode(raw).decode() != value[7:]:
            raise ValueError(f"invalid {field}")
    return pin


def write_pin(path, pin):
    validate_pin(pin)
    path = Path(path)
    with tempfile.NamedTemporaryFile(mode="w", dir=path.parent, delete=False) as output:
        temporary = Path(output.name)
        json.dump(pin, output, indent=2)
        output.write("\n")
    try:
        temporary.chmod(0o644)
        temporary.replace(path)
    finally:
        temporary.unlink(missing_ok=True)


def api(path):
    headers = {"Accept": "application/vnd.github+json", "User-Agent": "t3code-package-updater"}
    if os.environ.get("GITHUB_TOKEN"):
        headers["Authorization"] = "Bearer " + os.environ["GITHUB_TOKEN"]
    request = urllib.request.Request("https://api.github.com/repos/pingdotgg/t3code/" + path, headers=headers)
    with urllib.request.urlopen(request, timeout=60) as response:
        return json.load(response)


def resolve_tag(version, request=api):
    validate_version(version)
    obj = request("git/ref/tags/v" + version)["object"]
    for _ in range(10):
        sha = obj["sha"]
        if not re.fullmatch(r"[0-9a-f]{40}", sha):
            raise ValueError("invalid tag object SHA")
        if obj["type"] == "commit":
            return sha
        if obj["type"] != "tag":
            break
        obj = request("git/tags/" + sha)["object"]
    raise ValueError("tag does not resolve to a commit")


def main():
    command, *args = sys.argv[1:]
    if command == "target":
        version = os.environ.get("T3CODE_VERSION")
        if version is None:
            releases = []
            page = 1
            while True:
                batch = api(f"releases?per_page=100&page={page}")
                releases.extend(batch)
                if len(batch) < 100:
                    break
                page += 1
            version = select_nightly(releases)
        validate_version(version)
        print(version)
        print(resolve_tag(version))
    elif command == "write":
        path, version, revision, source_hash, cargo_hash, pnpm_hash = args
        write_pin(path, dict(zip(("version", "revision", "hash", "cargoHash", "pnpmDepsHash"),
                                 (version, revision, source_hash, cargo_hash, pnpm_hash))))
    elif command == "read":
        pin = validate_pin(json.loads(Path(args[0]).read_text()))
        print(pin["version"])
        print(pin["revision"])
    else:
        raise ValueError("unknown command")


if __name__ == "__main__":
    main()
