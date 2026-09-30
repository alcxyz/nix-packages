#!/usr/bin/env python3
"""Discover and validate published upstream and promoted fork T3 pins."""
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
FORK_OWNER = "alcxyz"
REPOSITORY = "t3code"
FORK_METADATA = ".github/fork-source.json"
CHANNELS = ("nightly", "stable")
VERSION = r"(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)"
NIGHTLY = VERSION + r"-nightly\.(\d{8})\.(\d+)"
UPSTREAM_FIELDS = {"version", "revision", "hash", "cargoHash", "pnpmDepsHash"}
FORK_FIELDS = {
    "version", "releaseTag", "revision", "upstreamRevision", "featureRevision",
    "hash", "cargoHash", "pnpmDepsHash", "patchRevision",
}
LEGACY_FIELDS = UPSTREAM_FIELDS | {
    "forkVersion", "forkRevision", "forkUpstreamRevision", "forkHash",
    "forkCargoHash", "forkPnpmDepsHash", "forkPatchRevision",
}
PROMOTION_FIELDS = {"channel", "releaseTag", "version", "upstreamRevision", "featureRevision"}
FAKE_HASH = "sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="


def validate_version(version, nightly=False):
    pattern = NIGHTLY if nightly else VERSION + r"(?:-nightly\.(\d{8})\.(\d+))?"
    match = re.fullmatch(pattern, version) if isinstance(version, str) else None
    if not match:
        raise ValueError("invalid T3 version")
    if match.groups() and match.group(1):
        datetime.datetime.strptime(match.group(1), "%Y%m%d")
    return version


def version_tuple(version, channel):
    validate_version(version, nightly=channel == "nightly")
    if channel == "stable" and not re.fullmatch(VERSION, version):
        raise ValueError("stable fork version must be stable")
    base, *suffix = version.split("-nightly.")
    return tuple(int(part) for part in base.split(".")) + (
        tuple(map(int, suffix[0].split("."))) if suffix else ()
    )


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
    if not isinstance(pin, dict):
        raise ValueError("T3 source pin must be an object")
    if set(pin) == LEGACY_FIELDS:
        validate_version(pin["version"])
        version_tuple(pin["forkVersion"], "stable")
        for field in ("revision", "forkRevision", "forkUpstreamRevision"):
            validate_sha(pin[field], field)
        for field in ("hash", "cargoHash", "pnpmDepsHash", "forkHash", "forkCargoHash", "forkPnpmDepsHash"):
            validate_hash(pin[field], field)
        if type(pin["forkPatchRevision"]) is not int or pin["forkPatchRevision"] < 1:
            raise ValueError("forkPatchRevision must be a positive integer")
        return pin
    if set(pin) != UPSTREAM_FIELDS | {"forks"}:
        raise ValueError("unexpected T3 source pin fields")
    validate_version(pin["version"])
    validate_sha(pin["revision"], "upstream revision")
    for field in ("hash", "cargoHash", "pnpmDepsHash"):
        validate_hash(pin[field], field)
    if not isinstance(pin["forks"], dict) or set(pin["forks"]) != set(CHANNELS):
        raise ValueError("both fork channels are required")
    for channel, fork in pin["forks"].items():
        if not isinstance(fork, dict) or set(fork) != FORK_FIELDS:
            raise ValueError(f"unexpected {channel} fork pin fields")
        version_tuple(fork["version"], channel)
        if fork["releaseTag"] != "v" + fork["version"]:
            raise ValueError(f"{channel} release tag/version mismatch")
        for field in ("revision", "upstreamRevision", "featureRevision"):
            validate_sha(fork[field], f"{channel} {field}")
        for field in ("hash", "cargoHash", "pnpmDepsHash"):
            validate_hash(fork[field], f"{channel} {field}")
        if type(fork["patchRevision"]) is not int or fork["patchRevision"] < 1:
            raise ValueError(f"{channel} patchRevision must be a positive integer")
    return pin


def validate_promotion(metadata, channel):
    if not isinstance(metadata, dict) or set(metadata) != PROMOTION_FIELDS:
        raise ValueError("unexpected fork promotion metadata fields")
    if metadata["channel"] != channel:
        raise ValueError("fork promotion channel mismatch")
    version_tuple(metadata["version"], channel)
    if metadata["releaseTag"] != "v" + metadata["version"]:
        raise ValueError("fork promotion release tag/version mismatch")
    validate_sha(metadata["upstreamRevision"], "fork upstream revision")
    validate_sha(metadata["featureRevision"], "fork feature revision")
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


