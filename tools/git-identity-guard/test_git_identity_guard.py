#!/usr/bin/env python3
"""End-to-end contract tests with disposable Git repositories."""

import json
import os
import shlex
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

HOOKS = Path(sys.argv[1]) if len(sys.argv) > 1 else None
BLOCKED = "blocked@invalid.test"
SAFE = "safe@invalid.test"


class RepositoryCase(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.repo = self.root / "repo"
        self.repo.mkdir()
        self.home = self.root / "home"
        self.home.mkdir()
        self.env = os.environ.copy()
        self.env.update(
            HOME=str(self.home),
            XDG_CONFIG_HOME=str(self.home),
            GIT_CONFIG_NOSYSTEM="1",
            GIT_CONFIG_GLOBAL=os.devnull,
            GIT_CONFIG_SYSTEM=os.devnull,
        )
        for key in list(self.env):
            if key.startswith(
                ("GIT_AUTHOR_", "GIT_COMMITTER_", "GIT_CONFIG_")
            ) and key not in (
                "GIT_CONFIG_NOSYSTEM",
                "GIT_CONFIG_GLOBAL",
                "GIT_CONFIG_SYSTEM",
            ):
                del self.env[key]
        self.run_git("init", "-q", "-b", "main")
        self.run_git("config", "user.name", "Example User")
        self.run_git("config", "user.email", SAFE)
        self.policy = self.root / "policy.json"
        self.policy.write_text(json.dumps({"blockedEmails": [BLOCKED]}))
        self.run_git("config", "identityGuard.policyFile", str(self.policy))
        self.run_git("config", "core.hooksPath", str(HOOKS))
        self.commit("base")

    def command(self, *args, input_data=None, env=None):
        return subprocess.run(
            args,
            cwd=self.repo,
            env=env or self.env,
            input=input_data,
            capture_output=True,
            check=False,
        )

    def run_git(self, *args, ok=True, env=None):
        result = self.command("git", *args, env=env)
        if ok and result.returncode:
            self.fail(f"git {args[0]} failed: {result.stderr.decode(errors='replace')}")
        return result

    def commit(self, message, *, env=None, bypass=False):
        marker = self.repo / "marker"
        marker.write_text(marker.read_text() + "x" if marker.exists() else "x")
        self.run_git("add", "marker")
        args = ["commit", "-qm", message]
        if bypass:
            args = ["-c", "core.hooksPath=/dev/null", *args]
        return self.run_git(*args, ok=False, env=env)

    def bad_env(self):
        env = self.env.copy()
        env["GIT_AUTHOR_EMAIL"] = BLOCKED
        env["GIT_COMMITTER_EMAIL"] = BLOCKED
        return env

    def hook(self, hook, *args, input_data=b""):
        return self.command(str(HOOKS / hook), *args, input_data=input_data)

    def oid(self, rev="HEAD"):
        return self.run_git("rev-parse", rev).stdout.decode().strip()


class GuardTest(RepositoryCase):
    def test_commit_identity_and_messages(self):
        author_only = self.env.copy()
        author_only["GIT_AUTHOR_EMAIL"] = BLOCKED
        self.assertEqual(self.commit("bad author", env=author_only).returncode, 1)
        committer_only = self.env.copy()
        committer_only["GIT_COMMITTER_EMAIL"] = BLOCKED
        self.assertEqual(self.commit("bad committer", env=committer_only).returncode, 1)
        self.assertEqual(self.commit(f"Contact {BLOCKED}").returncode, 1)
        self.assertEqual(self.commit("safe message").returncode, 0)
        self.assertEqual(
            self.run_git(
                "commit",
                "--amend",
                "--no-edit",
                "--no-verify",
                ok=False,
                env=self.bad_env(),
            ).returncode,
            1,
        )

    def test_amend_preserves_old_author_and_cherry_pick_is_caught_on_push(self):
        self.assertEqual(
            self.commit("old author", env=self.bad_env(), bypass=True).returncode, 0
        )
        self.assertEqual(
            self.run_git("commit", "--amend", "--no-edit", ok=False).returncode, 1
        )
        bad = self.oid()
        self.run_git("reset", "--hard", "HEAD~1")
        base = self.oid()
        # Git cherry-pick does not invoke commit hooks in this path.
        self.assertEqual(self.run_git("cherry-pick", bad, ok=False).returncode, 0)
        row = f"refs/heads/main {self.oid()} refs/heads/main {base}\n".encode()
        self.assertEqual(
            self.hook("pre-push", "origin", "unused", input_data=row).returncode, 1
        )

    def test_delegate_default_and_custom_path(self):
        original = self.repo / ".git/hooks/pre-commit"
        original.write_text("#!/bin/sh\nexit 37\n")
        original.chmod(0o755)
        self.assertEqual(self.hook("pre-commit").returncode, 37)
        self.assertNotEqual(self.commit("delegated").returncode, 0)
        original.unlink()
        custom = self.repo / "custom-hooks"
        custom.mkdir()
        custom_hook = custom / "pre-commit"
        custom_hook.write_text("#!/bin/sh\nexit 38\n")
        custom_hook.chmod(0o755)
        self.run_git("config", "identityGuard.repositoryHooksPath", "custom-hooks")
        self.assertEqual(self.hook("pre-commit").returncode, 38)
        self.assertNotEqual(self.commit("custom delegated").returncode, 0)

    def test_linked_worktree_delegates_common_dir_hook(self):
        linked = self.root / "linked"
        self.run_git("worktree", "add", "-q", "-b", "linked", str(linked))
        original = self.repo / ".git/hooks/pre-commit"
        marker = self.root / "linked-hook-ran"
        original.write_text(f"#!/bin/sh\ntouch {shlex.quote(str(marker))}\nexit 41\n")
        original.chmod(0o755)
        result = subprocess.run(
            [str(HOOKS / "pre-commit")],
            cwd=linked,
            env=self.env,
            capture_output=True,
            check=False,
        )
        self.assertEqual(result.returncode, 41)
        self.assertTrue(marker.exists())

    def test_delegate_rechecks_message_modified_by_hook(self):
        hook = self.repo / ".git/hooks/commit-msg"
        hook.write_text(f"#!/bin/sh\nprintf '\\nEmail: {BLOCKED}\\n' >> \"$1\"\n")
        hook.chmod(0o755)
        self.assertEqual(self.commit("safe initial message").returncode, 1)

    def test_policy_missing_fails_closed_and_unset_delegates(self):
        self.policy.unlink()
        self.assertEqual(self.commit("missing policy").returncode, 1)
        self.run_git("config", "--unset", "identityGuard.policyFile")
        self.assertEqual(
            self.commit("unconfigured policy", env=self.bad_env()).returncode, 0
        )

    def test_pre_push_existing_and_new_ref(self):
        base = self.oid()
        self.assertEqual(
            self.commit("bad old author", env=self.bad_env(), bypass=True).returncode, 0
        )
        bad = self.oid()
        row = f"refs/heads/main {bad} refs/heads/main {base}\n".encode()
        rejected = self.hook("pre-push", "origin", "unused", input_data=row)
        self.assertEqual(rejected.returncode, 1)
        self.assertIn(bad[:12].encode(), rejected.stderr)
        self.assertNotIn(BLOCKED.encode(), rejected.stderr)
        self.assertEqual(
            self.hook(
                "pre-push",
                "origin",
                "unused",
                input_data=f"refs/heads/main {bad} refs/heads/new {'0' * 40}\n".encode(),
            ).returncode,
            1,
        )
        # Ref deletion is safe even when the local branch has bad history.
        deletion = f"(delete) {'0' * 40} refs/heads/main {base}\n".encode()
        self.assertEqual(
            self.hook("pre-push", "origin", "unused", input_data=deletion).returncode, 0
        )

    def test_new_ref_uses_only_configured_remote_boundary(self):
        self.assertEqual(
            self.commit(
                "published old author", env=self.bad_env(), bypass=True
            ).returncode,
            0,
        )
        remote = self.root / "remote.git"
        self.run_git("init", "--bare", "-q", str(remote))
        self.run_git("remote", "add", "origin", str(remote))
        self.run_git("-c", "core.hooksPath=/dev/null", "push", "-q", "origin", "main")
        self.run_git("fetch", "-q", "origin")
        self.assertEqual(self.commit("new safe work").returncode, 0)
        row = f"refs/heads/main {self.oid()} refs/heads/new {'0' * 40}\n".encode()
        self.assertEqual(
            self.hook("pre-push", "origin", str(remote), input_data=row).returncode, 0
        )
        # URL pushes do not borrow origin's refs; full ancestry is checked.
        self.assertEqual(
            self.hook("pre-push", str(remote), str(remote), input_data=row).returncode,
            1,
        )

    def test_existing_ref_skips_commits_published_on_other_remote_refs(self):
        remote = self.root / "remote.git"
        self.run_git("init", "--bare", "-q", str(remote))
        self.run_git("remote", "add", "origin", str(remote))
        self.run_git("push", "-q", "origin", "main:main", "main:dev")
        self.assertEqual(
            self.commit(
                "published old author", env=self.bad_env(), bypass=True
            ).returncode,
            0,
        )
        self.run_git("-c", "core.hooksPath=/dev/null", "push", "-q", "origin", "main")
        self.run_git("fetch", "-q", "origin")
        dev_tip = (
            self.run_git("rev-parse", "origin/dev").stdout.decode().strip()
        )
        # Merging published main back into dev sends no new bad commits.
        row = f"refs/heads/main {self.oid()} refs/heads/dev {dev_tip}\n".encode()
        self.assertEqual(
            self.hook("pre-push", "origin", str(remote), input_data=row).returncode, 0
        )
        # URL pushes still check everything missing from the old tip.
        self.assertEqual(
            self.hook("pre-push", str(remote), str(remote), input_data=row).returncode,
            1,
        )
        # New bad commits are still rejected on an existing ref.
        self.assertEqual(
            self.commit("unpublished old author", env=self.bad_env(), bypass=True).returncode,
            0,
        )
        row = f"refs/heads/main {self.oid()} refs/heads/dev {dev_tip}\n".encode()
        self.assertEqual(
            self.hook("pre-push", "origin", str(remote), input_data=row).returncode, 1
        )

    def test_real_push_rejects_old_author_and_keeps_remote_tip(self):
        remote = self.root / "remote.git"
        self.run_git("init", "--bare", "-q", str(remote))
        self.run_git("remote", "add", "origin", str(remote))
        self.run_git("push", "-q", "origin", "main")
        published = self.oid()
        self.run_git("fetch", "-q", "origin")
        author_only = self.env.copy()
        author_only["GIT_AUTHOR_EMAIL"] = BLOCKED
        self.assertEqual(
            self.commit(
                "cherry-picked style author", env=author_only, bypass=True
            ).returncode,
            0,
        )
        self.assertNotEqual(
            self.run_git("push", "origin", "main", ok=False).returncode, 0
        )
        tip = (
            self.run_git("--git-dir", str(remote), "rev-parse", "refs/heads/main")
            .stdout.decode()
            .strip()
        )
        self.assertEqual(tip, published)

    def test_pre_push_url_clean_history_and_stdin_replay(self):
        output = self.root / "push-input"
        hook = self.repo / ".git/hooks/pre-push"
        hook.write_text(f"#!/bin/sh\ncat > {shlex.quote(str(output))}\nexit 39\n")
        hook.chmod(0o755)
        row = f"refs/heads/main {self.oid()} refs/heads/new {'0' * 40}\n".encode()
        self.assertEqual(
            self.hook("pre-push", "some-url", "some-url", input_data=row).returncode, 39
        )
        self.assertEqual(output.read_bytes(), row)
        custom = self.repo / "custom-hooks"
        custom.mkdir()
        custom_hook = custom / "pre-push"
        custom_hook.write_text(
            f"#!/bin/sh\ncat > {shlex.quote(str(output))}\nexit 40\n"
        )
        custom_hook.chmod(0o755)
        self.run_git("config", "identityGuard.repositoryHooksPath", "custom-hooks")
        self.assertEqual(
            self.hook("pre-push", "some-url", "some-url", input_data=row).returncode, 40
        )
        self.assertEqual(output.read_bytes(), row)

    def test_existing_remote_object_must_be_local(self):
        row = f"refs/heads/main {self.oid()} refs/heads/main {'a' * 40}\n".encode()
        self.assertEqual(
            self.hook("pre-push", "origin", "unused", input_data=row).returncode, 1
        )

    def test_annotated_tag_identity_message_and_noncommit_target(self):
        bad_tagger = self.bad_env()
        self.run_git("tag", "-a", "bad-tagger", "-m", "safe", env=bad_tagger)
        tag_oid = self.oid("refs/tags/bad-tagger")
        row = (
            f"refs/tags/bad-tagger {tag_oid} refs/tags/bad-tagger {'0' * 40}\n".encode()
        )
        self.assertEqual(
            self.hook("pre-push", "origin", "unused", input_data=row).returncode, 1
        )
        self.run_git("tag", "-a", "bad-message", "-m", f"Contact {BLOCKED}")
        tag_oid = self.oid("refs/tags/bad-message")
        row = f"refs/tags/bad-message {tag_oid} refs/tags/bad-message {'0' * 40}\n".encode()
        self.assertEqual(
            self.hook("pre-push", "origin", "unused", input_data=row).returncode, 1
        )
        blob = self.oid("HEAD:marker")
        self.run_git("tag", "-a", "blob-tag", "-m", "safe", blob)
        tag_oid = self.oid("refs/tags/blob-tag")
        row = f"refs/tags/blob-tag {tag_oid} refs/tags/blob-tag {'0' * 40}\n".encode()
        self.assertEqual(
            self.hook("pre-push", "origin", "unused", input_data=row).returncode, 0
        )


PUBLIC_URL = "https://git.example.test/owner/public.git"


class PrivateTermsTest(RepositoryCase):
    def setUp(self):
        super().setUp()
        self.policy.write_text(
            json.dumps(
                {
                    "blockedEmails": [BLOCKED],
                    "privateTerms": ["example-corp", r"\bacme-[a-z]"],
                    "publicRepositories": ["git.example.test/owner/public"],
                }
            )
        )
        self.run_git("remote", "add", "public", PUBLIC_URL)

    def change(self, name, text, message="safe change", *, bypass=False):
        path = self.repo / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text)
        self.run_git("add", "-A")
        args = ["commit", "-qm", message]
        if bypass:
            args = ["-c", "core.hooksPath=/dev/null", *args]
        return self.run_git(*args, ok=False)

    def test_public_repository_rejects_added_terms(self):
        rejected = self.change("notes.md", "Owned by Example-Corp\n")
        self.assertEqual(rejected.returncode, 1)
        self.assertIn(b"notes.md", rejected.stderr)
        self.assertNotIn(b"example-corp", rejected.stderr.lower())
        self.run_git("reset", "-q", "--hard")
        self.assertEqual(self.change("acme-tools/readme", "safe\n").returncode, 1)
        self.run_git("reset", "-q", "--hard")
        self.assertEqual(
            self.change("notes.md", "x", "Port from acme-billing").returncode, 1
        )
        self.run_git("reset", "-q", "--hard")
        # Word boundaries in a term keep unrelated words out.
        self.assertEqual(self.change("notes.md", "macme-x\n").returncode, 0)

    def test_removing_terms_and_comment_lines_are_allowed(self):
        self.assertEqual(
            self.change("notes.md", "keep\nexample-corp\n", bypass=True).returncode, 0
        )
        self.assertEqual(
            self.change("notes.md", "keep\n", "Remove old name").returncode, 0
        )
        message = self.root / "message"
        message.write_text("safe\n# example-corp in a comment\n")
        self.assertEqual(self.hook("commit-msg", str(message)).returncode, 0)
        message.write_text(
            "safe\n# ------------------------ >8 ------------------------\nexample-corp\n"
        )
        self.assertEqual(self.hook("commit-msg", str(message)).returncode, 0)

    def test_scp_remote_counts_as_public_and_private_repository_is_skipped(self):
        self.run_git("remote", "set-url", "public", "git@GIT.example.test:owner/public")
        self.assertEqual(self.change("notes.md", "example-corp\n").returncode, 1)
        self.run_git("reset", "-q", "--hard")
        self.run_git(
            "remote", "set-url", "public", "https://git.example.test/owner/private.git"
        )
        self.assertEqual(self.change("notes.md", "example-corp\n").returncode, 0)

    def test_push_checks_new_commits_but_not_published_merges(self):
        remote = self.root / "remote.git"
        self.run_git("init", "--bare", "-q", str(remote))
        self.run_git("remote", "add", "origin", str(remote))
        self.run_git("push", "-q", "origin", "main")
        self.run_git("checkout", "-q", "-b", "side")
        self.assertEqual(
            self.change("side.md", "example-corp\n", bypass=True).returncode, 0
        )
        self.run_git("-c", "core.hooksPath=/dev/null", "push", "-q", "origin", "side")
        self.run_git("checkout", "-q", "main")
        self.assertEqual(self.change("main.md", "safe\n").returncode, 0)
        self.run_git("merge", "-q", "--no-edit", "side")
        self.run_git("fetch", "-q", "origin")
        base = self.run_git("rev-parse", "origin/main").stdout.decode().strip()
        row = f"refs/heads/main {self.oid()} refs/heads/main {base}\n".encode()
        # The side commit is already published; the merge adds nothing new.
        self.assertEqual(
            self.hook("pre-push", "origin", str(remote), input_data=row).returncode, 0
        )
        self.assertEqual(self.change("late.md", "acme-x\n", bypass=True).returncode, 0)
        row = f"refs/heads/main {self.oid()} refs/heads/main {base}\n".encode()
        rejected = self.hook("pre-push", "origin", str(remote), input_data=row)
        self.assertEqual(rejected.returncode, 1)
        self.assertIn(b"late.md", rejected.stderr)
        self.assertNotEqual(
            self.run_git("push", "origin", "main", ok=False).returncode, 0
        )

    def test_empty_binary_and_undiffable_additions_are_checked(self):
        self.assertEqual(self.change("acme-tools/.gitkeep", "").returncode, 1)
        self.run_git("reset", "-q", "--hard")
        self.assertEqual(self.change("logo.bin", "\0example-corp\0").returncode, 1)
        self.run_git("reset", "-q", "--hard")
        self.assertEqual(
            self.change(".gitattributes", "notes.md -diff\n").returncode, 0
        )
        self.assertEqual(self.change("notes.md", "example-corp\n").returncode, 1)
        self.run_git("reset", "-q", "--hard")
        # Git splits patch lines only at LF.
        self.assertEqual(
            self.change("notes.md", "safe\u2028example-corp\n").returncode, 1
        )

    def test_absolute_scp_remote_counts_as_public(self):
        self.run_git(
            "remote", "set-url", "public", "git@git.example.test:/owner/public.git"
        )
        self.assertEqual(self.change("notes.md", "example-corp\n").returncode, 1)

    def test_comment_lines_given_with_m_are_caught_on_push(self):
        self.assertEqual(
            self.change("notes.md", "x", "Safe\n\n# example-corp").returncode, 0
        )
        row = f"refs/heads/main {self.oid()} refs/heads/main {self.oid('HEAD~1')}\n".encode()
        self.assertEqual(
            self.hook("pre-push", "public", PUBLIC_URL, input_data=row).returncode, 1
        )

    def test_pushed_ref_names_are_checked_but_deletions_allowed(self):
        row = f"refs/heads/main {self.oid()} refs/heads/acme-x {'0' * 40}\n".encode()
        self.assertEqual(
            self.hook("pre-push", "public", PUBLIC_URL, input_data=row).returncode, 1
        )
        row = f"(delete) {'0' * 40} refs/heads/acme-x {self.oid()}\n".encode()
        self.assertEqual(
            self.hook("pre-push", "public", PUBLIC_URL, input_data=row).returncode, 0
        )

    def test_merge_resolution_keeps_inherited_terms_but_not_new_ones(self):
        self.assertEqual(self.change("f.md", "a\nb\n").returncode, 0)
        remote = self.root / "remote.git"
        self.run_git("init", "--bare", "-q", str(remote))
        self.run_git("remote", "add", "origin", str(remote))
        self.run_git("push", "-q", "origin", "main")
        self.run_git("checkout", "-q", "-b", "side")
        self.assertEqual(
            self.change("f.md", "a\nexample-corp\n", bypass=True).returncode, 0
        )
        self.run_git("-c", "core.hooksPath=/dev/null", "push", "-q", "origin", "side")
        self.run_git("checkout", "-q", "main")
        self.assertEqual(self.change("f.md", "a\nmain\n").returncode, 0)
        self.run_git("fetch", "-q", "origin")
        base = self.run_git("rev-parse", "origin/main").stdout.decode().strip()
        for text, new_file, expected in (
            ("a\nexample-corp\nmain\n", None, 0),
            ("a\nexample-corp\nacme-x\n", None, 1),
            ("a\nexample-corp\nmain\n", "acme-tools/.gitkeep", 1),
        ):
            self.run_git("merge", "-q", "side", ok=False)
            (self.repo / "f.md").write_text(text)
            if new_file:
                (self.repo / new_file).parent.mkdir(exist_ok=True)
                (self.repo / new_file).write_text("")
            self.run_git("add", "-A")
            # Committing the merge is not blamed for the other parent's lines.
            self.run_git("commit", "-q", "--no-edit")
            row = f"refs/heads/main {self.oid()} refs/heads/main {base}\n".encode()
            result = self.hook("pre-push", "origin", str(remote), input_data=row)
            self.assertEqual(result.returncode, expected, text)
            self.run_git("reset", "-q", "--hard", "HEAD~1")

    def test_identities_and_tag_names_are_checked(self):
        corp = self.env.copy()
        corp["GIT_AUTHOR_EMAIL"] = "dev@example-corp.test"
        self.assertEqual(self.commit("safe", env=corp).returncode, 1)
        self.assertEqual(self.commit("safe", env=corp, bypass=True).returncode, 0)
        row = f"refs/heads/main {self.oid()} refs/heads/main {self.oid('HEAD~1')}\n".encode()
        self.assertEqual(
            self.hook("pre-push", "public", PUBLIC_URL, input_data=row).returncode, 1
        )
        self.run_git("reset", "-q", "--hard", "HEAD~1")
        for name in ("example-corp-release", "safe\u2028example-corp"):
            self.run_git("tag", "-a", name, "-m", "safe")
            tag = self.oid(f"refs/tags/{name}")
            row = f"refs/tags/x {tag} refs/tags/release {'0' * 40}\n".encode()
            self.assertEqual(
                self.hook("pre-push", "public", PUBLIC_URL, input_data=row).returncode,
                1,
                name,
            )

    def test_anchored_terms_see_each_field_alone(self):
        self.policy.write_text(
            json.dumps(
                {
                    "blockedEmails": [BLOCKED],
                    "privateTerms": ["^example-corp"],
                    "publicRepositories": ["git.example.test/owner/public"],
                }
            )
        )
        corp = self.env.copy()
        corp["GIT_COMMITTER_NAME"] = "example-corp Dev"
        self.assertEqual(self.commit("safe", env=corp, bypass=True).returncode, 0)
        row = f"refs/heads/main {self.oid()} refs/heads/main {self.oid('HEAD~1')}\n".encode()
        self.assertEqual(
            self.hook("pre-push", "public", PUBLIC_URL, input_data=row).returncode, 1
        )
        self.run_git("reset", "-q", "--hard", "HEAD~1")
        self.run_git("tag", "-a", "release", "-m", "example-corp confidential")
        tag = self.oid("refs/tags/release")
        row = f"refs/tags/release {tag} refs/tags/release {'0' * 40}\n".encode()
        self.assertEqual(
            self.hook("pre-push", "public", PUBLIC_URL, input_data=row).returncode, 1
        )

    def test_ref_named_merge_head_does_not_skip_commit_checks(self):
        self.run_git("tag", "MERGE_HEAD")
        self.assertEqual(self.change("notes.md", "example-corp\n").returncode, 1)

    def test_terms_policy_must_be_complete_and_valid(self):
        for extra in (
            {"privateTerms": ["example-corp"]},
            {"publicRepositories": ["git.example.test/owner/public"]},
            {
                "privateTerms": [],
                "publicRepositories": ["git.example.test/owner/public"],
            },
            {
                "privateTerms": ["("],
                "publicRepositories": ["git.example.test/owner/public"],
            },
            {"privateTerms": ["example-corp"], "publicRepositories": ["public"]},
            {"privateTerms": None},
            {"privateTerms": None, "publicRepositories": None},
        ):
            self.policy.write_text(json.dumps({"blockedEmails": [BLOCKED], **extra}))
            self.assertEqual(self.commit("safe").returncode, 1, extra)


class GlobalHooksTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        policy = self.root / "policy.json"
        policy.write_text(json.dumps({"blockedEmails": [BLOCKED]}))
        self.config = self.root / "gitconfig"
        self.config.write_text(
            f"[core]\n hooksPath = {HOOKS}\n"
            f"[identityGuard]\n policyFile = {policy}\n"
            "[user]\n name = Example User\n email = safe@invalid.test\n"
        )
        self.env = os.environ.copy()
        for key in list(self.env):
            if key.startswith(
                ("GIT_AUTHOR_", "GIT_COMMITTER_", "GIT_CONFIG_")
            ) or key in (
                "GIT_DIR",
                "GIT_WORK_TREE",
                "GIT_COMMON_DIR",
            ):
                del self.env[key]
        self.env.update(
            HOME=str(self.root),
            XDG_CONFIG_HOME=str(self.root),
            GIT_CONFIG_GLOBAL=str(self.config),
            GIT_CONFIG_NOSYSTEM="1",
            GIT_CONFIG_SYSTEM=os.devnull,
        )

    def git(self, *args, cwd=None):
        result = subprocess.run(
            ["git", *map(str, args)],
            cwd=cwd or self.root,
            env=self.env,
            capture_output=True,
            check=False,
        )
        self.assertEqual(result.returncode, 0, result.stderr.decode(errors="replace"))
        return result

    def test_global_hooks_allow_init_outside_and_inside_repository_and_clone(self):
        source = self.root / "source"
        self.git("init", "-q", source)
        self.git("init", "-q", "nested", cwd=source)
        (source / "file").write_text("content")
        self.git("-C", source, "add", "file")
        self.git("-C", source, "commit", "-qm", "safe")
        clone = self.root / "clone"
        self.git("clone", "-q", source, clone)
        self.assertTrue((clone / "file").exists())

    def test_global_init_delegates_template_reference_transaction_hook(self):
        template = self.root / "template"
        (template / "hooks").mkdir(parents=True)
        marker = self.root / "template-hook-ran"
        hook = template / "hooks/reference-transaction"
        hook.write_text(f"#!/bin/sh\ntouch {shlex.quote(str(marker))}\n")
        hook.chmod(0o755)
        self.git("config", "--file", self.config, "init.templateDir", template)
        self.git("init", "-q", self.root / "templated")
        self.assertTrue(marker.exists())


if __name__ == "__main__":
    if HOOKS is None:
        raise SystemExit("usage: test_git_identity_guard.py PACKAGE_HOOKS_DIR")
    unittest.main(argv=[sys.argv[0]])
