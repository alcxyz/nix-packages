"""Black-box deployment planning tests; every mutating/network command is mocked."""

import json
import os
from pathlib import Path
import shutil
import shlex
import subprocess
import sys
import tempfile
import time

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
# A quorum group whose members must switch one at a time, interleaved with
# ordinary fleet hosts so ordering is not an accident of deployAllHosts.
SERIAL_DATA = {
    **HOST_DATA,
    "knownHosts": [*HOST_DATA["knownHosts"], "k1", "k2", "k3"],
    "deployAllHosts": ["xyz", "k3", "node", "k1", "solo", "k2"],
    "sshHosts": {**HOST_DATA["sshHosts"], "k1": "k1.invalid", "k2": "k2.invalid", "k3": "k3.invalid"},
    "serialSystemHosts": ["k1", "k2", "k3"],
    "serialReadyCommand": "ready-check {host} --node={host}",
    "serialReadyTimeoutSeconds": 25,
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
elif name == "home-manager":
    if os.environ.get("MOCK_HM_FAILURE"):
        print("fixture home-manager failure", file=sys.stderr)
        sys.exit(4)
elif name == "ready-check":
    logged = [json.loads(line) for line in open(os.environ["MOCK_LOG"])]
    switch_at = next((i for i, c in enumerate(logged) if c[0] == "nixos-rebuild" and ".#" + args[0] in c), None)
    switched = switch_at is not None
    gate_attempt = switched and sum(c == ["ready-check", *args] for c in logged[switch_at:])
    if args[0] in os.environ.get("MOCK_READY_HANG", "").split(","):
        time.sleep(30)
    if gate_attempt and gate_attempt > 1 and args[0] in os.environ.get("MOCK_READY_HANG_RETRY", "").split(","):
        time.sleep(30)
    late = dict(item.split(":") for item in os.environ.get("MOCK_READY_FAIL_AFTER", "").split(",") if item)
    late_failure = args[0] in late and any(c[0] == "nixos-rebuild" and ".#" + late[args[0]] in c for c in logged)
    if late_failure or args[0] in os.environ.get("MOCK_READY_FAIL", "").split(",") or (
        switched and args[0] in os.environ.get("MOCK_READY_FAIL_AFTER_SWITCH", "").split(",")
    ):
        print("fixture node not ready", file=sys.stderr)
        sys.exit(1)
elif name == "nixos-rebuild":
    if os.environ.get("MOCK_FAIL_FLAKE") in args:
        print("fixture rebuild failure")
        sys.exit(7)
    failure = os.environ.get("MOCK_SWITCH_FAILURE")
    if args and args[0] == "switch" and failure:
        print("Pre-switch check 'switchInhibitors' failed" if failure == "inhibited" else "fixture rebuild failure")
        sys.exit(7)
elif name == "parallel":
    # Like GNU parallel: run every job, log each exit value, and exit with the
    # number of failed jobs.
    joblog = args[args.index("--joblog") + 1] if "--joblog" in args else None
    rows = ["Seq\tHost\tStarttime\tJobRuntime\tSend\tReceive\tExitval\tSignal\tCommand"]
    failed = 0
    for seq, job in enumerate(args[args.index(":::") + 1:], start=1):
        command = job.split("\t", 2)[2]
        result = subprocess.run([os.environ["MOCK_BASH"], "-c", command])
        failed += result.returncode != 0
        rows.append(f"{seq}\t:\t0\t0\t0\t0\t{result.returncode}\t0\t{command}")
    if joblog:
        pathlib.Path(joblog).write_text("\n".join(rows) + "\n")
    sys.exit(min(failed, 101))
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
    for name in ["bash", "cat", "jq", "tr", "sed", "tail", "grep", "mktemp", "rm", "timeout"]:
        target = shutil.which(name)
        assert target, name
        (commands / name).symlink_to(target)
    config = root / "inventory with spaces.json"
    config.write_text(json.dumps(HOST_DATA))
    for name in ["hostname", "uname", "sleep", "sudo", "ssh", "nix", "nixos-rebuild", "home-manager", "parallel", "ready-check"]:
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
        # CI's pressure guard may freeze the runner longer than the old 15s
        # limit. This is only a hang bound; the assertions below check behavior.
        result = subprocess.run([str(deploy), "--config", str(config), *args], cwd=root, env=env, capture_output=True, text=True, timeout=120)
        calls = [json.loads(line) for line in log.read_text().splitlines()]
        assert result.returncode == status, (args, result.returncode, result.stdout, result.stderr)
        return calls, result

    calls, _ = run("--help", status=1)
    assert not actions(calls)
    calls, _ = run("unknown", status=1)
    assert not actions(calls)
    calls, result = run()
    assert ["sudo", "nixos-rebuild", "switch", "--flake", ".#xyz"] in calls
    assert ["home-manager", "switch", "--flake", ".#alc-xyz"] in calls
    assert "Home Manager NOT updated" not in result.stderr
    # A Home Manager failure after a successful system switch is called out,
    # with the command that brings the host back in step.
    calls, result = run(status=4, MOCK_HM_FAILURE="1")
    assert ["sudo", "nixos-rebuild", "switch", "--flake", ".#xyz"] in calls
    assert "[xyz] system updated, Home Manager NOT updated" in result.stderr
    assert "run: deploy --hm xyz" in result.stderr
    # A Home Manager-only deployment leaves the system alone; no warning.
    calls, result = run("--hm", status=4, MOCK_HM_FAILURE="1")
    assert "Home Manager NOT updated" not in result.stderr
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
    assert ["sudo", "-v"] in calls
    calls, _ = run("--all", "--nixos", "--fail-unreachable", status=1, MOCK_UNREACHABLE="node.invalid")
    assert not actions(calls)
    calls, _ = run("--all", MOCK_UNREACHABLE="root@node.invalid")
    assert not any(c[0] == "nixos-rebuild" and ".#node" in c for c in calls)
    assert ["nix", "build", "--no-link", "--print-out-paths", ".#homeConfigurations.alc-node.activationPackage"] in calls
    assert not any("alc-solo.activationPackage" in arg for c in calls for arg in c)
    calls, _ = run("--all", "--fail-unreachable", status=1, MOCK_UNREACHABLE="root@node.invalid")
    assert not actions(calls)
    calls, _ = run("--all", "--fail-unreachable", status=1, MOCK_UNREACHABLE="alc@node.invalid")
    assert any(c[0] == "nixos-rebuild" and ".#node" in c for c in calls)
    assert not any(c[0] == "nix" and "alc-node.activationPackage" in c[-1] for c in calls)
    # A failed system rebuild must not skip Home Manager for the fleet, and the
    # run still fails with a per-host summary.
    calls, result = run("--all", "--no-preflight", status=1, MOCK_SWITCH_FAILURE="other")
    assert any(c[0] == "nixos-rebuild" and c[1] == "switch" and ".#node" in c for c in calls)
    assert ["home-manager", "switch", "--flake", ".#alc-xyz"] in calls
    assert ["nix", "build", "--no-link", "--print-out-paths", ".#homeConfigurations.alc-node.activationPackage"] in calls
    summary = result.stdout.split("summary", 1)[1].splitlines()
    assert ["node", "failed", "ok"] in [line.split() for line in summary]
    assert ["xyz", "ok", "ok"] in [line.split() for line in summary]
    assert ["solo", "ok", "-"] in [line.split() for line in summary]
    assert "not selected by --all: mac" in result.stdout
    assert "one or more fleet jobs failed" in result.stderr
    assert "Home Manager NOT updated" not in result.stderr
    # The fleet summary is followed by an explicit warning for hosts whose
    # system switched while Home Manager failed.
    calls, result = run("--all", "--no-preflight", status=1, MOCK_HM_FAILURE="1")
    assert ["xyz", "ok", "failed"] in [line.split() for line in result.stdout.split("summary", 1)[1].splitlines()]
    assert "[xyz] system updated, Home Manager NOT updated" in result.stderr
    assert "[node] system updated" not in result.stderr
    # Preflight-skipped hosts are reported as skipped and do not fail the run.
    calls, result = run("--all", "--nixos", MOCK_UNREACHABLE="node.invalid")
    assert ["node", "skipped", "-"] in [line.split() for line in result.stdout.split("summary", 1)[1].splitlines()]
    calls, _ = run("--all", "--hm", "--no-preflight")
    parallel = next(c for c in calls if c[0] == "parallel")
    recursive_jobs = [job for job in parallel[parallel.index(":::") + 1:] if "deploy --config" in job]
    assert recursive_jobs and all(shlex.split(job.split("\t", 2)[2])[2] == str(config) for job in recursive_jobs)
    assert all(" solo" not in job for job in recursive_jobs)
    assert ["home-manager", "switch", "--flake", ".#alc-xyz"] in calls
    calls, _ = run("--all", "--here", "--no-preflight", "--nixos", host="mac", system="Darwin")
    assert any(c[0] == "nixos-rebuild" and ".#node" in c for c in calls)
    assert not any("--build-host" in c for c in calls)
    assert not any(".#xyz" in c for c in calls)
    assert not any(c[0] == "sudo" for c in calls)
    config.write_text(json.dumps({**HOST_DATA, "deployAllHosts": ["solo"]}))
    calls, result = run("--all", "--hm")
    assert not actions(calls)
    assert not any(c[0] in {"ssh", "sudo", "parallel"} for c in calls)
    assert "no Home Manager-eligible hosts" in result.stdout
    config.write_text(json.dumps(HOST_DATA))
    calls, _ = run("--nixos", host="mac", system="Darwin")
    assert ["sudo", "/run/current-system/sw/bin/darwin-rebuild", "switch", "--flake", ".#mac"] in calls
    calls, _ = run("--no-preflight", "--nixos", "node", MOCK_SWITCH_FAILURE="inhibited")
    assert [c[1] for c in calls if c[0] == "nixos-rebuild"] == ["switch", "boot"]
    calls, _ = run("--no-preflight", "--nixos", "node", status=7, MOCK_SWITCH_FAILURE="other")
    assert [c[1] for c in calls if c[0] == "nixos-rebuild"] == ["switch"]

    def rebuilt(calls):
        return [arg[2:] for c in calls if c[0] == "nixos-rebuild" for arg in c if arg.startswith(".#")]

    def summary_rows(result):
        return [line.split() for line in result.stdout.split("summary", 1)[1].splitlines()]

    # Serial hosts leave the parallel batch. After a one-shot ready pre-check
    # of the whole group, they switch one at a time in serialSystemHosts
    # order, each followed by its ready gate.
    config.write_text(json.dumps(SERIAL_DATA))
    calls, result = run("--all", "--nixos", "--no-preflight")
    parallel = next(c for c in calls if c[0] == "parallel")
    assert not any(f".#{h}" in job for job in parallel for h in ["k1", "k2", "k3"])
    assert rebuilt(calls) == ["node", "solo", "k1", "k2", "k3"]
    sequence = [(c[0], c[1]) for c in calls if c[0] == "ready-check" or (c[0] == "nixos-rebuild" and c[-1] != "--ask-sudo-password" and ".#node" not in c)]
    assert sequence == [
        ("ready-check", "k1"), ("ready-check", "k2"), ("ready-check", "k3"),
        ("nixos-rebuild", "switch"), ("ready-check", "k1"),
        ("ready-check", "k1"), ("ready-check", "k3"),
        ("nixos-rebuild", "switch"), ("ready-check", "k2"),
        ("ready-check", "k1"), ("ready-check", "k2"),
        ("nixos-rebuild", "switch"), ("ready-check", "k3"),
    ], sequence
    assert [c for c in calls if c[0] == "ready-check"][0] == ["ready-check", "k1", "--node=k1"]
    assert all(row in summary_rows(result) for row in [["k1", "ok", "-"], ["k2", "ok", "-"], ["k3", "ok", "-"]])
    assert "ready gate passed after 1 attempt(s)" in result.stdout
    # A gate that never passes is retried every interval until the timeout,
    # then the rest of the serial group is skipped and the run fails.
    calls, result = run("--all", "--nixos", "--no-preflight", status=1, MOCK_READY_FAIL_AFTER_SWITCH="k2")
    assert rebuilt(calls) == ["node", "solo", "k1", "k2"]
    assert [c[1] for c in calls if c[0] == "ready-check"] == ["k1", "k2", "k3", "k1", "k1", "k3", "k2", "k2", "k2"]
    # The local sudo keepalive also sleeps; it is not part of the gate.
    assert [c[1:] for c in calls if c[0] == "sleep" and c[1:] != ["60"]] == [["10"], ["10"]]
    rows = summary_rows(result)
    assert ["k1", "ok", "-"] in rows and ["k2", "failed", "-"] in rows and ["k3", "skipped", "-"] in rows
    assert ["node", "ok", "-"] in rows and ["solo", "ok", "-"] in rows
    assert "ready gate did not pass within 25s (3 attempt(s))" in result.stderr
    assert "serial rollout stopped: k2 failed the serial ready gate (serialReadyCommand)" in result.stderr
    assert "skipped serial hosts: k3" in result.stderr
    assert "one or more fleet jobs failed" in result.stderr
    # A failed serial switch skips its gate and the remaining serial hosts.
    calls, result = run("--all", "--nixos", "--no-preflight", status=1, MOCK_FAIL_FLAKE=".#k1")
    assert rebuilt(calls) == ["node", "solo", "k1"]
    assert [c[1] for c in calls if c[0] == "ready-check"] == ["k1", "k2", "k3"]  # pre-check only
    rows = summary_rows(result)
    assert ["k1", "failed", "-"] in rows and ["k2", "skipped", "-"] in rows and ["k3", "skipped", "-"] in rows
    assert "serial rollout stopped: k1 failed its system switch" in result.stderr
    assert "skipped serial hosts: k2 k3" in result.stderr
    # One unreachable serial host blocks the whole serial group, because
    # switching another member could break quorum. Other hosts proceed.
    calls, result = run("--all", "--nixos", status=1, MOCK_UNREACHABLE="k2.invalid")
    assert rebuilt(calls) == ["node", "solo"]
    assert not any(c[0] == "ready-check" for c in calls)
    rows = summary_rows(result)
    assert all(row in rows for row in [["k1", "skipped", "-"], ["k2", "skipped", "-"], ["k3", "skipped", "-"], ["node", "ok", "-"], ["solo", "ok", "-"]])
    assert "serial group blocked: k2 failed preflight; switching the others could break quorum" in result.stderr
    assert "skipped serial hosts: k1 k3" in result.stderr
    assert "one or more fleet jobs failed" in result.stderr
    # A serial host that is not ready before the rollout blocks the group too.
    calls, result = run("--all", "--nixos", "--no-preflight", status=1, MOCK_READY_FAIL="k2")
    assert rebuilt(calls) == ["node", "solo"]
    assert [c[1] for c in calls if c[0] == "ready-check"] == ["k1", "k2"]
    rows = summary_rows(result)
    assert all(row in rows for row in [["k1", "skipped", "-"], ["k2", "skipped", "-"], ["k3", "skipped", "-"], ["node", "ok", "-"], ["solo", "ok", "-"]])
    assert "serial group blocked: k2 not ready before rollout; switching the others could break quorum" in result.stderr
    assert "one or more fleet jobs failed" in result.stderr
    # Members are checked again before each later switch: k3 stops being
    # ready after k1 switches, so k2 is left untouched.
    calls, result = run("--all", "--nixos", "--no-preflight", status=1, MOCK_READY_FAIL_AFTER="k3:k1")
    assert rebuilt(calls) == ["node", "solo", "k1"]
    rows = summary_rows(result)
    assert ["k1", "ok", "-"] in rows and ["k2", "skipped", "-"] in rows and ["k3", "skipped", "-"] in rows
    assert "serial rollout stopped: k3 not ready before switching k2" in result.stderr
    assert "skipped serial hosts: k2 k3" in result.stderr
    assert "one or more fleet jobs failed" in result.stderr
    # Serial members outside this run's selection are pre-checked too.
    config.write_text(json.dumps({**SERIAL_DATA, "deployAllHosts": ["xyz", "node", "k1", "solo", "k2"]}))
    calls, result = run("--all", "--nixos", "--no-preflight", status=1, MOCK_READY_FAIL="k3")
    assert rebuilt(calls) == ["node", "solo"]
    assert [c[1] for c in calls if c[0] == "ready-check"] == ["k1", "k2", "k3"]
    rows = summary_rows(result)
    assert ["k1", "skipped", "-"] in rows and ["k2", "skipped", "-"] in rows
    assert "serial group blocked: k3 not ready before rollout" in result.stderr
    config.write_text(json.dumps(SERIAL_DATA))
    # An unreachable non-serial host does not block the serial group.
    calls, result = run("--all", "--nixos", MOCK_UNREACHABLE="node.invalid")
    assert rebuilt(calls) == ["solo", "k1", "k2", "k3"]
    assert "serial group blocked" not in result.stderr
    # A single-host deploy of a serial host runs and reports its gate.
    calls, result = run("--no-preflight", "--nixos", "k2")
    assert rebuilt(calls) == ["k2"] and ["ready-check", "k2", "--node=k2"] in calls
    assert "ready gate passed" in result.stdout
    calls, result = run("--no-preflight", "--nixos", "k2", status=1, MOCK_READY_FAIL_AFTER_SWITCH="k2", DEPLOY_SERIAL_READY_INTERVAL="30")
    # A pause as long as the remaining budget would leave no time to retry.
    assert [c[1] for c in calls if c[0] == "ready-check"] == ["k1", "k3", "k2"]
    assert not any(c[0] == "sleep" for c in calls)
    assert "k2 switched but failed the serial ready gate" in result.stderr
    # A single-host deploy is refused before switching if another member of
    # the serial group is not ready.
    calls, result = run("--no-preflight", "--nixos", "k1", status=1, MOCK_READY_FAIL="k2")
    assert rebuilt(calls) == []
    assert "serial group blocked: k2 not ready before rollout; switching k1 could break quorum" in result.stderr
    calls, _ = run("--no-preflight", "--nixos", "node")
    assert not any(c[0] == "ready-check" for c in calls)
    # A hanging gate attempt is killed at the deadline instead of blocking.
    config.write_text(json.dumps({**SERIAL_DATA, "serialReadyTimeoutSeconds": 2}))
    started = time.monotonic()
    calls, result = run("--no-preflight", "--nixos", "k2", status=1, MOCK_READY_HANG="k2")
    assert time.monotonic() - started < 15
    assert rebuilt(calls) == ["k2"]
    assert "ready gate did not pass within 2s (1 attempt(s))" in result.stderr
    # Regression: when a pause would consume exactly the remaining budget, the
    # gate fails instead of starting an attempt with no time limit.
    config.write_text(json.dumps({**SERIAL_DATA, "serialReadyTimeoutSeconds": 10}))
    started = time.monotonic()
    calls, result = run("--no-preflight", "--nixos", "k2", status=1, MOCK_READY_FAIL_AFTER_SWITCH="k2", MOCK_READY_HANG_RETRY="k2")
    assert time.monotonic() - started < 15
    assert [c[1] for c in calls if c[0] == "ready-check"] == ["k1", "k3", "k2"]
    assert not any(c[0] == "sleep" for c in calls)
    assert "ready gate did not pass within 10s (1 attempt(s))" in result.stderr
    config.write_text(json.dumps(SERIAL_DATA))
    calls, _ = run("--no-preflight", "--nixos", "k2", status=1, DEPLOY_SERIAL_READY_INTERVAL="0")
    assert not actions(calls)
    # Without a ready command, serial hosts still switch one at a time.
    config.write_text(json.dumps({key: value for key, value in SERIAL_DATA.items() if key != "serialReadyCommand"}))
    calls, _ = run("--all", "--nixos", "--no-preflight")
    assert rebuilt(calls) == ["node", "solo", "k1", "k2", "k3"]
    assert not any(c[0] == "ready-check" for c in calls)
    config.write_text(json.dumps(HOST_DATA))

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
    for invalid in [
        {"serialSystemHosts": ["missing"]},
        {"serialSystemHosts": ["k1", "k1"]},
        {"serialSystemHosts": "k1"},
        {"serialReadyCommand": ["ready-check", "{host}"]},
        {"serialReadyTimeoutSeconds": "600"},
        {"serialReadyTimeoutSeconds": 0},
        {"serialReadyTimeoutSeconds": 1.5},
        *({key: value} for key in ["serialSystemHosts", "serialReadyCommand", "serialReadyTimeoutSeconds"] for value in [False, None]),
    ]:
        reject_inventory({**SERIAL_DATA, **invalid}, "does not satisfy schemaVersion 1")

print("Deployment contract: 44 mocked CLI cases and 20 invalid inventories passed; no deployment commands executed")
