"""Black-box deployment planning tests; every mutating/network command is mocked."""

import json
import os
from pathlib import Path
import shutil
import shlex
import subprocess
import sys
import tempfile

HOST_DATA = {
    "schemaVersion": 1,
    "operatorHost": "xyz",
    "homeManagerUser": "alc",
    "homeOutputPrefix": "alc-",
    "knownHosts": ["xyz", "node", "solo", "mac"],
    "homeManagerHosts": ["xyz", "node", "mac"],
    "remoteHosts": ["node", "solo"],
    "deployAllHosts": ["xyz", "node", "solo"],
    "aliases": {"workstation": "xyz", "edge": "node"},
    "sshHosts": {"node": "node.invalid", "solo": "solo.invalid"},
    "systemSshUsers": {"solo": "operator"},
    "systemRemoteSudoHosts": ["solo"],
    "systemActivationModes": {"solo": "boot"},
    "hostColors": {"node": "1;2;3"},
}
MOCK = r'''
import json, os, pathlib, subprocess, sys, time
name = pathlib.Path(sys.argv[0]).name
args = sys.argv[1:]
with open(os.environ["MOCK_LOG"], "a") as out:
    out.write(json.dumps([name, *args]) + "\n")
if name == "hostname":
    print(os.environ["MOCK_HOST"])
elif name == "uname":
    print(os.environ["MOCK_SYSTEM"])
elif name == "sleep":
    time.sleep(0.01)
elif name == "sudo":
    # Never execute the requested privileged command, including absolute paths.
    sys.exit(1 if args == ["-n", "-v"] else 0)
elif name == "ssh":
    if args[-1] == "true":
        if os.environ.get("MOCK_UNREACHABLE", "never.invalid") in args[-2]:
            print("fixture endpoint unavailable", file=sys.stderr)
            sys.exit(255)
    elif args[-1].startswith("df -Pk"):
        print(os.environ.get("MOCK_FREE_KIB", "99999999"))
elif name == "nix":
    if args and args[0] == "build":
        print("/nix/store/fixture-home-activation")
elif name == "nixos-rebuild":
    failure = os.environ.get("MOCK_SWITCH_FAILURE")
    if args and args[0] == "switch" and failure:
        print("Pre-switch check 'switchInhibitors' failed" if failure == "inhibited" else "fixture rebuild failure")
        sys.exit(7)
elif name == "parallel":
    for job in args[args.index(":::") + 1:]:
        result = subprocess.run([os.environ["MOCK_BASH"], "-c", job.split("\t", 2)[2]])
        if result.returncode:
            sys.exit(result.returncode)
'''


def actions(calls):
    return [c for c in calls if c[0] in {"nixos-rebuild", "home-manager", "nix"} or (c[0] == "sudo" and len(c) > 2 and c[1] not in {"-v", "-n"})]


