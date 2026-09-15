#!/usr/bin/env python3
"""Offline regression tests for release selection and immutable source pins."""
import importlib.util
import json
import os
import shutil
import subprocess
import sys
from pathlib import Path
import tempfile
import unittest

sys.dont_write_bytecode = True

spec = importlib.util.spec_from_file_location("source", Path(__file__).parents[1] / "update-packages/t3code-source.py")
source = importlib.util.module_from_spec(spec)
spec.loader.exec_module(source)


def release(tag, date="2026-09-09T01:00:00Z", **kwargs):
    return dict(tag_name=tag, published_at=date, draft=False) | kwargs


class SourceTests(unittest.TestCase):
    def test_latest_published_not_list_order(self):
        older = release("v0.0.41-nightly.20260909.1000")
        newer = release("v0.0.41-nightly.20260909.1439", "2026-09-09T15:00:00Z")
        for entries in ([newer, older], [older, newer]):
            self.assertEqual(source.select_nightly(entries), newer["tag_name"][1:])

    def test_excludes_draft_stable_and_malformed(self):
        good = release("v0.0.41-nightly.20260909.1439")
        entries = [good, release("v9.0.0"), release("v9.0.0-nightly.20260910.1", draft=True)]
        for tag in ("v1.0.0-nightly.20260231.1", "v1.0.0-nightly.20260909.bad", "v1.0.0-nightly.20260909.1-extra", "1.0.0-nightly.20260909.1"):
            entries.append(release(tag, "2027-01-01T00:00:00Z"))
        entries.append(release("v9.0.0-nightly.20260909.1", published_at=None))
        self.assertEqual(source.select_nightly(entries), good["tag_name"][1:])

    def test_no_nightly_fails(self):
        with self.assertRaises(ValueError):
            source.select_nightly([release("v1.0.0")])

    def test_explicit_versions(self):
        for version in ("0.0.40", "0.0.41-nightly.20260909.1439"):
            self.assertEqual(source.validate_version(version), version)
        for version in ("v0.0.40", "main", "0.0.40;true", "0.0.40-rc.1", "01.0.0", "0.0.40\n"):
            with self.assertRaises(ValueError):
                source.validate_version(version)

    def test_annotated_and_lightweight_tags(self):
        commit = "a" * 40
        self.assertEqual(source.resolve_tag("0.0.40", lambda path: {"object": {"type": "commit", "sha": commit}}), commit)
        calls = []
        def request(path):
            calls.append(path)
            return {"object": {"type": "tag", "sha": "b" * 40}} if len(calls) == 1 else {"object": {"type": "commit", "sha": commit}}
        self.assertEqual(source.resolve_tag("0.0.40", request), commit)
        self.assertEqual(calls[1], "git/tags/" + "b" * 40)

    def test_pin_validation_before_atomic_replace(self):
        pin = dict(version="0.0.40", revision="a" * 40, **dict.fromkeys(("hash", "cargoHash", "pnpmDepsHash"), "sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="))
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "source.json"
            source.write_pin(path, pin)
            original = path.read_bytes()
            for update in ({"revision": "main"}, {"hash": "sha256-wrong"}, {"version": "main"}, {"unexpected": "value"}, {"cargoHash": 1}):
                with self.assertRaises(ValueError):
                    source.write_pin(path, pin | update)
                self.assertEqual(path.read_bytes(), original)
            self.assertEqual(json.loads(original), pin)


