#!/usr/bin/env python3
"""Git hook dispatcher that rejects configured email addresses in new commits."""

import json
import os
import re
import subprocess
import sys
from pathlib import Path

IDENT_RE = re.compile(r"^[^<>]*<([^<>]+)> ")
ZERO_OID_RE = re.compile(r"^0+$")


class GuardError(Exception):
    pass


def git(*args, input_bytes=None):
    try:
        result = subprocess.run(
            ["git", *args],
            input=input_bytes,
            capture_output=True,
            check=False,
        )
    except OSError as exc:
        raise GuardError("Git is unavailable") from exc
    if result.returncode:
        raise GuardError("Git could not inspect repository state")
    return result.stdout


def configured_path(key):
    result = subprocess.run(
        ["git", "config", "--path", "--get", key],
        capture_output=True,
        check=False,
    )
    if result.returncode == 1 and not result.stdout:
        return None
    if result.returncode:
        raise GuardError("Git hook configuration could not be read")
    return Path(os.fsdecode(result.stdout).strip())


def policy():
    policy_path = configured_path("identityGuard.policyFile")
    if policy_path is None:
        return None
    try:
        data = json.loads(policy_path.read_text(encoding="utf-8"))
        emails = data["blockedEmails"]
        if not isinstance(emails, list) or not emails:
            raise ValueError("empty or invalid blockedEmails")
        if not all(
            isinstance(email, str) and email and "@" in email for email in emails
        ):
            raise ValueError("invalid blockedEmails entry")
        return tuple(email.casefold() for email in emails)
    except (OSError, ValueError, KeyError, TypeError) as exc:
        raise GuardError("identityGuard.policyFile is missing or invalid") from exc


def contains_blocked(text, emails):
    folded = text.casefold()
    return any(email in folded for email in emails)


def current_ident(kind, emails):
    ident = git("var", f"GIT_{kind}_IDENT").decode("utf-8", "replace")
    match = IDENT_RE.match(ident)
    if not match:
        raise GuardError("Git identity could not be read")
    if match.group(1).casefold() in emails:
        raise GuardError("configured email is used by the commit identity")


def message_file(path, emails):
    try:
        message = Path(path).read_text(encoding="utf-8", errors="replace")
    except OSError as exc:
        raise GuardError("commit message could not be read") from exc
    if contains_blocked(message, emails):
        raise GuardError("configured email appears in the commit message")


def outgoing_commits(remote_name, remote_url, lines):
    """Yield only commits added by the proposed ref updates."""
    seen = set()
    for line in lines.splitlines():
        fields = line.split()
        if len(fields) != 4:
            raise GuardError("push ref update could not be read")
        _local_ref, local_oid, _remote_ref, remote_oid = fields
        if ZERO_OID_RE.fullmatch(local_oid):
            continue  # Ref deletion sends no commits.
        try:
            local_commit = (
                git("rev-parse", "--verify", f"{local_oid}^{{commit}}").decode().strip()
            )
        except GuardError:
            # Trees and blobs can be pushed as tags, but contain no commits.
            peeled = git("rev-parse", "--verify", f"{local_oid}^{{}}").decode().strip()
            if git("cat-file", "-t", peeled).strip() in (b"tree", b"blob"):
                continue
            raise

        if ZERO_OID_RE.fullmatch(remote_oid):
            # A new ref has no old tip. Use this remote's known refs as the
            # published boundary, never refs belonging to a different remote.
            configured_url = None
            try:
                configured_url = (
                    git("remote", "get-url", "--push", remote_name).decode().strip()
                )
            except GuardError:
                pass
            if configured_url == remote_url:
                tracking = git(
                    "for-each-ref",
                    "--format=%(objectname)",
                    f"refs/remotes/{remote_name}",
                )
                if tracking.strip():
                    # --remotes expands as positive revisions; --not negates them.
                    revisions = [local_commit, "--not", f"--remotes={remote_name}"]
                else:
                    revisions = [local_commit]
            else:
                # URL pushes have no trustworthy tracking namespace. Inspect
                # full ancestry rather than silently assuming a remote base.
                revisions = [local_commit]
        else:
            try:
                remote_commit = (
                    git("rev-parse", "--verify", f"{remote_oid}^{{commit}}")
                    .decode()
                    .strip()
                )
            except GuardError as exc:
                raise GuardError("fetch the remote ref before pushing") from exc
            revisions = [local_commit, f"^{remote_commit}"]

        for oid in git("rev-list", *revisions).decode().splitlines():
            if oid not in seen:
                seen.add(oid)
                yield oid


