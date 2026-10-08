#!/usr/bin/env python3
"""Discover and validate the published upstream T3 nightly pin."""
import base64
import datetime
import json
import os
from pathlib import Path
import re
import sys
import tempfile
import urllib.error
import urllib.request

UPSTREAM_OWNER = "pingdotgg"
REPOSITORY = "t3code"
VERSION = r"(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)"
NIGHTLY = VERSION + r"-nightly\.(\d{8})\.(\d+)"
UPSTREAM_FIELDS = {"version", "revision", "hash", "cargoHash", "pnpmDepsHash"}


def validate_version(version, nightly=False):
    pattern = NIGHTLY if nightly else VERSION + r"(?:-nightly\.(\d{8})\.(\d+))?"
    match = re.fullmatch(pattern, version) if isinstance(version, str) else None
    if not match:
        raise ValueError("invalid T3 version")
    if match.groups() and match.group(1):
        datetime.datetime.strptime(match.group(1), "%Y%m%d")
    return version


def upstream_version_tuple(version):
    validate_version(version)
    base, *suffix = version.split("-nightly.")
    return tuple(int(part) for part in base.split(".")) + (
        (0, *map(int, suffix[0].split("."))) if suffix else (1, 0, 0)
    )


def validate_sha(value, label="revision"):
    if not isinstance(value, str) or not re.fullmatch(r"[0-9a-f]{40}", value):
        raise ValueError(f"{label} must be a full commit SHA")
    return value


def validate_hash(value, label):
    if not isinstance(value, str) or not re.fullmatch(r"sha256-[A-Za-z0-9+/]{43}=", value):
        raise ValueError(f"invalid {label}")
    raw = base64.b64decode(value[7:], validate=True)
    if len(raw) != 32 or base64.b64encode(raw).decode() != value[7:]:
        raise ValueError(f"invalid {label}")


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
    if not isinstance(pin, dict) or set(pin) != UPSTREAM_FIELDS:
        raise ValueError("unexpected T3 source pin fields")
    validate_version(pin["version"])
    validate_sha(pin["revision"], "upstream revision")
    for field in ("hash", "cargoHash", "pnpmDepsHash"):
        validate_hash(pin[field], field)
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


def api(owner, path):
    headers = {"Accept": "application/vnd.github+json", "User-Agent": "t3code-package-updater"}
    if os.environ.get("GITHUB_TOKEN"):
        headers["Authorization"] = "Bearer " + os.environ["GITHUB_TOKEN"]
    request = urllib.request.Request(
        f"https://api.github.com/repos/{owner}/{REPOSITORY}/{path}", headers=headers
    )
    with urllib.request.build_opener(NoRedirect()).open(request, timeout=60) as response:
        return json.load(response)


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        raise urllib.error.HTTPError(req.full_url, code, "T3 discovery refuses redirects", headers, fp)


def resolve_tag(version, request=api):
    validate_version(version)
    obj = request(UPSTREAM_OWNER, "git/ref/tags/v" + version)["object"]
    for _ in range(10):
        sha = validate_sha(obj["sha"], "tag object SHA")
        if obj["type"] == "commit":
            return sha
        if obj["type"] != "tag":
            break
        obj = request(UPSTREAM_OWNER, "git/tags/" + sha)["object"]
    raise ValueError("tag does not resolve to a commit")


def upstream_releases(request=api):
    releases = []
    page = 1
    while True:
        batch = request(UPSTREAM_OWNER, f"releases?per_page=100&page={page}")
        releases.extend(batch)
        if len(batch) < 100:
            break
        page += 1
    return releases


def discover_target(request=api):
    version = os.environ.get("T3CODE_VERSION")
    if version is None:
        version = select_nightly(upstream_releases(request))
    validate_version(version)
    return {"version": version, "revision": resolve_tag(version, request)}


def identity(pin):
    return {"version": pin["version"], "revision": pin["revision"]}


def decide(pin, target, pending=None):
    validate_pin(pin)
    if pending is not None:
        validate_pin(pending)
    for candidate in (pin, pending):
        if candidate is not None and upstream_version_tuple(target["version"]) < upstream_version_tuple(candidate["version"]):
            raise ValueError("upstream T3 version regression")
    if pending is not None and identity(pending) == identity(target):
        return False
    return identity(pin) != identity(target)


def materialize(target, hashes):
    if set(hashes) != {"hash", "cargoHash", "pnpmDepsHash"}:
        raise ValueError("all source and dependency hashes are required")
    return validate_pin(identity(target) | hashes)


def main():
    command, *args = sys.argv[1:]
    if command == "target" and not args:
        print(json.dumps(discover_target()))
    elif command == "read" and len(args) == 1:
        print(json.dumps(validate_pin(json.loads(Path(args[0]).read_text()))))
    elif command == "decide" and len(args) in (2, 3):
        pin = json.loads(Path(args[0]).read_text())
        target = json.loads(Path(args[1]).read_text())
        pending = json.loads(Path(args[2]).read_text()) if len(args) == 3 else None
        print(str(decide(pin, target, pending)).lower())
    elif command == "materialize" and len(args) == 3:
        path, target_path, hash_path = map(Path, args)
        write_pin(path, materialize(json.loads(target_path.read_text()), json.loads(hash_path.read_text())))
    else:
        raise ValueError("unknown command or arguments")


if __name__ == "__main__":
    main()
