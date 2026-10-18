#!/usr/bin/env python3
"""Git hook dispatcher that rejects configured email addresses in new commits,
and configured private terms in new commits to public repositories."""

import json
import os
import re
import subprocess
import sys
from collections import namedtuple
from pathlib import Path

IDENT_RE = re.compile(r"^[^<>]*<([^<>]+)> ")
ZERO_OID_RE = re.compile(r"^0+$")
GUARDED_HOOKS = {"pre-commit", "prepare-commit-msg", "commit-msg", "pre-push"}
URL_RE = re.compile(
    r"^[a-z][a-z0-9+.-]*://(?:[^@/]*@)?(?P<host>[^/:]+)(?::\d*)?/(?P<path>.*)$",
    re.IGNORECASE,
)
SCP_RE = re.compile(r"^(?:[^@/:]*@)?(?P<host>[^/:]+):(?P<path>.+)$")
HUNK_RE = re.compile(r"^(@{2,}) ")
SCISSORS_RE = re.compile(r"^\S+ -+ >8 -+$")
# Patches of every file as text, without colour, external drivers or
# configured prefixes.
PATCH_ARGS = (
    "-U0",
    "--text",
    "--no-color",
    "--no-ext-diff",
    "--no-textconv",
    "-M",
    "--src-prefix=a/",
    "--dst-prefix=b/",
)

Policy = namedtuple("Policy", "emails terms public")


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
        if ("privateTerms" in data) != ("publicRepositories" in data):
            raise ValueError("privateTerms and publicRepositories go together")
        terms = public = None
        if "privateTerms" in data:
            terms, public = data["privateTerms"], data["publicRepositories"]
            for entries in (terms, public):
                if not isinstance(entries, list) or not entries:
                    raise ValueError("empty or invalid private terms policy")
                if not all(isinstance(entry, str) and entry for entry in entries):
                    raise ValueError("invalid private terms policy entry")
            terms = tuple(re.compile(term, re.IGNORECASE) for term in terms)
            public = frozenset(repository_key(entry) for entry in public)
            if None in public:
                raise ValueError("invalid publicRepositories entry")
        return Policy(tuple(email.casefold() for email in emails), terms, public)
    except (OSError, ValueError, KeyError, TypeError, re.error) as exc:
        raise GuardError("identityGuard.policyFile is missing or invalid") from exc


def contains_blocked(text, emails):
    folded = text.casefold()
    return any(email in folded for email in emails)


def repository_key(url):
    """Return HOST/PATH for a remote URL or a policy entry, ignoring the
    scheme, user, port and a trailing .git; None for local paths."""
    url = url.strip()
    match = URL_RE.match(url) or SCP_RE.match(url)
    if match:
        host, path = match.group("host"), match.group("path")
    elif "/" in url and not url.startswith((".", "/", "~")):
        host, path = url.split("/", 1)
    else:
        return None
    path = path.strip("/").removesuffix(".git")
    return f"{host}/{path}".casefold()


def is_public(public, push_url=None):
    """Whether the push destination or any remote of the repository is public."""
    urls = [push_url] if push_url else []
    for name in git("remote").decode("utf-8", "replace").split():
        for args in (("--all",), ("--push", "--all")):
            try:
                urls += git("remote", "get-url", *args, name).decode().split()
            except GuardError:
                continue  # A remote without a URL cannot be public.
    return any(repository_key(url) in public for url in urls)


def contains_term(text, terms):
    return any(term.search(text) for term in terms)


def added_lines(patch):
    """Yield (path, text) for each line a patch adds. In a combined merge diff
    only lines missing from every parent count as added."""
    path = None
    columns = 0
    # Git separates patch lines with LF only; splitlines() would also split
    # inside a line at other line breaks and hide the rest of it.
    for line in patch.decode("utf-8", "replace").split("\n"):
        if line.startswith("diff "):
            path, columns = None, 0
        elif columns:
            if line[:columns] == "+" * columns:
                yield path, line[columns:]
        elif line.startswith("@@"):
            match = HUNK_RE.match(line)
            columns = len(match.group(1)) - 1 if match else 0
        elif line.startswith("+++ "):
            path = line[4:].removeprefix("b/")


