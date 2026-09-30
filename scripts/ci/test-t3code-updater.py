#!/usr/bin/env python3
"""Offline release, promotion, and pin checks for T3 Code."""
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


def promotion(channel, version, revision, baseline, feature="e" * 40):
    return {"channel": channel, "releaseTag": "v" + version, "version": version,
            "revision": revision, "upstreamRevision": baseline, "featureRevision": feature}


def pin():
    return {
        "version": "0.0.44-nightly.20260929.2456", "revision": "a" * 40,
        "hash": HASH, "cargoHash": HASH, "pnpmDepsHash": HASH,
        "forks": {
            channel: {key: value for key, value in promotion(channel, version, revision, "a" * 40).items()
                      if key != "channel"} | {
                          "hash": HASH, "cargoHash": HASH, "pnpmDepsHash": HASH, "patchRevision": 3
                      }
            for channel, version, revision in (
                ("nightly", "0.0.44-nightly.20260929.2456", "b" * 40),
                ("stable", "0.0.44", "c" * 40),
            )
        },
    }


def legacy_pin():
    return {
        "version": "0.0.43-nightly.20260929.2450", "revision": "f" * 40,
        "hash": HASH, "cargoHash": HASH, "pnpmDepsHash": HASH,
        "forkVersion": "0.0.42", "forkRevision": "d" * 40,
        "forkUpstreamRevision": "e" * 40, "forkHash": HASH,
        "forkCargoHash": HASH, "forkPnpmDepsHash": HASH, "forkPatchRevision": 23,
    }