def decode_content(response):
    if response.get("encoding") != "base64" or not isinstance(response.get("content"), str):
        raise ValueError("fork promotion metadata is not base64 content")
    try:
        encoded = "".join(response["content"].split())
        return json.loads(base64.b64decode(encoded, validate=True))
    except (ValueError, TypeError, json.JSONDecodeError) as error:
        raise ValueError("invalid fork promotion metadata content") from error


def resolve_fork_promotion(channel, request=api):
    if channel not in CHANNELS:
        raise ValueError("unknown fork channel")
    ref = request(FORK_OWNER, "git/ref/heads/fork/" + channel)
    revision = validate_sha(ref["object"]["sha"], "fork revision")
    if ref["object"].get("type") != "commit":
        raise ValueError("fork promotion ref does not resolve to a commit")
    metadata = validate_promotion(
        decode_content(request(FORK_OWNER, f"contents/{FORK_METADATA}?ref={revision}")), channel
    )
    feature = metadata["featureRevision"]
    if request(FORK_OWNER, "commits/" + feature).get("sha") != feature:
        raise ValueError("fork feature revision is not an exact fork commit")
    release = request(UPSTREAM_OWNER, "releases/tags/" + metadata["releaseTag"])
    if release.get("tag_name") != metadata["releaseTag"] or release.get("draft") is not False:
        raise ValueError("fork baseline is not a published upstream release")
    if channel == "stable" and release.get("prerelease") is not False:
        raise ValueError("stable fork baseline is a prerelease")
    baseline = resolve_tag(metadata["version"], request)
    if metadata["upstreamRevision"] != baseline:
        raise ValueError("fork baseline differs from exact upstream release tag commit")
    comparison = request(FORK_OWNER, f"compare/{baseline}...{revision}")
    merge_base = comparison.get("merge_base_commit") or {}
    if comparison.get("status") not in ("ahead", "identical") or merge_base.get("sha") != baseline:
        raise ValueError("fork promotion does not descend from its declared upstream revision")
    return metadata | {"revision": revision}


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
    return {"version": version, "revision": revision,
            "forks": {channel: resolve_fork_promotion(channel, request) for channel in CHANNELS}}


def identity(pin):
    return {"version": pin["version"], "revision": pin["revision"],
            "forks": {channel: {field: fork[field] for field in (PROMOTION_FIELDS - {"channel"}) | {"revision"}}
                      for channel, fork in pin["forks"].items()}} if "forks" in pin else None


def decide(pin, target, pending=None):
    validate_pin(pin)
    if pending is not None:
        validate_pin(pending)
    for candidate in (pin, pending):
        if candidate is None:
            continue
        if upstream_version_tuple(target["version"]) < upstream_version_tuple(candidate["version"]):
            raise ValueError("upstream T3 version regression")
        if "forks" in candidate:
            for channel in CHANNELS:
                old = candidate["forks"][channel]
                new = target["forks"][channel]
                if version_tuple(new["version"], channel) < version_tuple(old["version"], channel):
                    raise ValueError(f"{channel} fork version regression")
    if pending is not None and identity(pending) == identity(target):
        return False
    return identity(pin) != identity(target)


def materialize(pin, target, hashes):
    validate_pin(pin)
    if set(hashes) != {"upstream", *CHANNELS}:
        raise ValueError("all source and dependency hashes are required")
    result = {"version": target["version"], "revision": target["revision"]}
    for field in ("hash", "cargoHash", "pnpmDepsHash"):
        result[field] = hashes["upstream"][field]
    result["forks"] = {}
    for channel in CHANNELS:
        previous = pin["forks"][channel] if "forks" in pin else None
        new = target["forks"][channel]
        if previous:
            if version_tuple(new["version"], channel) < version_tuple(previous["version"], channel):
                raise ValueError(f"{channel} fork version regression")
            revision = previous["patchRevision"] + (identity(pin)["forks"][channel] != identity(target)["forks"][channel])
        elif channel == "nightly":
            revision = pin["forkPatchRevision"] + 1
        else:
            revision = 1
        result["forks"][channel] = {field: new[field] for field in PROMOTION_FIELDS | {"revision"} if field != "channel"}
        result["forks"][channel].update(hashes[channel])
        result["forks"][channel]["patchRevision"] = revision
    validate_pin(result)
    return result


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
    elif command == "materialize" and len(args) == 4:
        path, target_path, hash_path, old_path = map(Path, args)
        result = materialize(json.loads(old_path.read_text()), json.loads(target_path.read_text()),
                             json.loads(hash_path.read_text()))
        write_pin(path, result)
    else:
        raise ValueError("unknown command or arguments")


if __name__ == "__main__":
    main()