with tempfile.TemporaryDirectory(prefix="deploy-contract-") as directory:
    root = Path(directory)
    commands = root / "bin"
    commands.mkdir()
    deploy = commands / "deploy"
    source = Path(sys.argv[1]).read_text()
    deploy.write_text(source.replace("#!/usr/bin/env bash", "#!" + shutil.which("bash"), 1))
    deploy.chmod(0o755)
    # Keep the command search path closed; new operational commands cannot fall
    # through to an installed host tool. Mocks use an absolute Python shebang.
    for name in ["bash", "cat", "jq", "tr", "sed", "tail", "grep"]:
        target = shutil.which(name)
        assert target, name
        (commands / name).symlink_to(target)
    config = root / "inventory with spaces.json"
    config.write_text(json.dumps(HOST_DATA))
    for name in ["hostname", "uname", "sleep", "sudo", "ssh", "nix", "nixos-rebuild", "home-manager", "parallel"]:
        command = commands / name
        command.write_text("#!" + sys.executable + "\n" + MOCK)
        command.chmod(0o755)

    def run(*args, host="xyz", system="Linux", status=0, **settings):
        log = root / "commands.jsonl"
        log.write_text("")
        env = {
            "PATH": str(commands),
            "HOME": str(root), "MOCK_LOG": str(log), "MOCK_BASH": shutil.which("bash"),
            "MOCK_HOST": host, "MOCK_SYSTEM": system,
            "NIX_SSHOPTS": "-o ConnectTimeout=1", "NO_COLOR": "1",
            **settings,
        }
        result = subprocess.run([str(deploy), "--config", str(config), *args], cwd=root, env=env, capture_output=True, text=True, timeout=15)
        calls = [json.loads(line) for line in log.read_text().splitlines()]
        assert result.returncode == status, (args, result.returncode, result.stdout, result.stderr)
        return calls, result

    calls, _ = run("--help", status=1)
    assert not actions(calls)
    calls, _ = run("unknown", status=1)
    assert not actions(calls)
    calls, _ = run()
    assert ["sudo", "nixos-rebuild", "switch", "--flake", ".#xyz"] in calls
    assert ["home-manager", "switch", "--flake", ".#alc-xyz"] in calls
    calls, _ = run("--no-preflight", "--nixos", "edge")
    assert ["nixos-rebuild", "switch", "--flake", ".#node", "--target-host", "root@node.invalid", "--use-substitutes"] in calls
    assert not any(c[0] == "ssh" for c in calls)
    calls, _ = run("--hm", "node")
    assert ["nix", "build", "--no-link", "--print-out-paths", ".#homeConfigurations.alc-node.activationPackage"] in calls
    assert ["nix", "copy", "--substitute-on-destination", "--to", "ssh://alc@node.invalid", "/nix/store/fixture-home-activation"] in calls
    assert any(c[0] == "ssh" and c[-2] == "alc@node.invalid" and "max-jobs = 1" in c[-1] for c in calls)
    calls, _ = run("--no-preflight", "solo")
    assert ["nixos-rebuild", "boot", "--flake", ".#solo", "--target-host", "operator@solo.invalid", "--use-substitutes", "--ask-sudo-password"] in calls
    assert not any(c[0] in {"home-manager", "nix"} for c in calls)
    calls, _ = run("--hm", "solo", status=1)
    assert not actions(calls)
    calls, _ = run("--nixos", "node", status=1, MOCK_UNREACHABLE="node.invalid")
    assert not actions(calls)
    calls, _ = run("--nixos", "node", status=1, MOCK_FREE_KIB="1")
    assert not actions(calls)
    calls, _ = run("--nixos", "node", status=1, MOCK_FREE_KIB="invalid")
    assert not actions(calls)
    calls, _ = run("--no-preflight", "--nixos", "node", MOCK_UNREACHABLE="node.invalid")
    assert any(c[0] == "nixos-rebuild" for c in calls)
    calls, _ = run("--all", "--nixos", host="mac", system="Darwin", status=1)
    assert not actions(calls)
    calls, _ = run("--all", "--nixos", MOCK_UNREACHABLE="node.invalid")
    assert not any(c[0] == "nixos-rebuild" and ".#node" in c for c in calls)
    assert any(c[0] == "nixos-rebuild" and ".#solo" in c for c in calls)
    calls, _ = run("--all", "--nixos", "--fail-unreachable", status=1, MOCK_UNREACHABLE="node.invalid")
    assert not actions(calls)
    # Existing fleet semantics include NixOS-only hosts in the candidate list;
    # retain the failure while verifying recursive children keep the inventory.
    calls, _ = run("--all", "--hm", "--no-preflight", status=1)
    parallel = next(c for c in calls if c[0] == "parallel")
    recursive_jobs = [job for job in parallel[parallel.index(":::") + 1:] if "deploy --config" in job]
    assert recursive_jobs and all(shlex.split(job.split("\t", 2)[2])[2] == str(config) for job in recursive_jobs)
    calls, _ = run("--all", "--here", "--no-preflight", "--nixos", host="mac", system="Darwin")
    assert any(c[0] == "nixos-rebuild" and ".#node" in c for c in calls)
    assert not any("--build-host" in c for c in calls)
    assert not any(".#xyz" in c for c in calls)
    calls, _ = run("--nixos", host="mac", system="Darwin")
    assert ["sudo", "/run/current-system/sw/bin/darwin-rebuild", "switch", "--flake", ".#mac"] in calls
    calls, _ = run("--no-preflight", "--nixos", "node", MOCK_SWITCH_FAILURE="inhibited")
    assert [c[1] for c in calls if c[0] == "nixos-rebuild"] == ["switch", "boot"]
    calls, _ = run("--no-preflight", "--nixos", "node", status=7, MOCK_SWITCH_FAILURE="other")
    assert [c[1] for c in calls if c[0] == "nixos-rebuild"] == ["switch"]

    def reject_inventory(data, expected):
        candidate = root / "invalid.json"
        candidate.write_text(data if isinstance(data, str) else json.dumps(data))
        log = root / "commands.jsonl"
        log.write_text("")
        env = {
            "PATH": str(commands),
            "HOME": str(root), "MOCK_LOG": str(log), "MOCK_BASH": shutil.which("bash"),
            "MOCK_HOST": "xyz", "MOCK_SYSTEM": "Linux", "NO_COLOR": "1",
        }
        result = subprocess.run([str(deploy), "--config", str(candidate), "--help"], cwd=root, env=env, capture_output=True, text=True)
        assert result.returncode == 1
        assert expected in result.stderr, (data, result.stderr)
        assert not actions([json.loads(line) for line in log.read_text().splitlines()])

    missing = subprocess.run([str(deploy), "--help"], cwd=root, env={"PATH": str(commands)}, capture_output=True, text=True)
    assert missing.returncode == 1 and "no inventory configured" in missing.stderr
    configured = subprocess.run(
        [str(deploy), "--help"], cwd=root,
        env={"PATH": str(commands), "NIX_DEPLOY_CONFIG": str(config), "MOCK_LOG": str(root / "commands.jsonl"), "MOCK_HOST": "xyz", "MOCK_SYSTEM": "Linux"},
        capture_output=True, text=True,
    )
    assert configured.returncode == 1 and "Known hosts: xyz node solo mac" in configured.stdout
    reject_inventory("not-json", "not valid JSON")
    reject_inventory({**HOST_DATA, "schemaVersion": "1"}, "does not satisfy schemaVersion 1")
    reject_inventory({**HOST_DATA, "schemaVersion": 2}, "unsupported inventory schemaVersion")
    reject_inventory({key: value for key, value in HOST_DATA.items() if key != "knownHosts"}, "does not satisfy schemaVersion 1")
    reject_inventory({**HOST_DATA, "sshHosts": {"node": "node.invalid;touch-pwned"}}, "does not satisfy schemaVersion 1")
    reject_inventory({**HOST_DATA, "aliases": {"node": "solo"}}, "does not satisfy schemaVersion 1")
    reject_inventory({**HOST_DATA, "remoteHosts": ["missing"]}, "does not satisfy schemaVersion 1")

print("Deployment contract: 19 mocked CLI cases and 7 invalid inventories passed; no deployment commands executed")
