#!/usr/bin/env python3
"""Offline checks for the cheap grouped AI release probe."""

import base64
import importlib.util
import os
from pathlib import Path
import sys
import tempfile
import unittest
from unittest import mock
import urllib.error


sys.dont_write_bytecode = True
spec = importlib.util.spec_from_file_location(
    "probe_ai_updates", Path(__file__).parents[1] / "update-packages/probe-ai-updates.py"
)
probe = importlib.util.module_from_spec(spec)
spec.loader.exec_module(probe)

OLD = {"claude-code": "2.1.1", "codex-cli": "0.159.0", "codex-app-server": "0.159.0"}
NEW = {"claude-code": "2.1.2", "codex-cli": "0.159.1", "codex-app-server": "0.159.1"}
SHA = "a" * 40


class ProbeTests(unittest.TestCase):
    def setUp(self):
        self.environment = mock.patch.dict(os.environ, {
            "FORGEJO_URL": "https://forge.example",
            "FORGEJO_OWNER": "owner",
            "FORGEJO_REPO": "packages",
        })
        self.environment.start()
        self.addCleanup(self.environment.stop)

    def responses(self, upstream=NEW, pending=None, open_pr=True):
        pr = {"state": "open", "base": {"ref": "dev"},
              "head": {"ref": probe.BRANCH, "sha": SHA}}
        def get(url):
            if url.endswith("/pulls?state=open&base=dev&limit=100&page=1"):
                return [pr] if open_pr else [pr | {"state": "closed"}]
            if "/contents/" in url:
                name = next(name for name, (path, _) in probe.PACKAGES.items() if path in url)
                value = (pending or OLD)[name]
                content = base64.b64encode(f'version = "{value}";\n'.encode()).decode()
                return {"encoding": "base64", "content": content}
            if url.endswith("/releases/latest"):
                return {"draft": False, "prerelease": False,
                        "tag_name": "rust-v" + upstream["codex-app-server"]}
            if url.endswith("/claude-code/latest"):
                return {"version": upstream["claude-code"]}
            if url.endswith("/codex/latest"):
                return {"version": upstream["codex-cli"]}
            raise AssertionError(url)
        return get

    def probe(self, upstream=NEW, pending=None, open_pr=True, installed=OLD):
        get = self.responses(upstream, pending, open_pr)
        latest = probe.release_versions(get)
        pr = probe.open_bundle_pr(get)
        head = probe.pending_versions(pr, get) if pr else None
        return probe.decide(installed, latest, head), pr

    def test_no_update(self):
        should_run, _ = self.probe(upstream=OLD, pending=OLD)
        self.assertFalse(should_run)

    def test_new_release(self):
        should_run, _ = self.probe(open_pr=False)
        self.assertTrue(should_run)

    def test_pending_exact_candidate_is_skipped(self):
        should_run, pr = self.probe(pending=NEW)
        self.assertIsNotNone(pr)
        self.assertFalse(should_run)

    def test_newer_release_supersedes_pending_candidate(self):
        should_run, _ = self.probe(pending=OLD)
        self.assertTrue(should_run)

    def test_closed_branch_is_ignored(self):
        should_run, pr = self.probe(open_pr=False)
        self.assertIsNone(pr)
        self.assertTrue(should_run)

    def test_rollback_rejected_for_installed_and_pending(self):
        for installed, pending in ((NEW, None), (OLD, NEW)):
            with self.subTest(installed=installed, pending=pending):
                with self.assertRaisesRegex(ValueError, "avoid a downgrade"):
                    probe.decide(installed, OLD, pending)

    def test_prerelease_and_malformed_release_rejected(self):
        for value in ("0.160.0-rc.1", "01.2.3", "not-a-version"):
            with self.subTest(value=value):
                with self.assertRaisesRegex(ValueError, "invalid stable release"):
                    probe.release_versions(self.responses(NEW | {"codex-cli": value}))
        def prerelease(url):
            result = self.responses()(url)
            if url.endswith("/releases/latest"):
                result["prerelease"] = True
            return result
        with self.assertRaisesRegex(ValueError, "draft or prerelease"):
            probe.release_versions(prerelease)

    def test_missing_or_malformed_pr_head_fails(self):
        with self.assertRaisesRegex(ValueError, "head SHA"):
            probe.pending_versions({"head": {"sha": "bad"}}, self.responses())
        def malformed(url):
            result = self.responses()(url)
            if "/contents/" in url:
                return {"encoding": "base64", "content": base64.b64encode(b'no pin').decode()}
            return result
        with self.assertRaisesRegex(ValueError, "expected one version pin"):
            probe.pending_versions({"head": {"sha": SHA}}, malformed)

    def test_network_failure_and_timeout(self):
        def fail(_):
            raise urllib.error.URLError("network down")
        with self.assertRaises(urllib.error.URLError):
            probe.release_versions(fail)
        with mock.patch("urllib.request.urlopen", side_effect=urllib.error.URLError("timeout")) as urlopen:
            with self.assertRaises(urllib.error.URLError):
                probe.fetch_json("https://registry.npmjs.org/@openai/codex/latest")
            self.assertEqual(urlopen.call_args.kwargs["timeout"], 10)

    def test_local_pins(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for name, (path, _) in probe.PACKAGES.items():
                destination = root / path
                destination.parent.mkdir(parents=True)
                destination.write_text(f'version = "{OLD[name]}";\n')
            self.assertEqual(probe.local_versions(root), OLD)


if __name__ == "__main__":
    unittest.main()
