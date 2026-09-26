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


class GuardTest(unittest.TestCase):
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
