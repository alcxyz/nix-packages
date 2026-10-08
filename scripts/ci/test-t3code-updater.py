#!/usr/bin/env python3
"""Offline release and pin checks for T3 Code."""
import base64
import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

sys.dont_write_bytecode = True
spec = importlib.util.spec_from_file_location(
    "source", Path(__file__).parents[1] / "update-packages/t3code-source.py"
)
source = importlib.util.module_from_spec(spec)
spec.loader.exec_module(source)
probe_spec = importlib.util.spec_from_file_location(
    "probe", Path(__file__).parents[1] / "update-packages/probe-t3code-updates.py"
)
probe = importlib.util.module_from_spec(probe_spec)
probe_spec.loader.exec_module(probe)
HASH = "sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
GOT_HASH = "sha256-AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="


def pin():
    return {
        "version": "0.0.44-nightly.20260929.2456", "revision": "a" * 40,
        "hash": HASH, "cargoHash": HASH, "pnpmDepsHash": HASH,
    }


def target(value):
    return {"version": value["version"], "revision": value["revision"]}


def newer_target():
    return {"version": "0.0.45-nightly.20260930.1", "revision": "f" * 40}


def encoded(data):
    return {"encoding": "base64", "content": base64.b64encode(json.dumps(data).encode()).decode()}