def check_change(patch, names, terms, subject):
    """Check the added lines of a patch and NUL-separated `--name-status`
    output. A file counts as new when it is added against every parent."""
    fields = names.decode("utf-8", "replace").split("\0")
    for status, name in zip(fields[::2], fields[1::2]):
        if status and set(status) == {"A"} and contains_term(name, terms):
            raise GuardError(
                f"{subject} adds a file whose name contains a private term"
            )
    for path, text in added_lines(patch):
        if contains_term(text, terms):
            raise GuardError(f"{subject} adds a private term in {path}")


def check_staged(terms):
    merge_head = os.fsdecode(git("rev-parse", "--git-path", "MERGE_HEAD")).strip()
    if Path(merge_head).is_file():
        # The index also holds the other parent's lines; the push check sees
        # only what the merge adds beyond both parents.
        return
    patch = git("-c", "core.quotePath=false", "diff", "--cached", *PATCH_ARGS)
    names = git("diff", "--cached", "--name-status", "-z", "--no-renames")
    check_change(patch, names, terms, "the staged change")


def check_commit(oid, terms):
    # diff-tree ignores log.showRoot and signatures. A merge shows only lines
    # and files it adds beyond all its parents (--cc, -c).
    patch = git(
        "-c",
        "core.quotePath=false",
        "diff-tree",
        "--no-commit-id",
        "-r",
        "-p",
        "--root",
        "--cc",
        *PATCH_ARGS,
        oid,
    )
    names = git(
        "diff-tree",
        "--no-commit-id",
        "-r",
        "--root",
        "-c",
        "--name-status",
        "-z",
        "--no-renames",
        oid,
    )
    check_change(patch, names, terms, f"outgoing commit {oid[:12]}")


def message_body(message):
    """Return a commit message without comment lines and the verbose diff, as
    Git's default editor cleanup leaves it. Messages given with -m or -F keep
    their comment lines; the push check sees those."""
    lines = []
    for line in message.split("\n"):
        if SCISSORS_RE.match(line):
            break
        lines.append(line)
    return git(
        "stripspace", "--strip-comments", input_bytes="\n".join(lines).encode()
    ).decode("utf-8", "replace")


def current_ident(kind, emails, terms=None):
    ident = git("var", f"GIT_{kind}_IDENT").decode("utf-8", "replace")
    match = IDENT_RE.match(ident)
    if not match:
        raise GuardError("Git identity could not be read")
    if match.group(1).casefold() in emails:
        raise GuardError("configured email is used by the commit identity")
    if terms and contains_term(match.group(0), terms):
        raise GuardError("the commit identity contains a private term")


def message_file(path, emails, terms=None):
    try:
        message = Path(path).read_text(encoding="utf-8", errors="replace")
    except OSError as exc:
        raise GuardError("commit message could not be read") from exc
    if contains_blocked(message, emails):
        raise GuardError("configured email appears in the commit message")
    if terms and contains_term(message_body(message), terms):
        raise GuardError("the commit message contains a private term")


def published_boundary(remote_name, remote_url):
    """Return rev-list arguments excluding commits already on this remote.

    Only the pushed-to remote's tracking refs count, and only when the push
    uses its configured URL; URL pushes have no trustworthy tracking namespace.
    """
    try:
        configured_url = git("remote", "get-url", "--push", remote_name).decode().strip()
    except GuardError:
        return []
    if configured_url != remote_url:
        return []
    tracking = git("for-each-ref", "--format=%(objectname)", f"refs/remotes/{remote_name}")
    if not tracking.strip():
        return []
    # --remotes expands as positive revisions; --not negates them.
    return ["--not", f"--remotes={remote_name}"]


def outgoing_commits(remote_name, remote_url, lines):
    """Yield only commits added by the proposed ref updates."""
    seen = set()
    boundary = None
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

        if boundary is None:
            boundary = published_boundary(remote_name, remote_url)
        if ZERO_OID_RE.fullmatch(remote_oid):
            # A new ref has no old tip. Without this remote's known refs,
            # inspect full ancestry rather than silently assuming a base.
            revisions = [local_commit, *boundary]
        else:
            try:
                remote_commit = (
                    git("rev-parse", "--verify", f"{remote_oid}^{{commit}}")
                    .decode()
                    .strip()
                )
            except GuardError as exc:
                raise GuardError("fetch the remote ref before pushing") from exc
            # Commits already published on another branch of this remote, such
            # as main merged back into dev, are not newly published.
            revisions = [local_commit, f"^{remote_commit}", *boundary]

        for oid in git("rev-list", *revisions).decode().splitlines():
            if oid not in seen:
                seen.add(oid)
                yield oid