def check_push(args, stdin_data, emails):
    if len(args) < 2:
        raise GuardError("push destination could not be read")
    lines = stdin_data.decode("utf-8", "replace")
    for line in lines.splitlines():
        fields = line.split()
        if len(fields) != 4:
            raise GuardError("push ref update could not be read")
        if not ZERO_OID_RE.fullmatch(fields[1]):
            check_tag_chain(fields[1], emails)
    for oid in outgoing_commits(args[0], args[1], lines):
        record = git("show", "-s", "--format=%ae%x00%ce%x00%B", oid).decode(
            "utf-8", "replace"
        )
        fields = record.split("\x00", 2)
        if len(fields) != 3:
            raise GuardError("commit metadata could not be read")
        if fields[0].casefold() in emails or fields[1].casefold() in emails:
            raise GuardError("configured email is used by an outgoing commit identity")
        if contains_blocked(fields[2], emails):
            raise GuardError("configured email appears in an outgoing commit message")


def check_tag_chain(oid, emails):
    """Check annotated tag objects, including tags that point to other tags."""
    seen = set()
    while True:
        if oid in seen:
            raise GuardError("tag object chain could not be read")
        seen.add(oid)
        kind = git("cat-file", "-t", oid).strip()
        if kind != b"tag":
            return
        payload = git("cat-file", "-p", oid).decode("utf-8", "replace")
        headers, separator, message = payload.partition("\n\n")
        if not separator:
            raise GuardError("tag object could not be read")
        target = None
        tagger = None
        for header in headers.splitlines():
            if header.startswith("object "):
                target = header[7:]
            elif header.startswith("tagger "):
                tagger = header[7:]
        if not target or not tagger:
            raise GuardError("tag metadata could not be read")
        match = IDENT_RE.match(tagger)
        if not match:
            raise GuardError("tag identity could not be read")
        if match.group(1).casefold() in emails:
            raise GuardError("configured email is used by an outgoing tag identity")
        if contains_blocked(message, emails):
            raise GuardError("configured email appears in an outgoing tag message")
        oid = target


def delegate(hook, args, stdin_data):
    hook_dir = configured_path("identityGuard.repositoryHooksPath")
    if hook_dir is None:
        common_dir = Path(os.fsdecode(git("rev-parse", "--git-common-dir")).strip())
        hook_dir = common_dir / "hooks"
    active_dir = configured_path("core.hooksPath")
    if active_dir is not None and hook_dir.resolve() == active_dir.resolve():
        raise GuardError("repository hooks path points to the dispatcher")
    hook_path = hook_dir / hook
    if not hook_path.is_file() or not os.access(hook_path, os.X_OK):
        return 0
    if stdin_data is None:
        return subprocess.run([str(hook_path), *args], check=False).returncode
    return subprocess.run(
        [str(hook_path), *args], input=stdin_data, check=False
    ).returncode


def main(argv):
    if len(argv) < 2 or argv[1] != "hook" or len(argv) < 3:
        print("usage: git-identity-guard hook HOOK [ARGS...]", file=sys.stderr)
        return 2
    hook, args = argv[2], argv[3:]
    if "/" in hook or hook in (".", ".."):
        return 2
    stdin_data = sys.stdin.buffer.read() if hook == "pre-push" else None
    try:
        emails = policy()
        if emails:
            if hook == "pre-commit":
                current_ident("AUTHOR", emails)
                current_ident("COMMITTER", emails)
            elif hook == "prepare-commit-msg" or hook == "commit-msg":
                current_ident("AUTHOR", emails)
                current_ident("COMMITTER", emails)
                if args:
                    message_file(args[0], emails)
            elif hook == "pre-push":
                check_push(args, stdin_data, emails)
        result = delegate(hook, args, stdin_data)
        if (
            result == 0
            and emails
            and hook in ("prepare-commit-msg", "commit-msg")
            and args
        ):
            # Repository hooks may edit the message after the first check.
            message_file(args[0], emails)
        return result
    except GuardError as exc:
        print(f"git-identity-guard: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main(sys.argv))