class T3SourceTests(unittest.TestCase):
    def test_published_nightly_selection_uses_publish_time(self):
        older = {"tag_name": "v0.0.44-nightly.20260929.2455", "published_at": "2026-09-29T01:00:00Z", "draft": False}
        newer = {"tag_name": "v0.0.44-nightly.20260929.2456", "published_at": "2026-09-29T02:00:00Z", "draft": False}
        stable = {"tag_name": "v0.0.44", "published_at": "2026-09-30T01:00:00Z", "draft": False}
        draft = newer | {"draft": True, "tag_name": "v9.0.0-nightly.20260930.1"}
        self.assertEqual(source.select_nightly([stable, newer, draft, older]), newer["tag_name"][1:])
        with self.assertRaises(ValueError):
            source.select_nightly([stable, draft])

    def test_annotated_and_lightweight_release_tags(self):
        commit = "a" * 40
        tag_object = "b" * 40
        self.assertEqual(source.resolve_tag("0.0.44", lambda _owner, _path: {
            "object": {"type": "commit", "sha": commit}
        }), commit)
        calls = []
        def request(owner, path):
            calls.append((owner, path))
            return {"object": {"type": "tag", "sha": tag_object}} if len(calls) == 1 else {
                "object": {"type": "commit", "sha": commit}
            }
        self.assertEqual(source.resolve_tag("0.0.44", request), commit)
        self.assertEqual(calls[-1], ("pingdotgg", "git/tags/" + tag_object))

    def test_discovers_newest_published_nightly_tag_commit(self):
        version = "0.0.44-nightly.20260929.2456"
        def request(owner, path):
            self.assertEqual(owner, "pingdotgg")
            if path.startswith("releases?"):
                return [{"tag_name": "v" + version, "published_at": "2026-09-29T02:00:00Z", "draft": False}]
            if path == "git/ref/tags/v" + version:
                return {"object": {"type": "commit", "sha": "a" * 40}}
            raise AssertionError(path)
        with mock.patch.dict(os.environ):
            os.environ.pop("T3CODE_VERSION", None)
            self.assertEqual(source.discover_target(request), {"version": version, "revision": "a" * 40})

    def test_pin_validation_and_atomic_write(self):
        value = pin()
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "source.json"
            source.write_pin(path, value)
            original = path.read_bytes()
            for changed in (
                {"hash": "sha256-"},
                {"revision": "main"},
                {"version": "latest"},
                {"forks": {}},
            ):
                with self.subTest(changed=changed), self.assertRaises(ValueError):
                    source.write_pin(path, value | changed)
                self.assertEqual(path.read_bytes(), original)

    def test_unchanged_open_pr_skips_rebuild_and_versions_cannot_regress(self):
        installed = pin()
        self.assertFalse(source.decide(installed, target(installed)))
        changed = newer_target()
        self.assertTrue(source.decide(installed, changed))
        pending = installed | changed
        self.assertFalse(source.decide(installed, changed, pending))
        regressed = target(installed) | {"version": "0.0.43-nightly.20260928.1"}
        with self.assertRaisesRegex(ValueError, "upstream T3 version regression"):
            source.decide(installed, regressed)
        with self.assertRaisesRegex(ValueError, "upstream T3 version regression"):
            source.decide(installed, target(installed), pending)

    def test_materialize_combines_target_and_hashes(self):
        hashes = {"hash": GOT_HASH, "cargoHash": HASH, "pnpmDepsHash": HASH}
        result = source.materialize(newer_target(), hashes)
        self.assertEqual(result, newer_target() | hashes)
        with self.assertRaisesRegex(ValueError, "all source and dependency hashes"):
            source.materialize(newer_target(), {"hash": GOT_HASH})

    def test_probe_exports_update_decision(self):
        installed = pin()
        for candidate, expected in ((newer_target(), "true"), (target(installed), "false")):
            with self.subTest(expected=expected), tempfile.TemporaryDirectory() as directory:
                pin_path = Path(directory) / "source.json"
                pin_path.write_text(json.dumps(installed))
                output = Path(directory) / "output"
                with mock.patch.object(probe, "PIN_PATH", str(pin_path)), \
                        mock.patch.object(probe, "open_update_pr", return_value=None), \
                        mock.patch.object(probe.source, "discover_target", return_value=candidate), \
                        mock.patch.dict(os.environ, {"GITHUB_OUTPUT": str(output)}), \
                        mock.patch("sys.stdout"):
                    probe.main()
                lines = output.read_text().splitlines()
                self.assertIn(f"should_run={expected}", lines)
                self.assertIn("existing_update_pr=false", lines)

    def test_probe_reads_immutable_pr_head_and_skips_identical_candidate(self):
        installed = pin()
        candidate = target(installed)
        head = "d" * 40
        pull = {"state": "open", "base": {"ref": "dev"},
                "head": {"ref": "update/t3code", "sha": head}}
        calls = []
        def get(url):
            calls.append(url)
            if "/pulls?" in url:
                return [pull]
            return encoded(installed)
        with mock.patch.dict(os.environ, {
            "FORGEJO_URL": "https://forge.example", "FORGEJO_OWNER": "owner",
            "FORGEJO_REPO": "repo",
        }):
            found = probe.open_update_pr(get)
            pending = probe.pending_pin(found, get)
        self.assertEqual(pending, installed)
        self.assertTrue(calls[-1].endswith("?ref=" + head))
        self.assertFalse(source.decide(installed, candidate, pending))