def target(value):
    return {"version": value["version"], "revision": value["revision"],
            "forks": {channel: source.validate_promotion(
                {"channel": channel, **{key: fork[key] for key in source.PROMOTION_FIELDS - {"channel"}}},
                channel,
            ) | {"revision": fork["revision"]}
            for channel, fork in value["forks"].items()}}


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

    def test_metadata_requires_exact_channel_tag_and_full_revisions(self):
        valid = promotion("nightly", "0.0.44-nightly.20260929.2456", "b" * 40, "a" * 40)
        valid.pop("revision")
        self.assertEqual(source.validate_promotion(valid, "nightly"), valid)
        for changed in (
            {"channel": "stable"}, {"releaseTag": "v0.0.43"},
            {"version": "0.0.44"}, {"featureRevision": "main"}, {"extra": True},
        ):
            with self.subTest(changed=changed), self.assertRaises(ValueError):
                source.validate_promotion(valid | changed, "nightly")
        with self.assertRaises(ValueError):
            source.validate_promotion(valid | {"channel": "stable"}, "stable")

    def test_resolves_promoted_head_against_published_exact_tag_commit(self):
        version = "0.0.44"
        baseline = "a" * 40
        head = "b" * 40
        feature = "c" * 40
        metadata = promotion("stable", version, head, baseline, feature)
        metadata.pop("revision")
        calls = []

        def request(owner, path):
            calls.append((owner, path))
            if path == "git/ref/heads/fork/stable":
                return {"object": {"type": "commit", "sha": head}}
            if path.startswith("contents/"):
                self.assertTrue(path.endswith("?ref=" + head))
                return encoded(metadata)
            if path == "commits/" + feature:
                return {"sha": feature}
            if path == "releases/tags/v" + version:
                return {"tag_name": "v" + version, "draft": False, "prerelease": False}
            if path == "git/ref/tags/v" + version:
                return {"object": {"type": "commit", "sha": baseline}}
            if path.startswith("compare/"):
                return {"status": "ahead", "merge_base_commit": {"sha": baseline}}
            raise AssertionError((owner, path))

        self.assertEqual(source.resolve_fork_promotion("stable", request), metadata | {"revision": head})
        self.assertIn(("alcxyz", f"compare/{baseline}...{head}"), calls)
        self.assertNotIn(("alcxyz", f"compare/{feature}...{head}"), calls)

        def wrong_baseline(owner, path):
            value = request(owner, path)
            if path == "git/ref/tags/v" + version:
                return {"object": {"type": "commit", "sha": "d" * 40}}
            return value
        with self.assertRaisesRegex(ValueError, "exact upstream release tag"):
            source.resolve_fork_promotion("stable", wrong_baseline)

        def missing_feature(owner, path):
            value = request(owner, path)
            return {"sha": "d" * 40} if path == "commits/" + feature else value
        with self.assertRaisesRegex(ValueError, "exact fork commit"):
            source.resolve_fork_promotion("stable", missing_feature)

        def divergent(owner, path):
            value = request(owner, path)
            if path.startswith("compare/"):
                return {"status": "diverged", "merge_base_commit": {"sha": "d" * 40}}
            return value
        with self.assertRaisesRegex(ValueError, "does not descend"):
            source.resolve_fork_promotion("stable", divergent)

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

    def test_nightly_promotion_requires_published_matching_release(self):
        version = "0.0.44-nightly.20260929.2456"
        baseline = "a" * 40
        head = "b" * 40
        metadata = promotion("nightly", version, head, baseline)
        metadata.pop("revision")

        def request(owner, path):
            if path == "git/ref/heads/fork/nightly":
                return {"object": {"type": "commit", "sha": head}}
            if path.startswith("contents/"):
                return encoded(metadata)
            if path == "commits/" + metadata["featureRevision"]:
                return {"sha": metadata["featureRevision"]}
            if path == "releases/tags/v" + version:
                return {"tag_name": "v" + version, "draft": False, "prerelease": True}
            if path == "git/ref/tags/v" + version:
                return {"object": {"type": "commit", "sha": baseline}}
            if path.startswith("compare/"):
                return {"status": "ahead", "merge_base_commit": {"sha": baseline}}
            raise AssertionError((owner, path))
        self.assertEqual(source.resolve_fork_promotion("nightly", request)["revision"], head)

        def draft(owner, path):
            value = request(owner, path)
            return value | {"draft": True} if path == "releases/tags/v" + version else value
        with self.assertRaisesRegex(ValueError, "published upstream release"):
            source.resolve_fork_promotion("nightly", draft)

    def test_decodes_wrapped_metadata_content(self):
        metadata = promotion("stable", "0.0.44", "b" * 40, "a" * 40)
        response = encoded(metadata)
        content = response["content"]
        response["content"] = content[:20] + "\n" + content[20:]
        self.assertEqual(source.decode_content(response), metadata)

    def test_pin_validation_and_atomic_write(self):
        value = pin()
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "source.json"
            source.write_pin(path, value)
            original = path.read_bytes()
            for changed in (
                {"hash": "sha256-"},
                {"forks": value["forks"] | {"stable": value["forks"]["stable"] | {"releaseTag": "v0.0.43"}}},
                {"forks": value["forks"] | {"stable": value["forks"]["stable"] | {"patchRevision": True}}},
            ):
                with self.assertRaises(ValueError):
                    source.write_pin(path, value | changed)
                self.assertEqual(path.read_bytes(), original)

    def test_legacy_pin_requires_bootstrap_and_is_not_called_nightly(self):
        value = legacy_pin()
        self.assertNotIn("forks", source.validate_pin(value))
        current_target = target(pin())
        self.assertTrue(source.decide(value, current_target))
        hashes = {channel: {"hash": HASH, "cargoHash": HASH, "pnpmDepsHash": HASH}
                  for channel in ("upstream", "nightly", "stable")}
        result = source.materialize(value, current_target, hashes)
        self.assertEqual(result["forks"]["nightly"]["patchRevision"], value["forkPatchRevision"] + 1)
        self.assertEqual(result["forks"]["stable"]["patchRevision"], 1)
        self.assertEqual(result["forks"]["stable"]["version"], "0.0.44")

    def test_unchanged_open_pr_skips_rebuild_and_versions_cannot_regress(self):
        installed = pin()
        candidate = target(installed)
        self.assertFalse(source.decide(installed, candidate))
        self.assertFalse(source.decide(installed, candidate, installed))
        changed = json.loads(json.dumps(candidate))
        changed["forks"]["nightly"]["revision"] = "d" * 40
        self.assertTrue(source.decide(installed, changed))
        pending = json.loads(json.dumps(installed))
        pending["forks"]["nightly"]["revision"] = "d" * 40
        self.assertFalse(source.decide(installed, changed, pending))
        changed["forks"]["stable"]["version"] = "0.0.43"
        changed["forks"]["stable"]["releaseTag"] = "v0.0.43"
        with self.assertRaisesRegex(ValueError, "stable fork version regression"):
            source.decide(installed, changed)
        changed = target(installed)
        changed["forks"]["nightly"]["version"] = "0.0.44-nightly.20260929.2455"
        changed["forks"]["nightly"]["releaseTag"] = "v0.0.44-nightly.20260929.2455"
        with self.assertRaisesRegex(ValueError, "nightly fork version regression"):
            source.decide(installed, changed)
        changed = target(installed)
        changed["version"] = "0.0.43-nightly.20260928.1"
        with self.assertRaisesRegex(ValueError, "upstream T3 version regression"):
            source.decide(installed, changed)

    def test_each_channel_revision_increases_independently(self):
        installed = pin()
        candidate = target(installed)
        hashes = {channel: {"hash": HASH, "cargoHash": HASH, "pnpmDepsHash": HASH}
                  for channel in ("upstream", "nightly", "stable")}
        unchanged = source.materialize(installed, candidate, hashes)
        self.assertEqual(unchanged["forks"]["nightly"]["patchRevision"], 3)
        self.assertEqual(unchanged["forks"]["stable"]["patchRevision"], 3)
        candidate["forks"]["nightly"]["revision"] = "d" * 40
        updated = source.materialize(installed, candidate, hashes)
        self.assertEqual(updated["forks"]["nightly"]["patchRevision"], 4)
        self.assertEqual(updated["forks"]["stable"]["patchRevision"], 3)

    def test_lagging_stable_promotion_can_remain_pinned(self):
        installed = pin()
        candidate = target(installed)
        candidate["version"] = "0.0.45-nightly.20260930.1"
        candidate["revision"] = "f" * 40
        candidate["forks"]["nightly"]["version"] = candidate["version"]
        candidate["forks"]["nightly"]["releaseTag"] = "v" + candidate["version"]
        candidate["forks"]["nightly"]["upstreamRevision"] = candidate["revision"]
        candidate["forks"]["nightly"]["revision"] = "d" * 40
        self.assertTrue(source.decide(installed, candidate))
        hashes = {channel: {"hash": HASH, "cargoHash": HASH, "pnpmDepsHash": HASH}
                  for channel in ("upstream", "nightly", "stable")}
        result = source.materialize(installed, candidate, hashes)
        self.assertEqual(result["forks"]["stable"], installed["forks"]["stable"])
        self.assertEqual(result["forks"]["nightly"]["patchRevision"], 4)

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
            original_pin = source.validate_pin(legacy_pin())
            candidate = target(pin())
            (root / "target.json").write_text(json.dumps(candidate))
            pin_path = root / "pkgs/t3code/source.json"
            if current:
                hashes = {channel: {"hash": GOT_HASH, "cargoHash": GOT_HASH, "pnpmDepsHash": GOT_HASH}
                          for channel in ("upstream", "nightly", "stable")}
                source.write_pin(pin_path, source.materialize(original_pin, candidate, hashes))
            else:
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
elif args[:2] == ["build", "-L"] and args[2].endswith(".src"):
    if os.environ.get("FAILURE") == "preflight-" + args[2].split("fork-")[-1].split(".")[0]:
        print("quota patch failed", file=sys.stderr)
        sys.exit(23)
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
            report = root / "report.json"
            if failure == "discovery":
                report.write_text('{"status": "stale"}\n')
            result = subprocess.run(
                ["bash", "scripts/update-packages/update-t3code.sh"], cwd=root,
                env=os.environ | {"PATH": str(root / "bin") + ":" + os.environ["PATH"],
                                  "GITHUB_OUTPUT": str(output), "T3CODE_PREFLIGHT_REPORT": str(report),
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
                    self.assertFalse(report.exists())
                    self.assertFalse((root / "nix-calls").exists())
                    return
                calls = (root / "nix-calls").read_text()
                if failure.startswith("preflight-"):
                    channel = failure.removeprefix("preflight-")
                    self.assertEqual(json.loads(report.read_text())["channels"][channel]["status"], "failed")
                    self.assertNotIn("resourceMonitor", calls)
                    self.assertNotIn("pnpmDeps", calls)
                    self.assertFalse((root / "verified").exists())
                elif failure == "hash":
                    self.assertIn("resourceMonitor", calls)
                    self.assertNotIn("pnpmDeps", calls)
                    self.assertFalse((root / "verified").exists())
                elif failure == "verify":
                    self.assertTrue((root / "verified").exists())
                return
            self.assertEqual(result.returncode, 0, result.stderr)
            installed = source.validate_pin(json.loads(pin_path.read_text()))
            self.assertEqual(installed["forks"]["nightly"]["hash"], GOT_HASH)
            self.assertEqual(installed["forks"]["stable"]["pnpmDepsHash"], GOT_HASH)
            self.assertEqual(installed["forks"]["nightly"]["patchRevision"], original_pin["forkPatchRevision"] + 1)
            self.assertTrue((root / "verified").exists())
            calls = (root / "nix-calls").read_text()
            for channel in ("nightly", "stable"):
                self.assertLess(calls.index(f".#t3code-fork-{channel}.src"),
                                calls.index(f".#t3code-fork-{channel}.resourceMonitor"))
            self.assertIn("updated=true", output.read_text())

    def test_success_bootstraps_complete_channel_pins(self):
        self.run_updater()

    def test_unchanged_candidate_skips_nix(self):
        self.run_updater(current=True)

    def test_each_patch_failure_restores_previous_pin(self):
        for channel in ("nightly", "stable"):
            with self.subTest(channel=channel):
                self.run_updater(failure="preflight-" + channel)

    def test_missing_dependency_hash_restores_previous_pin(self):
        self.run_updater(failure="hash")

    def test_runtime_validation_failure_restores_previous_pin(self):
        self.run_updater(failure="verify")

    def test_discovery_failure_clears_stale_preflight_report(self):
        self.run_updater(failure="discovery")


if __name__ == "__main__":
    unittest.main()