def check_push(args, stdin_data, emails, terms=None):
    if len(args) < 2:
        raise GuardError("push destination could not be read")
    lines = stdin_data.decode("utf-8", "replace")
    for line in lines.splitlines():
        fields = line.split()
        if len(fields) != 4:
            raise GuardError("push ref update could not be read")
        if not ZERO_OID_RE.fullmatch(fields[1]):
            if terms and contains_term(fields[2], terms):
                raise GuardError("a pushed ref name contains a private term")
            check_tag_chain(fields[1], emails, terms)
    for oid in outgoing_commits(args[0], args[1], lines):
        record = git("show", "-s", "--format=%ae%x00%ce%x00%B", oid).decode(
            "utf-8", "replace"
        )
        fields = record.split("\x00", 2)
        if len(fields) != 3:
            raise GuardError("commit metadata could not be read")
        if fields[0].casefold() in emails or fields[1].casefold() in emails:
            raise GuardError(
                f"outgoing commit {oid[:12]} uses a blocked identity email"
            )
        if contains_blocked(fields[2], emails):
            raise GuardError(
                f"outgoing commit {oid[:12]} message contains a blocked email"
            )
        if terms:
            if contains_term(fields[2], terms):
                raise GuardError(
                    f"outgoing commit {oid[:12]} message contains a private term"
                )
            idents = git("show", "-s", "--format=%an <%ae>%x00%cn <%ce>", oid)
            for ident in idents.decode("utf-8", "replace").strip().split("\0"):
                if contains_term(ident, terms):
                    raise GuardError(
                        f"outgoing commit {oid[:12]} identity contains a private term"
                    )
            check_commit(oid, terms)


def check_tag_chain(oid, emails, terms=None):
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
        name = ""
        # Split only at LF, as Git does; other line breaks can be in a name.
        for header in headers.split("\n"):
            if header.startswith("object "):
                target = header[7:]
            elif header.startswith("tagger "):
                tagger = header[7:]
            elif header.startswith("tag "):
                name = header[4:]
        if not target or not tagger:
            raise GuardError("tag metadata could not be read")
        match = IDENT_RE.match(tagger)
        if not match:
            raise GuardError("tag identity could not be read")
        if match.group(1).casefold() in emails:
            raise GuardError(f"outgoing tag {oid[:12]} uses a blocked tagger email")
        if contains_blocked(message, emails):
            raise GuardError(
                f"outgoing tag {oid[:12]} message contains a blocked email"
            )
        if terms and any(contains_term(text, terms) for text in (name, tagger, message)):
            raise GuardError(f"outgoing tag {oid[:12]} contains a private term")
        oid = target


def delegate(hook, args, stdin_data):
    hook_dir = configured_path("identityGuard.repositoryHooksPath")
    if hook_dir is None:
        try:
            common_dir = Path(os.fsdecode(git("rev-parse", "--git-common-dir")).strip())
        except GuardError:
            # Git invokes reference-transaction while initializing a repo,
            # before HEAD exists and rev-parse can recognize GIT_DIR. Git has
            # already created its hooks directory at that point.
            if hook in GUARDED_HOOKS:
                raise
            provisional_dir = os.environ.get("GIT_COMMON_DIR") or os.environ.get(
                "GIT_DIR"
            )
            if not provisional_dir:
                raise
            common_dir = Path(provisional_dir)
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
        guard = policy() if hook in GUARDED_HOOKS else None
        emails = guard.emails if guard else None
        terms = None
        if guard and guard.terms and hook in ("pre-commit", "commit-msg", "pre-push"):
            push_url = args[1] if hook == "pre-push" and len(args) > 1 else None
            if is_public(guard.public, push_url):
                terms = guard.terms
        if emails:
            if hook == "pre-commit":
                current_ident("AUTHOR", emails, terms)
                current_ident("COMMITTER", emails, terms)
                if terms:
                    check_staged(terms)
            elif hook == "prepare-commit-msg" or hook == "commit-msg":
                current_ident("AUTHOR", emails)
                current_ident("COMMITTER", emails)
                if args:
                    message_file(args[0], emails, terms)
            elif hook == "pre-push":
                check_push(args, stdin_data, emails, terms)
        result = delegate(hook, args, stdin_data)
        if (
            result == 0
            and emails
            and hook in ("prepare-commit-msg", "commit-msg")
            and args
        ):
            # Repository hooks may edit the message after the first check.
            message_file(args[0], emails, terms)
        return result
    except GuardError as exc:
        print(f"git-identity-guard: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main(sys.argv))
