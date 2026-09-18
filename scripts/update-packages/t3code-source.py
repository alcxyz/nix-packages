#!/usr/bin/env python3
"""Discover and maintain immutable upstream and tested-fork T3 source pins."""
import base64
import datetime
import json
import os
from pathlib import Path
import re
import sys
import tempfile
import urllib.request

UPSTREAM_OWNER = "pingdotgg"
FORK_OWNER = "alcxyz"
REPOSITORY = "t3code"
FORK_BRANCH = "feat/automatic-thread-titles"
FORK_METADATA = ".github/fork-source.json"
VERSION = r"(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)"
NIGHTLY = VERSION + r"-nightly\.(\d{8})\.(\d+)"
FIELDS = {
    "version", "revision", "hash", "cargoHash", "pnpmDepsHash",
    "forkVersion", "forkRevision", "forkUpstreamRevision", "forkHash",
    "forkCargoHash", "forkPnpmDepsHash", "forkPatchRevision",
}
PROMOTION_FIELDS = {"upstreamRevision", "version"}
IDENTITY_FIELDS = (
    "version", "revision", "forkVersion", "forkRevision", "forkUpstreamRevision",
)
HASH_FIELDS = (
    "hash", "cargoHash", "pnpmDepsHash", "forkHash", "forkCargoHash",
    "forkPnpmDepsHash",
)


def validate_version(version, nightly=False):
    pattern = NIGHTLY if nightly else VERSION + r"(?:-nightly\.(\d{8})\.(\d+))?"
    match = re.fullmatch(pattern, version)
    if not match:
        raise ValueError("invalid T3 version")
    if match.groups() and match.group(1):
        datetime.datetime.strptime(match.group(1), "%Y%m%d")
    return version


def version_tuple(version):
    if not isinstance(version, str) or not re.fullmatch(VERSION, version):
        raise ValueError("fork version must be a stable T3 version")
    return tuple(int(part) for part in version.split("."))


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
    if not isinstance(pin, dict) or set(pin) != FIELDS:
        raise ValueError("unexpected T3 source pin fields")
    validate_version(pin["version"])
    version_tuple(pin["forkVersion"])
    validate_sha(pin["revision"], "source revision")
    validate_sha(pin["forkRevision"], "fork revision")
    validate_sha(pin["forkUpstreamRevision"], "fork upstream revision")
    for field in HASH_FIELDS:
        validate_hash(pin[field], field)
    if type(pin["forkPatchRevision"]) is not int or pin["forkPatchRevision"] < 1:
        raise ValueError("forkPatchRevision must be a positive integer")
    return pin


def validate_promotion(metadata):
    if not isinstance(metadata, dict) or set(metadata) != PROMOTION_FIELDS:
        raise ValueError("unexpected fork promotion metadata fields")
    version_tuple(metadata["version"])
    validate_sha(metadata["upstreamRevision"], "fork upstream revision")
    return metadata


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
    with urllib.request.urlopen(request, timeout=60) as response:
        return json.load(response)


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


def decode_content(response):
    if response.get("encoding") != "base64" or not isinstance(response.get("content"), str):
        raise ValueError("fork promotion metadata is not base64 content")
    try:
        encoded = "".join(response["content"].split())
        return json.loads(base64.b64decode(encoded, validate=True))
    except (ValueError, TypeError, json.JSONDecodeError) as error:
        raise ValueError("invalid fork promotion metadata content") from error


def resolve_fork_promotion(request=api):
    ref = request(FORK_OWNER, "git/ref/heads/" + FORK_BRANCH)
    fork_revision = validate_sha(ref["object"]["sha"], "fork revision")
    if ref["object"].get("type") != "commit":
        raise ValueError("fork promotion ref does not resolve to a commit")

    metadata = validate_promotion(
        decode_content(request(FORK_OWNER, f"contents/{FORK_METADATA}?ref={fork_revision}"))
    )
    baseline = metadata["upstreamRevision"]
    upstream_commit = request(UPSTREAM_OWNER, "commits/" + baseline)
    if upstream_commit.get("sha") != baseline:
        raise ValueError("fork baseline is not an exact upstream commit")

    comparison = request(FORK_OWNER, f"compare/{baseline}...{fork_revision}")
    merge_base = comparison.get("merge_base_commit") or {}
    if comparison.get("status") not in ("ahead", "identical") or merge_base.get("sha") != baseline:
        raise ValueError("fork promotion does not descend from its declared upstream revision")
    return metadata["version"], fork_revision, baseline


def discover_target(request=api):
    version = os.environ.get("T3CODE_VERSION")
    if version is None:
        releases = []
        page = 1
        while True:
            batch = request(UPSTREAM_OWNER, f"releases?per_page=100&page={page}")
            releases.extend(batch)
            if len(batch) < 100:
                break
            page += 1
        version = select_nightly(releases)
    validate_version(version)
    revision = resolve_tag(version, request)
    return (version, revision, *resolve_fork_promotion(request))


def fork_patch_revision(pin, fork_version, fork_revision, fork_upstream_revision):
    validate_pin(pin)
    version_tuple(fork_version)
    validate_sha(fork_revision, "fork revision")
    validate_sha(fork_upstream_revision, "fork upstream revision")
    if version_tuple(fork_version) < version_tuple(pin["forkVersion"]):
        raise ValueError(
            f"refusing fork version regression from {pin['forkVersion']} to {fork_version}"
        )
    current = tuple(pin[field] for field in IDENTITY_FIELDS[2:])
    target = (fork_version, fork_revision, fork_upstream_revision)
    return pin["forkPatchRevision"] + (target != current)


def print_identity(values):
    for value in values:
        print(value)


def main():
    command, *args = sys.argv[1:]
    if command == "target" and not args:
        print_identity(discover_target())
    elif command == "read" and len(args) == 1:
        pin = validate_pin(json.loads(Path(args[0]).read_text()))
        print_identity(pin[field] for field in IDENTITY_FIELDS)
    elif command == "fork-patch-revision" and len(args) == 4:
        pin = validate_pin(json.loads(Path(args[0]).read_text()))
        print(fork_patch_revision(pin, *args[1:]))
    elif command == "write" and len(args) == 13:
        path, *values = args
        names = (
            "version", "revision", "hash", "cargoHash", "pnpmDepsHash",
            "forkVersion", "forkRevision", "forkUpstreamRevision", "forkHash",
            "forkCargoHash", "forkPnpmDepsHash",
        )
        pin = dict(zip(names, values[:-1]))
        pin["forkPatchRevision"] = int(values[-1])
        write_pin(path, pin)
    else:
        raise ValueError("unknown command or arguments")


if __name__ == "__main__":
    main()