class UpdaterTests(unittest.TestCase):
    def run_updater(self, *, failure=None, current=False):
        repo = Path(__file__).parents[2]
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for folder in ("scripts/update-packages", "scripts/ci", "pkgs/t3code", "bin"):
                (root / folder).mkdir(parents=True, exist_ok=True)
            for path in (
                "scripts/update-packages/update-t3code.sh",
                "scripts/ci/t3code-nix-home.sh",
                "scripts/ci/ephemeral-nix-home.sh",
            ):
                shutil.copyfile(repo / path, root / path)
            real_helper = root / "scripts/update-packages/real-helper.py"
            shutil.copyfile(repo / "scripts/update-packages/t3code-source.py", real_helper)
            helper = root / "scripts/update-packages/t3code-source.py"
            helper.write_text('''#!/usr/bin/env python3
import os, sys
if sys.argv[1] == "target":
    if os.environ.get("FAILURE") == "discovery":
        print("discovery unavailable", file=sys.stderr)
        sys.exit(1)
    print(open("target.json").read())
else:
    os.execv(sys.executable, [sys.executable, "scripts/update-packages/real-helper.py", *sys.argv[1:]])
''')
            original_pin = pin()
            candidate = target(original_pin) if current else newer_target()
            (root / "target.json").write_text(json.dumps(candidate))
            pin_path = root / "pkgs/t3code/source.json"
            source.write_pin(pin_path, original_pin)
            initial = pin_path.read_bytes()
            nix = root / "bin/nix"
            nix.write_text('''#!/usr/bin/env python3
import json, os, pathlib, sys
args = sys.argv[1:]
with pathlib.Path("nix-calls").open("a") as calls:
    calls.write(" ".join(args) + "\\n")
if args[:2] == ["store", "prefetch-file"]:
    print(json.dumps({"hash": "''' + GOT_HASH + '''"}))
else:
    if os.environ.get("FAILURE") == "hash" and "resourceMonitor" in args[2]:
        print("hash unavailable", file=sys.stderr)
        sys.exit(1)
    print("error: hash mismatch\\n  got: ''' + GOT_HASH + '''", file=sys.stderr)
    sys.exit(1)
''')
            nix.chmod(0o755)
            verify = root / "scripts/ci/verify-t3code-providers.sh"
            verify.write_text('''#!/usr/bin/env bash
touch verified
[[ ${FAILURE:-} != verify ]]
''')
            verify.chmod(0o755)
            output = root / "output"
            result = subprocess.run(
                ["bash", "scripts/update-packages/update-t3code.sh"], cwd=root,
                env=os.environ | {"PATH": str(root / "bin") + ":" + os.environ["PATH"],
                                  "GITHUB_OUTPUT": str(output),
                                  "T3CODE_CI_CLEAN_HOME": "false", "NIX_CI_EPHEMERAL_CONTAINER": "0",
                                  "FAILURE": failure or ""},
                capture_output=True, text=True,
            )
            if current:
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn("updated=false", output.read_text())
                self.assertFalse((root / "nix-calls").exists())
                self.assertEqual(pin_path.read_bytes(), initial)
                return
            if failure:
                self.assertNotEqual(result.returncode, 0, result.stderr)
                self.assertEqual(pin_path.read_bytes(), initial)
                self.assertFalse(output.exists())
                if failure == "discovery":
                    self.assertIn("discovery unavailable", result.stderr)
                    self.assertFalse((root / "nix-calls").exists())
                    return
                calls = (root / "nix-calls").read_text()
                if failure == "hash":
                    self.assertIn(".#t3code.resourceMonitor", calls)
                    self.assertNotIn("pnpmDeps", calls)
                    self.assertFalse((root / "verified").exists())
                elif failure == "verify":
                    self.assertTrue((root / "verified").exists())
                return
            self.assertEqual(result.returncode, 0, result.stderr)
            installed = source.validate_pin(json.loads(pin_path.read_text()))
            self.assertEqual(installed, candidate | {
                "hash": GOT_HASH, "cargoHash": GOT_HASH, "pnpmDepsHash": GOT_HASH,
            })
            self.assertTrue((root / "verified").exists())
            calls = (root / "nix-calls").read_text()
            self.assertIn("pingdotgg/t3code/archive/" + candidate["revision"], calls)
            self.assertNotIn("alcxyz", calls)
            self.assertLess(calls.index(".#t3code.resourceMonitor"), calls.index(".#t3code.pnpmDeps"))
            self.assertEqual(output.read_text().splitlines(),
                             ["updated=true", "version=" + candidate["version"]])

    def test_success_pins_published_upstream_nightly(self):
        self.run_updater()

    def test_unchanged_candidate_skips_nix(self):
        self.run_updater(current=True)

    def test_missing_dependency_hash_restores_previous_pin(self):
        self.run_updater(failure="hash")

    def test_runtime_validation_failure_restores_previous_pin(self):
        self.run_updater(failure="verify")

    def test_discovery_failure_leaves_pin_untouched(self):
        self.run_updater(failure="discovery")


if __name__ == "__main__":
    unittest.main()