class UpdaterTests(unittest.TestCase):
    def run_updater(self, fail_validation=False, fail_preflight=False, fail_discovery=False):
        root = Path(__file__).parents[2]
        with tempfile.TemporaryDirectory() as directory:
            work = Path(directory)
            for name in ("scripts/update-packages", "scripts/ci", "pkgs/t3code", "bin"):
                (work / name).mkdir(parents=True)
            for name in ("scripts/update-packages/update-t3code.sh",
                         "scripts/ci/t3code-nix-home.sh",
                         "scripts/ci/ephemeral-nix-home.sh"):
                shutil.copyfile(root / name, work / name)
            # Use the production pin validation/writer, replacing only network discovery.
            helper = (root / "scripts/update-packages/t3code-source.py").read_text()
            helper = helper.replace('version = os.environ.get("T3CODE_VERSION")', 'version = "0.0.41-nightly.20260909.1439"')
            helper = helper.replace('print(resolve_tag(version))', 'print("b" * 40)')
            if fail_discovery:
                helper = helper.replace('version = "0.0.41-nightly.20260909.1439"',
                                        'raise ValueError("discovery unavailable")')
            (work / "scripts/update-packages/t3code-source.py").write_text(helper)
            pin = dict(version="0.0.40", revision="a" * 40, **dict.fromkeys(("hash", "cargoHash", "pnpmDepsHash"), "sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="))
            pin_path = work / "pkgs/t3code/source.json"
            source.write_pin(pin_path, pin)
            original = pin_path.read_bytes()
            nix = work / "bin/nix"
            nix.write_text("""#!/usr/bin/env python3
import json, os, pathlib, sys
value = 'sha256-AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=' 
with pathlib.Path(os.environ['NIX_CALLS']).open('a') as calls:
    calls.write(' '.join(sys.argv[1:]) + '\\n')
if sys.argv[1:3] == ['store', 'prefetch-file']:
    print(json.dumps({'hash': value}))
elif '.#t3code-fork.src' in sys.argv:
    if os.environ.get('FAIL_PREFLIGHT') == 'true':
        print('error: fork patch failed to apply', file=sys.stderr)
        sys.exit(23)
else:
    print('error: hash mismatch\\n  got: ' + value, file=sys.stderr)
    sys.exit(1)
""")
            nix.chmod(0o755)
            verify = work / "scripts/ci/verify-t3code-providers.sh"
            verify.write_text("#!/usr/bin/env bash\n[[ $T3CODE_VERIFY_ALWAYS == true ]] || exit 2\ntouch verified\nexit " + ("1" if fail_validation else "0") + "\n")
            verify.chmod(0o755)
            output = work / "output"
            report = work / "preflight.json"
            report.write_text('{"status": "passed"}\n')
            nix_calls = work / "nix-calls"
            result = subprocess.run(["bash", "scripts/update-packages/update-t3code.sh"], cwd=work,
                                    env=os.environ | {"PATH": str(work / "bin") + ":" + os.environ["PATH"], "GITHUB_OUTPUT": str(output), "T3CODE_CI_CLEAN_HOME": "false", "NIX_CI_EPHEMERAL_CONTAINER": "0", "T3CODE_PREFLIGHT_REPORT": str(report), "NIX_CALLS": str(nix_calls), "FAIL_PREFLIGHT": str(fail_preflight).lower()},
                                    capture_output=True, text=True)
            if fail_discovery:
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(report.exists())
                self.assertFalse(nix_calls.exists())
                self.assertFalse(output.exists())
                self.assertEqual(pin_path.read_bytes(), original)
                return
            preflight = json.loads(report.read_text())
            self.assertEqual(preflight["scope"], "patch-application-only")
            self.assertFalse(preflight["fullBuildValidated"])
            calls = nix_calls.read_text()
            if fail_preflight:
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(preflight["status"], "failed")
                self.assertEqual(preflight["exitCode"], 23)
                self.assertEqual(pin_path.read_bytes(), original)
                self.assertFalse(output.exists())
                self.assertFalse((work / "verified").exists())
                self.assertNotIn(".#t3code.resourceMonitor", calls)
                self.assertNotIn(".#t3code.pnpmDeps", calls)
            elif fail_validation:
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(preflight["status"], "passed")
                self.assertEqual(pin_path.read_bytes(), original)
                self.assertFalse(output.exists())
                self.assertTrue((work / "verified").exists(), result.stderr)
            else:
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(preflight["status"], "passed")
                self.assertTrue((work / "verified").exists(), result.stderr)
                self.assertLess(calls.index(".#t3code-fork.src"),
                                calls.index(".#t3code.resourceMonitor"))
                self.assertEqual(source.validate_pin(json.loads(pin_path.read_text()))["revision"], "b" * 40)
                self.assertIn("updated=true", output.read_text())

    def test_cleanup_is_disabled_locally_and_guarded_in_ci(self):
        helper = Path(__file__).parent / "t3code-nix-home.sh"
        for enabled, expected in (("false", 0), ("true", 1)):
            result = subprocess.run(["bash", "-c", 'source "$1"; rm() { exit 99; }; clean_homeless_shelter', "test", str(helper)],
                                    env=os.environ | {"T3CODE_CI_CLEAN_HOME": enabled,
                                                      "NIX_CI_EPHEMERAL_CONTAINER": "0"},
                                    capture_output=True, text=True)
            self.assertEqual(result.returncode, expected, result.stderr)

    def test_success_publishes_complete_pin(self):
        self.run_updater()

    def test_validation_failure_restores_pin(self):
        self.run_updater(fail_validation=True)

    def test_patch_conflict_stops_before_dependency_hash_builds(self):
        self.run_updater(fail_preflight=True)

    def test_discovery_failure_removes_previous_success_report(self):
        self.run_updater(fail_discovery=True)


if __name__ == "__main__":
    unittest.main()
