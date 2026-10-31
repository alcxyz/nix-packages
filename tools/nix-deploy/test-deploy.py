"""Black-box deployment planning tests; every mutating/network command is mocked."""

import json
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess
import sys
import tempfile
import time

# Bounds a wait that only stops a hung test, in seconds. CI can pause a whole
# job for minutes (#711), so a hang guard must outlast such a pause rather
# than measure speed; the assertions check behavior and timing themselves.
HANG_GUARD = 900

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
# Two independent serial groups: the quorum group above and a pair of DNS
# servers with their own ready command.
GROUP_DATA = {
    **SERIAL_DATA,
    "knownHosts": [*SERIAL_DATA["knownHosts"], "d1", "d2"],
    "deployAllHosts": [*SERIAL_DATA["deployAllHosts"], "d1", "d2"],
    "sshHosts": {**SERIAL_DATA["sshHosts"], "d1": "d1.invalid", "d2": "d2.invalid"},
    "serialGroups": [{"hosts": ["d1", "d2"], "readyCommand": "ready-check {host} --dns", "readyTimeoutSeconds": 25}],
}
MOCK = r'''
import json, os, pathlib, re, sys, time
name = pathlib.Path(sys.argv[0]).name
args = sys.argv[1:]
store = pathlib.Path(os.environ["MOCK_STORE"])
with open(os.environ["MOCK_LOG"], "a") as out:
    out.write(json.dumps([name, *args]) + "\n")

def listed(variable, value):
    return value in os.environ.get(variable, "").split(",")

def endpoint_host(endpoint):
    return endpoint.split("@")[-1].split(".")[0]

if name == "hostname":
    print(os.environ["MOCK_HOST"])
elif name == "uname":
    print(os.environ["MOCK_SYSTEM"])
elif name == "sudo":
    # Never execute the requested privileged command, including absolute paths.
    sys.exit(1 if args == ["-n", "-v"] else 0)
elif name == "activate":
    if os.environ.get("MOCK_HM_FAILURE"):
        print("fixture home-manager failure", file=sys.stderr)
        sys.exit(4)
elif name == "ssh":
    remote = args[-1]
    if remote == "true":
        if os.environ.get("MOCK_UNREACHABLE", "never.invalid") in args[-2]:
            print("fixture endpoint unavailable", file=sys.stderr)
            sys.exit(255)
    elif remote.startswith("df -Pk"):
        # Collection frees space only on the endpoint that ran it.
        logged = [json.loads(line) for line in open(os.environ["MOCK_LOG"])]
        collected = any(c[0] == "ssh" and "nix-gc.service" in c[-1] and c[-2] == args[-2] for c in logged)
        free = os.environ.get("MOCK_FREE_KIB", "99999999")
        print(os.environ.get("MOCK_FREE_KIB_AFTER_GC", free) if collected else free)
    elif "nix-gc.service" in remote:
        failure = os.environ.get("MOCK_GC_FAILURE")
        if failure == "hang":
            time.sleep(30)
        elif failure == "missing":
            sys.exit(3)
        elif failure:
            print("fixture gc failure", file=sys.stderr)
            sys.exit(1)
    elif "/run/booted-system" in remote:
        # nixfleet status reads the boot check, then the runner units. Linux
        # hosts booted kernel k1 and switched to k2; mac is nix-darwin.
        host = endpoint_host(args[-2])
        if listed("MOCK_STATUS_UNREACHABLE", host):
            print("fixture endpoint unavailable", file=sys.stderr)
            sys.exit(255)
        if host == "mac":
            print("os=Darwin")
            sys.exit(0)
        print("os=Linux")
        for side, system, kernel in [("booted", "old", "k1"), ("current", "new", "k2"), ("profile", "new", "k2")]:
            print(f"{side}={store}/{system}-system")
            print(f"{side}-kernel={store}/{kernel}-linux/bzImage")
            print(f"{side}-initrd={store}/i1-initrd/initrd")
            print(f"{side}-kernel-modules={store}/m1-modules")
        print("runner=forgejo-actions-runner.service loaded active running fixture runner")
    elif "/run/current-system" in remote or "profiles/home-manager" in remote:
        # Hosts listed in MOCK_CURRENT already run what the fixture builds.
        # Both scripts print the running generation, then the profile. A Home
        # Manager activation that stopped early moved the profile only.
        host = endpoint_host(args[-2])
        kind = "home" if "home-manager" in remote else "system"
        current = store / (f"fixture-{host}-{kind}" if listed("MOCK_CURRENT", host) else f"old-{host}-{kind}")
        profile = store / f"fixture-{host}-{kind}" if listed("MOCK_PROFILE_ONLY", host) else current
        print(f"{current}\n{profile}")
elif name == "nix":
    if args[:2] == ["config", "show"] or args == ["show-config"]:
        # This machine's settings, which cap the heavy build profile. Nix
        # before 2.20 has only show-config; MOCK_NIX_CONFIG=none has neither.
        settings = {"max-jobs": os.environ.get("MOCK_MAX_JOBS", "32"), "cores": os.environ.get("MOCK_CORES", "16")}
        old = os.environ.get("MOCK_NIX_CONFIG")
        if args[0] == "config" and old in {"old", "none"} or args[0] == "show-config" and old == "none":
            print("error: 'config' is not a recognised command", file=sys.stderr)
            sys.exit(1)
        if args[0] == "config":
            print(settings[args[2]])
        else:
            print("\n".join(f"{key} = {value}" for key, value in settings.items()))
    elif args[0] == "eval":
        attribute = next(a for a in args if a.startswith(".#"))
        match = re.match(r'\.#(nixos|darwin|home)Configurations\."(?:alc-)?([^"]+)"\.', attribute)
        kind = {"nixos": "system", "darwin": "darwin", "home": "home"}[match[1]]
        derivation = f"{match[2]}-{kind}"
        if listed("MOCK_EVAL_FAIL", derivation):
            print(f"error: fixture evaluation failure in {derivation}", file=sys.stderr)
            sys.exit(1)
        if os.environ.get("MOCK_EVAL_LOCK"):
            # Nix rewrites a lock that no longer matches flake.nix.
            pathlib.Path("flake.lock").write_text(os.environ["MOCK_EVAL_LOCK"])
        # MOCK_VARIANT builds another generation of the same configuration.
        output = f"{store}/fixture{os.environ.get('MOCK_VARIANT', '')}-{derivation}"
        print(json.dumps({"drv": output + ".drv", "out": output}))
    elif args[0] == "build" and "--dry-run" in args:
        # The count before the build: MOCK_LOCAL_BUILDS derivations would be
        # built here, and one path fetched.
        if os.environ.get("MOCK_DRY_RUN_FAIL"):
            print("error: fixture dry-run failure", file=sys.stderr)
            sys.exit(1)
        count = int(os.environ.get("MOCK_LOCAL_BUILDS", "0"))
        # After the shared build, a derivation in MOCK_BUILD_FAIL is still to
        # be built, as Nix lists a failed one.
        logged = [json.loads(line) for line in open(os.environ["MOCK_LOG"])]
        failing = [a[: -len("^out")] for a in args if a.endswith(".drv^out") and listed("MOCK_BUILD_FAIL", re.sub(r"^fixture\d*-", "", pathlib.Path(a[: -len(".drv^out")]).name))]
        if failing and any(c[:2] == ["nix", "build"] and "--keep-going" in c for c in logged):
            print(f"these {len(failing)} derivations will be built:", file=sys.stderr)
            for drv in failing:
                print(f"  {drv}", file=sys.stderr)
        elif os.environ.get("MOCK_DRY_RUN_RENAMED"):
            print(f"these {count} derivations are queued:", file=sys.stderr)
            print(f"  {store}/queued.drv", file=sys.stderr)
        elif count:
            print(f"these {count} derivations will be built:" if count > 1 else "this derivation will be built:", file=sys.stderr)
            for i in range(count):
                print(f"  {store}/local-{i}.drv", file=sys.stderr)
        print("this path will be fetched (1.0 KiB download, 1.0 KiB unpacked):", file=sys.stderr)
        print(f"  {store}/fetched", file=sys.stderr)
    elif args[0] == "build":
        # A build of one derivation with an out-link roots a built target;
        # MOCK_ROOT_FAIL refuses one.
        targets = [a for a in args if a.endswith(".drv^out")]
        if "--keep-going" not in args and len(targets) == 1 and listed("MOCK_ROOT_FAIL", re.sub(r"^fixture\d*-", "", pathlib.Path(targets[0][: -len(".drv^out")]).name)):
            sys.exit(1)
        failed = False
        for arg in args:
            if not arg.endswith(".drv^out"):
                continue
            output = pathlib.Path(arg[: -len(".drv^out")])
            if listed("MOCK_BUILD_FAIL", re.sub(r"^fixture\d*-", "", output.name)):
                print(f"error: fixture build failure in {output.name}", file=sys.stderr)
                failed = True
                continue
            output.mkdir(parents=True, exist_ok=True)
            if output.name.endswith("-home"):
                (output / "gen-version").write_text("1")
                activate = output / "activate"
                activate.write_text(pathlib.Path(sys.argv[0]).read_text())
                activate.chmod(0o755)
        sys.exit(1 if failed else 0)
    elif args[:2] == ["derivation", "show"]:
        # The fixture's derivations have no input derivations.
        print(json.dumps({"derivations": {pathlib.Path(a).name: {"inputs": {"drvs": {}, "srcs": []}} for a in args[2:]}, "version": 4}))
    elif args[0] == "log":
        # Nix keeps the log of a failed build only.
        name = re.sub(r"^fixture\d*-", "", pathlib.Path(args[-1]).name[: -len(".drv")])
        if not listed("MOCK_BUILD_FAIL", name):
            print(f"error: build log of '{args[-1]}' is not available", file=sys.stderr)
            sys.exit(1)
        print(f"fixture full build log of {name}")
    elif args[0] == "path-info":
        output = pathlib.Path(args[1][: -len(".drv^out")])
        if not output.is_dir():
            print(f"error: path '{output}' is not valid", file=sys.stderr)
            sys.exit(1)
        print(output)
elif name == "ready-check":
    logged = [json.loads(line) for line in open(os.environ["MOCK_LOG"])]
    def switches(host):
        return [i for i, c in enumerate(logged) if c[0] == "nixos-rebuild" and any(re.search(rf"fixture\d*-{host}-system", a) for a in c)]
    switched = switches(args[0])
    gate_attempt = switched and sum(c == ["ready-check", *args] for c in logged[switched[0]:])
    if listed("MOCK_READY_HANG", args[0]):
        time.sleep(30)
    if gate_attempt and gate_attempt > 1 and listed("MOCK_READY_HANG_RETRY", args[0]):
        time.sleep(30)
    late = dict(item.split(":") for item in os.environ.get("MOCK_READY_FAIL_AFTER", "").split(",") if item)
    late_failure = args[0] in late and switches(late[args[0]])
    if late_failure or listed("MOCK_READY_FAIL", args[0]) or (switched and listed("MOCK_READY_FAIL_AFTER_SWITCH", args[0])):
        print("fixture node not ready", file=sys.stderr)
        sys.exit(1)
elif name == "kubectl":
    # nixfleet reboot's reads: the hosts in MOCK_NODES are the cluster's
    # nodes, no database primaries run on them, and the first runs a core
    # singleton.
    if args[:2] == ["get", "nodes"]:
        nodes = [n for n in os.environ.get("MOCK_NODES", "").split(",") if n]
        print(json.dumps({"items": [{"metadata": {"name": n}, "spec": {}, "status": {"conditions": [{"type": "Ready", "status": "True"}]}} for n in nodes]}))
    elif args == ["get", "pods", "-A", "-o", "json", "--request-timeout=10s"]:
        nodes = [n for n in os.environ.get("MOCK_NODES", "").split(",") if n]
        print(json.dumps({"items": [{
            "metadata": {"namespace": "platform", "name": "forge-0", "ownerReferences": [{"kind": "StatefulSet", "name": "forge", "controller": True}]},
            "spec": {"nodeName": n, "priorityClassName": "core"}, "status": {"phase": "Running"}} for n in nodes[:1]]}))
    else:
        sys.exit(1)
elif name == "nixos-rebuild":
    if args == ["--no-reexec", "--store-path", "/nonexistent"]:
        # deploy's capability probe: a frontend that knows both options only
        # reports the missing action; the classic one rejects an option.
        if os.environ.get("MOCK_CLASSIC_REBUILD"):
            print("nixos-rebuild: unknown option `--no-reexec'", file=sys.stderr)
            sys.exit(1)
        print("nixos-rebuild: error: No valid subcommands. Type nixos-rebuild --help for more information", file=sys.stderr)
        sys.exit(2)
    path = args[args.index("--store-path") + 1]
    host = re.search(r"fixture\d*-([^-]+)-system", path)[1]
    if listed("MOCK_REBUILD_HANG", host):
        time.sleep(30)
    if listed("MOCK_FAIL_HOST", host):
        print("fixture rebuild failure")
        sys.exit(7)
    failure = os.environ.get("MOCK_SWITCH_FAILURE")
    if args and args[0] == "switch" and failure:
        print("Pre-switch check 'switchInhibitors' failed" if failure == "inhibited" else "fixture rebuild failure")
        sys.exit(7)
'''


def actions(calls):
    """Calls that evaluate, build or change a host."""
    return [
        c for c in calls
        if c[0] in {"nixos-rebuild", "home-manager", "nix", "activate"}
        or (c[0] == "sudo" and len(c) > 2 and c[1] not in {"-v", "-n"})
        or (c[0] == "ssh" and "/activate" in c[-1])
    ]


def activations(calls):
    """Calls that change a host."""
    return [
        c for c in calls
        if c[0] in {"nixos-rebuild", "activate"}
        or (c[0] == "sudo" and len(c) > 2 and c[1] != "-v" and c[1:] != ["-n", "-v"])
        or (c[0] == "nix" and c[1] == "copy")
        or (c[0] == "ssh" and "/activate" in c[-1])
    ]


def evaluated(calls):
    return [next(a for a in c if a.startswith(".#")) for c in calls if c[0] == "nix" and c[1] == "eval"]


def counted(calls):
    """Derivations passed to the dry run that counts local builds."""
    return [Path(a[: -len(".drv^out")]).name for c in calls if c[:3] == ["nix", "build", "--dry-run"] for a in c if a.endswith(".drv^out")]


def built(calls):
    """Derivations passed to the shared build."""
    return [Path(a[: -len(".drv^out")]).name for c in calls if c[0] == "nix" and c[1] == "build" and "--keep-going" in c for a in c if a.endswith(".drv^out")]


with tempfile.TemporaryDirectory(prefix="deploy-contract-") as directory:
    root = Path(directory)
    commands = root / "bin"
    commands.mkdir()
    store = root / "store"
    # The program is chosen by the name it is invoked as, so the path is not
    # resolved through a symbolic link named deploy.
    deploy = Path(sys.argv[1]).absolute()
    # Keep the command search path closed; new operational commands cannot fall
    # through to an installed host tool. Mocks use an absolute Python shebang,
    # the ready command uses bash, and report permissions use GNU ls.
    for name in ["bash", "ls"]:
        target = shutil.which(name)
        assert target, name
        (commands / name).symlink_to(target)
    config = root / "inventory with spaces.json"
    config.write_text(json.dumps(HOST_DATA))
    # The flake deployed is the working directory's; its lock goes into
    # every run record.
    (root / "flake.lock").write_text('{"nodes": {"root": {}}, "root": "root", "version": 7}\n')
    (root / "flake.nix").write_text("{ outputs = _: { }; }\n")
    for name in ["hostname", "uname", "sudo", "ssh", "nix", "nix-env", "nixos-rebuild", "home-manager", "ready-check"]:
        command = commands / name
        command.write_text("#!" + sys.executable + "\n" + MOCK)
        command.chmod(0o755)

    def system_path(host):
        return str(store / f"fixture-{host}-system")

    def home_path(host):
        return str(store / f"fixture-{host}-home")

    def run(*args, host="xyz", system="Linux", status=0, keep_state=False, **settings):
        log = root / "commands.jsonl"
        log.write_text("")
        shutil.rmtree(store, ignore_errors=True)
        store.mkdir()
        if not keep_state:
            # Each case starts without remembered failed activations.
            shutil.rmtree(root / ".local/state/nix-deploy", ignore_errors=True)
        env = {
            "PATH": str(commands),
            "HOME": str(root), "MOCK_LOG": str(log), "MOCK_STORE": str(store),
            "MOCK_HOST": host, "MOCK_SYSTEM": system,
            "NIX_SSHOPTS": "-o ConnectTimeout=1", "NO_COLOR": "1",
            **settings,
        }
        # No inherited stdin: bash builds that detect SSH by a socket on stdin
        # would read the system bashrc and reset the mocked PATH.
        result = subprocess.run([str(deploy), "--config", str(config), *args], cwd=root, env=env, stdin=subprocess.DEVNULL, capture_output=True, text=True, timeout=HANG_GUARD)
        calls = [json.loads(line) for line in log.read_text().splitlines()]
        assert result.returncode == status, (args, result.returncode, result.stdout, result.stderr)
        return calls, result

    def rebuilt(calls):
        return [m[1] for c in calls if c[0] == "nixos-rebuild" for a in c for m in [re.search(r"fixture\d*-([^-]+)-system", a)] if m]

    def summary_rows(result):
        return [line.split()[:3] for line in result.stdout.split("==> summary", 1)[1].splitlines()]

    calls, _ = run("--help", status=1)
    assert not actions(calls)
    # --version needs no inventory or environment.
    version = subprocess.run([str(deploy), "--version"], env={}, stdin=subprocess.DEVNULL, capture_output=True, text=True, timeout=HANG_GUARD)
    assert version.returncode == 0 and re.fullmatch(r"deploy \S+\n", version.stdout), version
    version = version.stdout.split()[1]
    # As the value of another option it is a file name, not --version.
    calls, _ = run("--config", "--version", status=1)
    calls, _ = run("--no-preflight", "node", "--json", "--version")
    assert json.loads((root / "--version").read_text())["version"] == version
    calls, _ = run("unknown", status=1)
    assert not actions(calls)
    # Everything is built in one step before anything is activated.
    calls, result = run()
    # Evaluations run concurrently, so their order varies.
    assert sorted(evaluated(calls)) == [
        '.#homeConfigurations."alc-xyz".activationPackage',
        '.#nixosConfigurations."xyz".config.system.build.toplevel',
    ]
    builds = [c for c in calls if c[0] == "nix" and c[1] == "build" and "--keep-going" in c]
    assert len(builds) == 1 and builds[0][:3] == ["nix", "build", "--keep-going"] and builds[0][3] == "--out-link"
    assert builds[0][5:] == [system_path("xyz") + ".drv^out", home_path("xyz") + ".drv^out"]
    assert ["sudo", str(commands / "nixos-rebuild"), "switch", "--store-path", system_path("xyz"), "--no-reexec"] in calls
    assert ["activate", "--driver-version", "1"] in calls
    assert calls.index(builds[0]) < calls.index(["sudo", str(commands / "nixos-rebuild"), "switch", "--store-path", system_path("xyz"), "--no-reexec"])
    # Built paths stay rooted until their activation: the shared build links
    # every output, and each built target is rooted again on its own.
    roots = [c for c in calls if c[0] == "nix" and c[1] == "build" and "--keep-going" not in c and "--dry-run" not in c]
    assert [c[-1] for c in roots] == [system_path("xyz") + ".drv^out", home_path("xyz") + ".drv^out"]
    assert all(c[2] == "--out-link" for c in roots) and builds[0][4].endswith("/build")
    assert max(calls.index(c) for c in roots) < calls.index(["sudo", str(commands / "nixos-rebuild"), "switch", "--store-path", system_path("xyz"), "--no-reexec"])
    assert "Home Manager NOT updated" not in result.stderr
    # A run that builds little here keeps the host's Nix settings.
    assert counted(calls) == ["fixture-xyz-system", "fixture-xyz-home"]
    assert "deploy: 0 derivations to build (heavy from 50); light build profile: host defaults" in result.stdout
    # A Home Manager failure after a successful system switch is called out,
    # with the command that brings the host back in step.
    calls, result = run(status=4, MOCK_HM_FAILURE="1")
    assert ["sudo", str(commands / "nixos-rebuild"), "switch", "--store-path", system_path("xyz"), "--no-reexec"] in calls
    assert "[xyz] system updated, Home Manager NOT updated" in result.stderr
    assert "run: deploy --hm xyz" in result.stderr
    # A Home Manager-only deployment leaves the system alone; no warning.
    calls, result = run("--hm", status=4, MOCK_HM_FAILURE="1")
    assert "Home Manager NOT updated" not in result.stderr
    assert not any(c[0] == "sudo" for c in calls)
    calls, _ = run("--no-preflight", "--nixos", "edge")
    assert ["nixos-rebuild", "switch", "--store-path", system_path("node"), "--no-reexec", "--target-host", "root@node.invalid", "--use-substitutes"] in calls
    # Without preflight, SSH is only used to read the current generation.
    assert all("/run/current-system" in c[-1] for c in calls if c[0] == "ssh")
    calls, _ = run("--hm", "node")
    assert evaluated(calls) == ['.#homeConfigurations."alc-node".activationPackage']
    assert ["nix", "copy", "--substitute-on-destination", "--to", "ssh://alc@node.invalid", home_path("node")] in calls
    # The profile is set before activation, as the home-manager command does.
    # It runs under sh whatever the remote login shell is.
    assert any(c[0] == "ssh" and c[-2] == "alc@node.invalid" and c[-1].startswith("sh -c '") and "max-jobs = 1" in c[-1]
               and f'g="{home_path("node")}"' in c[-1] and 'nix-env --profile "$d/home-manager" --set "$g"' in c[-1]
               and "--driver-version 1" in c[-1] for c in calls)
    calls, _ = run("--no-preflight", "solo")
    assert ["nixos-rebuild", "boot", "--store-path", system_path("solo"), "--no-reexec", "--target-host", "operator@solo.invalid", "--use-substitutes", "--ask-sudo-password"] in calls
    assert not any("alc-solo" in arg or "solo-home" in arg for c in calls for arg in c)
    calls, _ = run("--hm", "solo", status=1)
    assert not actions(calls)
    calls, _ = run("--nixos", "node", status=1, MOCK_UNREACHABLE="node.invalid")
    assert not actions(calls)
    def remote_gc(calls):
        return [c[-2] for c in calls if c[0] == "ssh" and "nix-gc.service" in c[-1]]

    calls, _ = run("--nixos", "node", status=1, MOCK_FREE_KIB="1")
    assert not actions(calls)
    assert remote_gc(calls) == ["root@node.invalid"]
    # Low space runs the target's own GC once, then checks again.
    calls, result = run("--nixos", "node", MOCK_FREE_KIB="1", MOCK_FREE_KIB_AFTER_GC="99999999")
    assert remote_gc(calls) == ["root@node.invalid"]
    assert rebuilt(calls) == ["node"]
    assert "running nix-gc.service on the host" in result.stdout
    calls, result = run("--nixos", "node", status=1, MOCK_FREE_KIB="1", MOCK_GC_FAILURE="1")
    assert not actions(calls)
    assert "nix-gc.service failed: fixture gc failure" in result.stderr
    calls, result = run("--nixos", "node", status=1, MOCK_FREE_KIB="1", MOCK_GC_FAILURE="missing")
    assert not actions(calls)
    assert "no nix-gc.timer on the host" in result.stderr
    # A failed collection may still have freed enough space.
    calls, _ = run("--nixos", "node", MOCK_FREE_KIB="1", MOCK_FREE_KIB_AFTER_GC="99999999", MOCK_GC_FAILURE="1")
    assert rebuilt(calls) == ["node"]
    # A stalled collection is abandoned at the deadline.
    started = time.monotonic()
    calls, result = run("--nixos", "node", status=1, MOCK_FREE_KIB="1", MOCK_GC_FAILURE="hang", DEPLOY_REMOTE_GC_TIMEOUT="1")
    assert time.monotonic() - started < 15
    assert not actions(calls)
    assert "nix-gc.service did not finish within 1s" in result.stderr
    calls, _ = run("--nixos", "node", status=1, MOCK_FREE_KIB="1", DEPLOY_REMOTE_GC="0")
    assert not actions(calls) and not remote_gc(calls)
    # A non-root system user cannot start the unit without a sudo prompt.
    calls, _ = run("--nixos", "solo", status=1, MOCK_FREE_KIB="1", MOCK_FREE_KIB_AFTER_GC="99999999")
    assert not actions(calls) and not remote_gc(calls)
    calls, _ = run("--nixos", "node", status=1, DEPLOY_REMOTE_GC="yes")
    assert not actions(calls)
    calls, _ = run("--nixos", "node", status=1, DEPLOY_REMOTE_GC_TIMEOUT="0")
    assert not actions(calls)
    calls, _ = run("--nixos", "node", status=1, DEPLOY_EVAL_JOBS="0")
    assert not actions(calls)
    # In a fleet, the root host is collected and deployed; the non-root host
    # gets no collection and is skipped.
    calls, result = run("--all", "--nixos", MOCK_FREE_KIB="1", MOCK_FREE_KIB_AFTER_GC="99999999")
    assert remote_gc(calls) == ["root@node.invalid"]
    assert rebuilt(calls) == ["node"]
    assert ["solo", "skipped", "-"] in summary_rows(result)
    calls, _ = run("--nixos", "node", status=1, MOCK_FREE_KIB="invalid")
    assert not actions(calls)
    calls, _ = run("--no-preflight", "--nixos", "node", MOCK_UNREACHABLE="node.invalid")
    assert rebuilt(calls) == ["node"]
    calls, _ = run("--all", "--nixos", host="mac", system="Darwin", status=1)
    assert not actions(calls)
    calls, _ = run("--all", "--nixos", MOCK_UNREACHABLE="node.invalid")
    assert rebuilt(calls) == ["solo"]
    assert not any('nixosConfigurations."node"' in arg for arg in evaluated(calls))
    # The local sudo prompt comes before preflight, not after it.
    assert calls.index(["sudo", "-v"]) < min(i for i, c in enumerate(calls) if c[0] == "ssh")
    calls, _ = run("--all", "--nixos", "--fail-unreachable", status=1, MOCK_UNREACHABLE="node.invalid")
    assert not actions(calls)
    calls, _ = run("--all", MOCK_UNREACHABLE="root@node.invalid")
    assert "node" not in rebuilt(calls)
    assert '.#homeConfigurations."alc-node".activationPackage' in evaluated(calls)
    assert not any("alc-solo" in arg for arg in evaluated(calls))
    calls, _ = run("--all", "--fail-unreachable", status=1, MOCK_UNREACHABLE="root@node.invalid")
    assert not actions(calls)
    # Both phases are checked before anything is built, so an unreachable Home
    # Manager endpoint stops the run before any system switches.
    calls, _ = run("--all", "--fail-unreachable", status=1, MOCK_UNREACHABLE="alc@node.invalid")
    assert not actions(calls)
    # A failed system switch must not skip Home Manager for the fleet, and the
    # run still fails with a per-host summary.
    calls, result = run("--all", "--no-preflight", status=1, MOCK_SWITCH_FAILURE="other")
    assert any(c[0] == "nixos-rebuild" and c[1] == "switch" and system_path("node") in c for c in calls)
    assert ["activate", "--driver-version", "1"] in calls
    assert ["nix", "copy", "--substitute-on-destination", "--to", "ssh://alc@node.invalid", home_path("node")] in calls
    rows = summary_rows(result)
    assert ["node", "failed", "ok"] in rows
    assert ["xyz", "ok", "ok"] in rows
    assert ["solo", "ok", "-"] in rows
    assert "not selected by --all: mac" in result.stdout
    assert "shared build:" in result.stdout
    assert "one or more fleet jobs failed" in result.stderr
    assert "Home Manager NOT updated" not in result.stderr
    # The fleet summary is followed by an explicit warning for hosts whose
    # system switched while Home Manager failed.
    calls, result = run("--all", "--no-preflight", status=1, MOCK_HM_FAILURE="1")
    assert ["xyz", "ok", "failed"] in summary_rows(result)
    assert "[xyz] system updated, Home Manager NOT updated" in result.stderr
    assert "[node] system updated" not in result.stderr
    # Output of parallel jobs carries the phase and host.
    assert "[hm] [xyz]\tfixture home-manager failure" in result.stderr.splitlines()
    # Preflight-skipped hosts are reported as skipped and do not fail the run.
    calls, result = run("--all", "--nixos", MOCK_UNREACHABLE="node.invalid")
    assert ["node", "skipped", "-"] in summary_rows(result)
    calls, result = run("--all", "--hm", "--no-preflight")
    assert '.#homeConfigurations."alc-node".activationPackage' in evaluated(calls)
    assert not any("alc-solo" in arg for arg in evaluated(calls))
    assert ["activate", "--driver-version", "1"] in calls
    assert not any(c[0] in {"nixos-rebuild", "home-manager"} for c in calls)
    calls, _ = run("--all", "--here", "--no-preflight", "--nixos", host="mac", system="Darwin")
    assert rebuilt(calls) == ["node", "solo"]
    assert not any("--build-host" in c for c in calls)
    assert not any("xyz" in arg for arg in evaluated(calls))
    assert not any(c[0] == "sudo" for c in calls)
    config.write_text(json.dumps({**HOST_DATA, "deployAllHosts": ["solo"]}))
    calls, result = run("--all", "--hm")
    assert not actions(calls)
    assert not any(c[0] in {"ssh", "sudo"} for c in calls)
    assert "no Home Manager-eligible hosts" in result.stdout
    config.write_text(json.dumps(HOST_DATA))
    calls, _ = run("--nixos", host="mac", system="Darwin")
    assert evaluated(calls) == ['.#darwinConfigurations."mac".system']
    assert ["sudo", "/run/current-system/sw/bin/darwin-rebuild", "switch", "--flake", ".#mac"] in calls
    calls, _ = run("--no-preflight", "--nixos", "node", MOCK_SWITCH_FAILURE="inhibited")
    assert [c[1] for c in calls if c[0] == "nixos-rebuild" and "/nonexistent" not in c] == ["switch", "boot"]
    calls, _ = run("--no-preflight", "--nixos", "node", status=7, MOCK_SWITCH_FAILURE="other")
    assert [c[1] for c in calls if c[0] == "nixos-rebuild" and "/nonexistent" not in c] == ["switch"]
    # The switch-inhibitor fallback applies to fleet runs too.
    calls, _ = run("--all", "--nixos", "--no-preflight", MOCK_SWITCH_FAILURE="inhibited")
    assert [c[1] for c in calls if c[0] == "nixos-rebuild" and system_path("node") in c] == ["switch", "boot"]

    # Hosts that already run what was built are left alone: no copy, no
    # activation. The local host is compared with its own profile.
    calls, result = run("--no-preflight", "--nixos", "node", MOCK_CURRENT="node")
    assert not activations(calls)
    assert "[system] [node] unchanged: fixture-node-system" in result.stdout
    assert "node is up to date; nothing to activate" in result.stdout
    calls, result = run("--hm", "node", MOCK_CURRENT="node")
    assert not activations(calls)
    calls, result = run("--no-preflight", "--nixos", "node")
    assert "[system] [node] changed: old-node-system -> fixture-node-system" in result.stdout
    # --force activates anyway.
    calls, result = run("--no-preflight", "--nixos", "node", "--force", MOCK_CURRENT="node")
    assert rebuilt(calls) == ["node"]
    assert "activating anyway (--force)" in result.stdout
    # A host staged with 'boot' is compared by its profile alone.
    calls, _ = run("--no-preflight", "--nixos", "solo", MOCK_CURRENT="solo")
    assert not activations(calls)
    # Home Manager counts as unchanged only when its last activation finished,
    # not when an interrupted one already moved the profile.
    calls, result = run("--hm", "node", MOCK_PROFILE_ONLY="node")
    assert any(c[0] == "ssh" and "/activate" in c[-1] for c in calls)
    # A target that cannot be rooted is not activated.
    calls, result = run("--all", "--nixos", "--no-preflight", status=1, MOCK_ROOT_FAIL="node-system")
    assert rebuilt(calls) == ["solo"]
    assert "[system] [node] cannot protect the build from garbage collection" in result.stderr
    # A dry run never starts the remote garbage collection; it assumes the
    # collection a real run would start frees enough space, and plans on.
    calls, result = run("--nixos", "node", "--dry-run", MOCK_FREE_KIB="1", MOCK_FREE_KIB_AFTER_GC="99999999")
    assert not remote_gc(calls) and not activations(calls)
    assert "dry run: not starting nix-gc.service" in result.stdout and "[system] [node] changed:" in result.stdout
    # A generation whose last activation from this machine failed is
    # activated again even though the host's links already point to it.
    calls, _ = run("--no-preflight", "--nixos", "node", status=7, MOCK_FAIL_HOST="node")
    calls, result = run("--no-preflight", "--nixos", "node", keep_state=True, MOCK_CURRENT="node")
    assert rebuilt(calls) == ["node"] and "last activation of fixture-node-system from this machine failed" in result.stdout
    calls, result = run("--no-preflight", "--nixos", "node", keep_state=True, MOCK_CURRENT="node")
    assert rebuilt(calls) == [] and "[system] [node] unchanged" in result.stdout
    calls, _ = run("--all", "--no-preflight", status=1, MOCK_HM_FAILURE="1")
    calls, result = run("--hm", keep_state=True)
    assert ["activate", "--driver-version", "1"] in calls
    # The local Home Manager activation sets the profile first.
    assert any(c[0] == "nix-env" and c[-2:] == ["--set", home_path("xyz")] for c in calls)
    # An operator whose nixos-rebuild cannot activate store paths switches
    # nothing; the probe passes the options without an action.
    calls, result = run("--no-preflight", "--nixos", "node", status=1, MOCK_CLASSIC_REBUILD="1")
    assert rebuilt(calls) == [] and "cannot activate a built store path" in result.stderr
    assert ["nixos-rebuild", "--no-reexec", "--store-path", "/nonexistent"] in calls
    calls, result = run("--all", "--no-preflight", status=1, MOCK_CLASSIC_REBUILD="1")
    assert rebuilt(calls) == [] and ["node", "failed", "ok"] in summary_rows(result)
    # Nothing is activated unless its unfinished-activation entry is saved.
    state = root / ".local/state/nix-deploy"
    state.parent.mkdir(parents=True, exist_ok=True)
    shutil.rmtree(state, ignore_errors=True)
    state.write_text("not a directory")
    calls, result = run("--no-preflight", "--nixos", "node", status=1, keep_state=True)
    assert rebuilt(calls) == [] and "cannot lock system node for activation" in result.stderr
    calls, result = run("--all", "--no-preflight", status=1, keep_state=True)
    assert rebuilt(calls) == [] and not any(c[0] == "ssh" and "/activate" in c[-1] for c in calls)
    state.unlink()
    # A record that cannot be read counts as unfinished, so a target whose
    # links match is not passed as unchanged; without a usable record it is
    # not activated either, and the run fails.
    state.mkdir()
    (state / "failed-activations.json").write_text("{damaged")
    calls, result = run("--no-preflight", "--nixos", "node", status=1, keep_state=True, MOCK_CURRENT="node")
    assert rebuilt(calls) == [] and "unchanged" not in result.stdout and "cannot use the activation record" in result.stderr
    shutil.rmtree(state)
    # An activation that never returns, here interrupted, is retried too.
    log = root / "commands.jsonl"
    log.write_text("")
    shutil.rmtree(root / ".local/state/nix-deploy", ignore_errors=True)
    env = {"PATH": str(commands), "HOME": str(root), "MOCK_LOG": str(log), "MOCK_STORE": str(store), "MOCK_HOST": "xyz",
           "MOCK_SYSTEM": "Linux", "NIX_SSHOPTS": "-o ConnectTimeout=1", "NO_COLOR": "1", "MOCK_REBUILD_HANG": "node"}
    report = root / "report.json"
    report.write_text(json.dumps({"failed": False}))
    interrupted = subprocess.Popen([str(deploy), "--config", str(config), "--no-preflight", "--nixos", "node", "--json", str(report)], cwd=root, env=env,
                                   stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, start_new_session=True)
    deadline = time.monotonic() + HANG_GUARD
    while not any(json.loads(line)[0] == "nixos-rebuild" and "switch" in json.loads(line) for line in log.read_text().splitlines()):
        assert time.monotonic() < deadline and interrupted.poll() is None
        time.sleep(0.1)
    os.killpg(interrupted.pid, signal.SIGINT)
    assert interrupted.wait(timeout=HANG_GUARD) == 130
    assert json.loads(report.read_text())["failed"]
    calls, result = run("--no-preflight", "--nixos", "node", keep_state=True, MOCK_CURRENT="node")
    assert rebuilt(calls) == ["node"] and "from this machine failed" in result.stdout
    # A later failure does not hide an earlier one the host may still run.
    calls, _ = run("--no-preflight", "--nixos", "node", status=7, MOCK_FAIL_HOST="node")
    calls, _ = run("--no-preflight", "--nixos", "node", keep_state=True, status=7, MOCK_FAIL_HOST="node", MOCK_VARIANT="2")
    calls, result = run("--no-preflight", "--nixos", "node", keep_state=True, MOCK_CURRENT="node")
    assert rebuilt(calls) == ["node"] and "from this machine failed" in result.stdout
    # --dry-run stops after the plan, before anything is built.
    calls, result = run("--no-preflight", "--dry-run")
    assert not activations(calls) and not built(calls) and not any(c[0] == "nix" and c[1] == "build" and "--dry-run" not in c for c in calls)
    assert "[system] [xyz] changed:" in result.stdout or "[system] [xyz] current generation unknown" in result.stdout
    calls, result = run("--all", "--dry-run", "--no-preflight", MOCK_CURRENT="node")
    assert not activations(calls) and not any(c == ["sudo", "-v"] for c in calls)
    rows = summary_rows(result)
    assert ["node", "unchanged", "unchanged"] in rows and ["solo", "planned", "-"] in rows
    # --build-only evaluates, plans and builds like a real run, and stops
    # before anything changes a host: no preflight (so no remote garbage
    # collection), no sudo prompt, no copy and no activation.
    calls, result = run("--all", "--build-only", MOCK_CURRENT="node", MOCK_FREE_KIB="1")
    assert not activations(calls) and not remote_gc(calls) and ["sudo", "-v"] not in calls
    assert all("/run/current-system" in c[-1] or "profiles/home-manager" in c[-1] for c in calls if c[0] == "ssh")
    assert sorted(built(calls)) == ["fixture-solo-system", "fixture-xyz-home", "fixture-xyz-system"]
    rows = summary_rows(result)
    assert ["node", "unchanged", "unchanged"] in rows and ["solo", "built", "-"] in rows and ["xyz", "built", "built"] in rows
    [record] = list((root / ".local/state/nix-deploy/runs").iterdir())
    info = json.loads((record / "run.json").read_text())
    assert info["buildOnly"] and not info["dryRun"] and not info["failed"]
    calls, result = run("--nixos", "node", "--build-only", "--force", MOCK_CURRENT="node")
    assert built(calls) == ["fixture-node-system"] and not activations(calls)
    assert "building anyway (--force)" in result.stdout and "node built; nothing was activated" in result.stdout
    calls, result = run("node", "--build-only", MOCK_CURRENT="node")
    assert not built(calls) and "node is up to date; nothing to build" in result.stdout
    calls, result = run("--all", "--build-only", status=1, MOCK_BUILD_FAIL="node-system")
    assert not activations(calls) and ["node", "failed", "built"] in summary_rows(result)
    # A single host keeps building the phase that evaluated.
    calls, result = run("node", "--build-only", status=1, MOCK_EVAL_FAIL="node-system")
    assert built(calls) == ["fixture-node-home"] and not activations(calls)
    calls, result = run("--build-only", "--dry-run", status=1)
    assert not actions(calls) and "--build-only and --dry-run cannot be combined" in result.stderr
    # A build or evaluation failure stops only its own target in a fleet, and
    # a single-host deploy activates nothing unless everything built.
    calls, result = run("--all", "--no-preflight", status=1, MOCK_BUILD_FAIL="node-system")
    assert rebuilt(calls) == ["solo"]
    rows = summary_rows(result)
    assert ["node", "failed", "ok"] in rows and ["solo", "ok", "-"] in rows
    assert "[system] [node] build failed" in result.stderr
    # The build keeps the terminal; the record takes the failed build's log
    # from Nix instead, and only for derivations that have one.
    [record] = list((root / ".local/state/nix-deploy/runs").iterdir())
    assert (record / "logs/build-fixture-node-system.log").read_text() == "fixture full build log of node-system\n"
    summary = (record / "logs/build.log").read_text()
    assert summary.startswith("command: nix build --keep-going ") and "exit status: 1\n" in summary
    assert "failed targets: system:node\n" in summary and "attempted, with no pending input: 1\n" in summary and "log of " + system_path("node") + ".drv: build-fixture-node-system.log" in summary
    assert [c for c in calls if c[:2] == ["nix", "log"]] == [["nix", "log", "--option", "substituters", "", system_path("node") + ".drv"]]
    calls, result = run("--all", "--no-preflight", status=1, MOCK_EVAL_FAIL="node-system")
    assert rebuilt(calls) == ["solo"] and ["node", "failed", "ok"] in summary_rows(result)
    assert "[system] [node]\terror: fixture evaluation failure in node-system" in result.stderr.splitlines()
    # The run leaves its evidence: the raw output of each step, the report
    # and the lock.
    state = root / ".local/state/nix-deploy"
    [record] = list((state / "runs").iterdir())
    assert f"deploy: run record: {record}" == result.stderr.splitlines()[-1]
    assert (record / "logs/system-node-evaluate.log").read_text() == "error: fixture evaluation failure in node-system\n"
    # Parallel fleet jobs are logged; solo, staged for boot and switched with
    # this terminal for its sudo prompt, keeps the terminal and has no log.
    assert (record / "logs/system-xyz-activate.log").exists() and (record / "logs/hm-xyz-activate.log").exists()
    assert not (record / "logs/system-solo-activate.log").exists() and (record / "logs/build.log").exists()
    info = json.loads((record / "run.json").read_text())
    assert info["failed"] and info["ended"] and info["args"] == ["--config", str(config), "--all", "--no-preflight"], info
    assert json.loads((record / "report.json").read_text())["failed"]
    assert (record / "flake.lock").read_text() == (root / "flake.lock").read_text()
    assert not (state / "last-good.json").exists()
    # With an agent configured, a failed run offers triage only on a
    # terminal; without one its output stays as it was, and so does its status.
    config.write_text(json.dumps({**HOST_DATA, "triage": {"agentCommand": ["fixture-agent"]}}))
    calls, offered = run("--all", "--no-preflight", status=1, MOCK_EVAL_FAIL="node-system")
    config.write_text(json.dumps(HOST_DATA))
    assert "nixfleet triage" not in offered.stdout + offered.stderr
    assert offered.stderr.splitlines()[-1].startswith("deploy: run record: ")
    # A lock the evaluation rewrote is the one deployed; the one the run
    # started with is kept beside it.
    started_lock = (root / "flake.lock").read_text()
    calls, result = run("--no-preflight", "--nixos", "node", MOCK_EVAL_LOCK='{"rewritten": true}\n')
    [record] = list((state / "runs").iterdir())
    info = json.loads((record / "run.json").read_text())
    assert (record / "flake.lock").read_text() == '{"rewritten": true}\n' and (record / "flake.start.lock").read_text() == started_lock
    assert info["startLockSha256"] != info["lockSha256"]
    # A single-host switch is logged through the buffer that already reads it.
    assert (record / "logs/system-node-activate.log").exists()
    (root / "flake.lock").write_text(started_lock)
    calls, result = run("--no-preflight", "node", status=1, MOCK_BUILD_FAIL="node-home")
    assert not activations(calls)
    assert "node did not build; nothing was activated" in result.stderr
    calls, result = run("--no-preflight", "node", status=1, MOCK_EVAL_FAIL="node-home")
    assert not activations(calls) and not built(calls)
    assert "node did not evaluate; nothing was activated" in result.stderr
    # Only changed targets are built: a host compares with what it already
    # runs, which this machine may no longer hold (or could only build slowly,
    # under emulation).
    calls, result = run("--no-preflight", "node", MOCK_CURRENT="node")
    assert not any(c[0] == "nix" and c[1] == "build" for c in calls) and not activations(calls)
    assert "node is up to date" in result.stdout
    calls, result = run("--all", "--no-preflight", MOCK_CURRENT="node")
    assert sorted(built(calls)) == ["fixture-solo-system", "fixture-xyz-home", "fixture-xyz-system"]
    assert rebuilt(calls) == ["solo"]
    calls, result = run("--all", "--no-preflight", "--force", MOCK_CURRENT="node")
    assert "fixture-node-system" in built(calls) and "node" in rebuilt(calls)
    # --json writes per-host results and timings.
    report = root / "report.json"
    calls, result = run("--all", "--no-preflight", "--json", str(report), MOCK_CURRENT="node")
    data = json.loads(report.read_text())
    assert data["version"] == version
    hosts = {h["host"]: h for h in data["hosts"]}
    assert hosts["node"]["system"]["status"] == "unchanged" and hosts["solo"]["system"]["status"] == "ok"
    assert hosts["xyz"]["homeManager"]["status"] == "ok" and hosts["solo"]["homeManager"]["status"] == "-"
    assert hosts["solo"]["system"]["evaluateSeconds"] > 0 and data["buildSeconds"] > 0 and not data["failed"]
    calls, _ = run("--all", "--json", status=1)
    calls, _ = run("--no-preflight", "--nixos", "node", "--json", str(report), status=7, MOCK_SWITCH_FAILURE="other")
    data = json.loads(report.read_text())
    assert data["failed"] and data["hosts"][0]["system"]["status"] == "failed"
    calls, _ = run("--no-preflight", "node", "--json", str(report))
    data = json.loads(report.read_text())
    assert not data["failed"] and [data["hosts"][0][phase]["status"] for phase in ["system", "homeManager"]] == ["ok", "ok"]
    calls, _ = run("--no-preflight", "node", "--dry-run", "--json", str(report), MOCK_CURRENT="node")
    data = json.loads(report.read_text())
    assert data["hosts"][0]["system"]["status"] == "unchanged"
    # Nothing to build is not counted.
    assert not counted(calls) and "buildProfile" not in data and "pendingBuilds" not in data

    # Build profiles: a run that would build many derivations here bounds the
    # shared build, and the roots built after it.
    def build_options(calls):
        """The options of the shared build and of each root, in call order."""
        return [[a for a in c[c.index("--out-link") + 2:] if not a.endswith(".drv^out")]
                for c in calls if c[:2] == ["nix", "build"] and "--dry-run" not in c]
    heavy = ["--option", "max-jobs", "6", "--option", "cores", "8"]
    calls, result = run("--all", "--no-preflight", "--json", str(report), MOCK_CURRENT="node", MOCK_LOCAL_BUILDS="50")
    assert sorted(counted(calls)) == ["fixture-solo-system", "fixture-xyz-home", "fixture-xyz-system"]
    assert calls.index(next(c for c in calls if c[:3] == ["nix", "build", "--dry-run"])) < calls.index(next(c for c in calls if "--keep-going" in c))
    assert "deploy: 50 derivations to build (heavy from 50); heavy build profile: max-jobs 6, cores 8" in result.stdout
    options = build_options(calls)
    assert len(options) == 4 and all(o == heavy for o in options), options
    data = json.loads(report.read_text())
    assert data["buildProfile"] == "heavy" and data["pendingBuilds"] == 50
    calls, result = run("--all", "--no-preflight", "--json", str(report), MOCK_CURRENT="node", MOCK_LOCAL_BUILDS="49")
    assert "light build profile: host defaults" in result.stdout and all(o == [] for o in build_options(calls))
    data = json.loads(report.read_text())
    assert data["buildProfile"] == "light" and data["pendingBuilds"] == 49
    calls, result = run("--no-preflight", "node", MOCK_LOCAL_BUILDS="1", DEPLOY_HEAVY_BUILDS="1", DEPLOY_HEAVY_MAX_JOBS="3", DEPLOY_HEAVY_CORES="4")
    assert "1 derivations to build (heavy from 1); heavy build profile: max-jobs 3, cores 4" in result.stdout
    assert all(o == ["--option", "max-jobs", "3", "--option", "cores", "4"] for o in build_options(calls))
    # A count that fails builds anyway, under the heavy profile.
    calls, result = run("--no-preflight", "node", MOCK_DRY_RUN_FAIL="1")
    assert "cannot count pending builds" in result.stderr and "fixture dry-run failure" in result.stderr
    assert all(o == heavy for o in build_options(calls)) and rebuilt(calls) == ["node"]
    calls, result = run("--no-preflight", "node", MOCK_DRY_RUN_RENAMED="1")
    assert "cannot count pending builds" in result.stderr and "derivations are queued" in result.stderr
    assert all(o == heavy for o in build_options(calls))
    # Heavy never loads this machine beyond its own settings.
    calls, result = run("--no-preflight", "node", "--heavy", MOCK_MAX_JOBS="2", MOCK_CORES="4")
    assert "heavy build profile (--heavy): max-jobs 2, cores 4" in result.stdout
    assert all(o == ["--option", "max-jobs", "2", "--option", "cores", "4"] for o in build_options(calls))
    calls, result = run("--no-preflight", "node", "--heavy", MOCK_MAX_JOBS="0", MOCK_CORES="4", MOCK_NIX_CONFIG="old")
    assert all(o == ["--option", "max-jobs", "0", "--option", "cores", "4"] for o in build_options(calls))
    # Settings that cannot be read are left to the host, never raised.
    calls, result = run("--no-preflight", "node", "--heavy", MOCK_NIX_CONFIG="none")
    assert "max-jobs unchanged (host setting unknown), cores unchanged (host setting unknown)" in result.stdout
    assert all(o == [] for o in build_options(calls))
    # An explicit profile skips the count; the option outranks the environment.
    calls, result = run("--no-preflight", "node", "--heavy", MOCK_LOCAL_BUILDS="1")
    assert not counted(calls) and all(o == heavy for o in build_options(calls))
    assert "deploy: heavy build profile (--heavy): max-jobs 6, cores 8" in result.stdout
    calls, result = run("--no-preflight", "node", "--light", MOCK_LOCAL_BUILDS="99", DEPLOY_BUILD_PROFILE="heavy")
    assert not counted(calls) and all(o == [] for o in build_options(calls))
    assert "deploy: light build profile (--light): host defaults" in result.stdout
    calls, result = run("--no-preflight", "node", MOCK_LOCAL_BUILDS="1", DEPLOY_BUILD_PROFILE="heavy")
    assert not counted(calls) and "heavy build profile (DEPLOY_BUILD_PROFILE)" in result.stdout
    # A dry run reports the profile without building.
    calls, result = run("--all", "--no-preflight", "--dry-run", "--json", str(report), MOCK_LOCAL_BUILDS="60")
    assert counted(calls) and not built(calls) and not activations(calls)
    assert "60 derivations to build (heavy from 50); heavy build profile" in result.stdout
    assert json.loads(report.read_text())["buildProfile"] == "heavy"
    for name, value in [("DEPLOY_BUILD_PROFILE", "fast"), ("DEPLOY_HEAVY_BUILDS", "0"), ("DEPLOY_HEAVY_MAX_JOBS", "x"), ("DEPLOY_HEAVY_CORES", "-1")]:
        calls, result = run("--no-preflight", "node", status=1, **{name: value})
        assert name in result.stderr and not actions(calls)
    # A run that cannot start, or stops early, still replaces the previous
    # report.
    calls, _ = run("--all", "--json", str(report), status=1, DEPLOY_EVAL_JOBS="0")
    assert json.loads(report.read_text())["version"] == version
    assert json.loads(report.read_text()) == {"version": version, "buildSeconds": 0, "totalSeconds": 0, "failed": True, "hosts": []}
    calls, _ = run("--all", "--no-preflight", "--json", str(report))
    calls, _ = run("--all", "--json", str(report), "--dryrun", status=1)
    assert json.loads(report.read_text())["failed"]
    calls, _ = run("--all", "--no-preflight", "--json", str(report))
    calls, _ = run("--all", "--json", str(report), "--json", status=1)
    assert json.loads(report.read_text())["failed"]
    calls, _ = run("--all", "--fail-unreachable", "--json", str(report), status=1, MOCK_UNREACHABLE="node.invalid")
    data = json.loads(report.read_text())
    assert data["failed"] and {h["host"] for h in data["hosts"]} == {"xyz", "node", "solo"}

    # Serial hosts leave the parallel batch. After a one-shot ready pre-check
    # of the whole group, they switch one at a time in serialSystemHosts
    # order, each followed by its ready gate.
    config.write_text(json.dumps(SERIAL_DATA))
    calls, result = run("--all", "--nixos", "--no-preflight")
    assert rebuilt(calls) == ["node", "solo", "k1", "k2", "k3"]
    sequence = [(c[0], c[1]) if c[0] == "ready-check" else (c[0], rebuilt([c])[0]) for c in calls if c[0] == "ready-check" or rebuilt([c]) in (["k1"], ["k2"], ["k3"])]
    assert sequence == [
        ("ready-check", "k1"), ("ready-check", "k2"), ("ready-check", "k3"),
        ("nixos-rebuild", "k1"), ("ready-check", "k1"),
        ("ready-check", "k1"), ("ready-check", "k3"),
        ("nixos-rebuild", "k2"), ("ready-check", "k2"),
        ("ready-check", "k1"), ("ready-check", "k2"),
        ("nixos-rebuild", "k3"), ("ready-check", "k3"),
    ], sequence
    assert [c for c in calls if c[0] == "ready-check"][0] == ["ready-check", "k1", "--node=k1"]
    assert all(row in summary_rows(result) for row in [["k1", "ok", "-"], ["k2", "ok", "-"], ["k3", "ok", "-"]])
    assert "ready gate passed after 1 attempt(s)" in result.stdout
    # A gate that never passes is retried every interval until the timeout,
    # then the rest of the serial group is skipped and the run fails.
    started = time.monotonic()
    calls, result = run("--all", "--nixos", "--no-preflight", status=1, MOCK_READY_FAIL_AFTER_SWITCH="k2")
    # Two pauses of DEPLOY_SERIAL_READY_INTERVAL (10s) separate the attempts.
    assert 20 <= time.monotonic() - started < 60
    assert rebuilt(calls) == ["node", "solo", "k1", "k2"]
    assert [c[1] for c in calls if c[0] == "ready-check"] == ["k1", "k2", "k3", "k1", "k1", "k3", "k2", "k2", "k2"]
    rows = summary_rows(result)
    assert ["k1", "ok", "-"] in rows and ["k2", "failed", "-"] in rows and ["k3", "skipped", "-"] in rows
    assert ["node", "ok", "-"] in rows and ["solo", "ok", "-"] in rows
    assert "ready gate did not pass within 25s (3 attempt(s))" in result.stderr
    assert "serial rollout stopped: k2 failed the serial ready gate (serialReadyCommand)" in result.stderr
    assert "skipped serial hosts: k3" in result.stderr
    assert "one or more fleet jobs failed" in result.stderr
    # A failed serial switch skips its gate and the remaining serial hosts.
    calls, result = run("--all", "--nixos", "--no-preflight", status=1, MOCK_FAIL_HOST="k1")
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
    # Only changed members switch and wait on the gate; the whole group is
    # still checked before the first switch.
    calls, result = run("--all", "--nixos", "--no-preflight", MOCK_CURRENT="node,solo,k1,k3")
    assert rebuilt(calls) == ["k2"]
    assert [c[1] for c in calls if c[0] == "ready-check"] == ["k1", "k2", "k3", "k2"]
    rows = summary_rows(result)
    assert ["k1", "unchanged", "-"] in rows and ["k2", "ok", "-"] in rows and ["node", "unchanged", "-"] in rows
    # A fleet with nothing changed activates nothing and runs no gates.
    calls, result = run("--all", "--nixos", "--no-preflight", MOCK_CURRENT="node,solo,k1,k2,k3")
    assert rebuilt(calls) == [] and not any(c[0] == "ready-check" for c in calls)
    assert all(row in summary_rows(result) for row in [["k1", "unchanged", "-"], ["node", "unchanged", "-"], ["solo", "unchanged", "-"]])
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
    # A member that switched but failed its gate is switched and gated again.
    calls, _ = run("--no-preflight", "--nixos", "k2", status=1, MOCK_READY_FAIL_AFTER_SWITCH="k2", DEPLOY_SERIAL_READY_INTERVAL="30")
    calls, result = run("--no-preflight", "--nixos", "k2", keep_state=True, MOCK_CURRENT="k2")
    assert rebuilt(calls) == ["k2"] and ["ready-check", "k2", "--node=k2"] in calls
    # An unchanged serial host is neither checked nor gated.
    calls, result = run("--no-preflight", "--nixos", "k2", MOCK_CURRENT="k2")
    assert rebuilt(calls) == [] and not any(c[0] == "ready-check" for c in calls)
    started = time.monotonic()
    calls, result = run("--no-preflight", "--nixos", "k2", status=1, MOCK_READY_FAIL_AFTER_SWITCH="k2", DEPLOY_SERIAL_READY_INTERVAL="30")
    # A pause as long as the remaining budget would leave no time to retry.
    assert [c[1] for c in calls if c[0] == "ready-check"] == ["k1", "k3", "k2"]
    assert time.monotonic() - started < 15
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
    assert "ready gate did not pass within 10s (1 attempt(s))" in result.stderr
    config.write_text(json.dumps(SERIAL_DATA))
    calls, _ = run("--no-preflight", "--nixos", "k2", status=1, DEPLOY_SERIAL_READY_INTERVAL="0")
    assert not actions(calls)
    # Without a ready command, serial hosts still switch one at a time.
    config.write_text(json.dumps({key: value for key, value in SERIAL_DATA.items() if key != "serialReadyCommand"}))
    calls, _ = run("--all", "--nixos", "--no-preflight")
    assert rebuilt(calls) == ["node", "solo", "k1", "k2", "k3"]
    assert not any(c[0] == "ready-check" for c in calls)

    # Serial groups are independent: a blocked quorum group leaves the DNS
    # group's rollout alone, and groups switch one after another.
    config.write_text(json.dumps(GROUP_DATA))
    calls, result = run("--all", "--nixos", "--no-preflight", status=1, MOCK_READY_FAIL="k2")
    assert rebuilt(calls) == ["node", "solo", "d1", "d2"]
    assert [c[1:] for c in calls if c[0] == "ready-check"] == [
        ["k1", "--node=k1"], ["k2", "--node=k2"],
        ["d1", "--dns"], ["d2", "--dns"], ["d1", "--dns"],
        ["d1", "--dns"], ["d2", "--dns"],
    ]
    rows = summary_rows(result)
    assert ["d1", "ok", "-"] in rows and ["d2", "ok", "-"] in rows and ["k1", "skipped", "-"] in rows
    assert "serial group blocked: k2 not ready before rollout" in result.stderr
    # An unreachable DNS server blocks only its own group.
    calls, result = run("--all", "--nixos", status=1, MOCK_UNREACHABLE="d2.invalid")
    assert rebuilt(calls) == ["node", "solo", "k1", "k2", "k3"]
    assert ["d1", "skipped", "-"] in summary_rows(result)
    assert "serial group blocked: d2 failed preflight" in result.stderr
    # A single-host deploy checks only its own group.
    calls, result = run("--no-preflight", "--nixos", "d1")
    assert [c[1:] for c in calls if c[0] == "ready-check"] == [["d2", "--dns"], ["d1", "--dns"]]
    config.write_text(json.dumps(HOST_DATA))

    def reject_inventory(data, expected):
        candidate = root / "invalid.json"
        candidate.write_text(data if isinstance(data, str) else json.dumps(data))
        log = root / "commands.jsonl"
        log.write_text("")
        env = {
            "PATH": str(commands),
            "HOME": str(root), "MOCK_LOG": str(log), "MOCK_STORE": str(store),
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
        env={"PATH": str(commands), "NIX_DEPLOY_CONFIG": str(config), "MOCK_LOG": str(root / "commands.jsonl"), "MOCK_STORE": str(store), "MOCK_HOST": "xyz", "MOCK_SYSTEM": "Linux"},
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
        {"serialGroups": [{"hosts": ["k1"]}]},
        {"serialGroups": [{"hosts": []}]},
        {"serialGroups": [{"hosts": ["node"], "command": "true"}]},
        {"serialGroups": {"hosts": ["node"]}},
        {"kubernetesNodes": ["missing"]},
        {"kubernetesNodes": ["node", "node"]},
        {"kubernetesNodes": "node"},
        {"kubernetesNodes": None},
        {"kubernetesContext": ""},
        {"kubernetesContext": "-x"},
        {"kubernetesContext": None},
        {"kubernetesCorePriorityClass": ""},
        {"kubernetesCorePriorityClass": "Core"},
        {"kubernetesCorePriorityClass": None},
        {"interactiveHosts": ["node"]},
        {"interactiveHosts": ["missing"], "rebootGuardCommand": "guard"},
        {"rebootGuardCommand": ""},
        {"bootCheck": "yes"},
    ]:
        reject_inventory({**SERIAL_DATA, **invalid}, "does not satisfy schemaVersion 1")

    # One program under three names (#548): deploy and k8s-node-power keep
    # their command lines, and nixfleet names a subcommand.
    names = root / "names"
    names.mkdir()
    for name in ["nixfleet", "deploy", "k8s-node-power"]:
        (names / name).symlink_to(deploy.resolve())

    def invoke(name, *args, status=0, **settings):
        log = root / "commands.jsonl"
        log.write_text("")
        env = {
            "PATH": str(commands), "HOME": str(root), "MOCK_LOG": str(log), "MOCK_STORE": str(store),
            "MOCK_HOST": "xyz", "MOCK_SYSTEM": "Linux", "NIX_SSHOPTS": "-o ConnectTimeout=1", "NO_COLOR": "1",
            **settings,
        }
        result = subprocess.run([str(names / name), *args], cwd=root, env=env, stdin=subprocess.DEVNULL, capture_output=True, text=True, timeout=HANG_GUARD)
        assert result.returncode == status, (name, args, result.returncode, result.stdout, result.stderr)
        return [json.loads(line) for line in log.read_text().splitlines()], result

    for name, args, expected in [
        ("nixfleet", ["--version"], "nixfleet"),
        ("deploy", ["--version"], "deploy"),
        ("k8s-node-power", ["--version"], "k8s-node-power"),
        ("nixfleet", ["deploy", "--version"], "deploy"),
    ]:
        _, result = invoke(name, *args)
        assert result.stdout == f"{expected} {version}\n", (name, args, result.stdout)
    _, result = invoke("nixfleet", "--help")
    for usage in ["nixfleet deploy", "nixfleet reboot [OPTIONS] [--all | <host>...]", "nixfleet off|on", "nixfleet status", "Subcommands:"]:
        assert usage in result.stdout, result.stdout
    _, result = invoke("nixfleet", status=2)
    assert result.stdout == "" and "Subcommands:" in result.stderr
    _, result = invoke("nixfleet", "bogus", status=2)
    assert "nixfleet: unknown subcommand 'bogus'" in result.stderr
    # deploy as a subcommand is the deploy command line.
    calls, result = invoke("nixfleet", "deploy", "--config", str(config), "--help", status=1)
    assert "Usage: deploy" in result.stdout and "Known hosts: xyz node solo mac" in result.stdout and not actions(calls)
    calls, result = invoke("nixfleet", "deploy", "--config", str(config), "--no-preflight", "--dry-run", "--nixos", "node")
    assert "[system] [node] changed:" in result.stdout and not activations(calls)
    # off and on are k8s-node-power's, named for the command typed; reboot is
    # the fleet reboot (ADR-0010), and k8s-node-power reboot stays one node.
    _, result = invoke("nixfleet", "off", "--help")
    assert result.stdout.startswith("Usage: nixfleet off [options] <host>\n       nixfleet on [options] <host>\n"), result.stdout
    assert "see nixfleet reboot --help" in result.stdout
    assert "inventory's kubernetesContext (NIX_DEPLOY_CONFIG),\n                           else the current context." in result.stdout, result.stdout
    _, result = invoke("k8s-node-power", "reboot", "--help")
    assert result.stdout.startswith("Usage: k8s-node-power reboot [options] <host>")
    assert "Defaults to the\n                           current context." in result.stdout and "kubernetesContext" not in result.stdout
    # nixfleet off and on take the inventory's context and stop when it
    # cannot be read, rather than use the current context.
    calls, result = invoke("nixfleet", "off", "node", status=1, NIX_DEPLOY_CONFIG=str(root / "missing.json"))
    assert "missing.json" in result.stderr and calls == [], result.stderr
    calls, result = invoke("nixfleet", "off", "--needed", "node", status=1)
    assert "--needed applies only to reboot" in result.stderr and calls == []
    calls, result = invoke("k8s-node-power", "reboot", "--needed", "node", status=1)
    assert "missing required command: kubectl" in result.stderr and calls == []

    # nixfleet reboot only reads before a confirmation. kubectl is mocked
    # from its own directory, so the status cases below still lack it.
    def reboots(calls):
        return [c for c in calls if c[0] == "ssh" and re.search(r"systemctl (stop|start|reboot)", c[-1])]

    _, result = invoke("nixfleet", "reboot", "--help")
    assert result.stdout.startswith("Usage: nixfleet reboot [--config FILE] [--all | <host>...]") and "kubernetesNodes" in result.stdout
    for args, message in [
        ([], "no host given; name hosts or pass --all"),
        (["--resume-maintenance", "node", "solo"], "--resume-maintenance needs exactly one host"),
        (["--node", "edge", "node"], "--node comes from the inventory"),
        (["nope"], "unknown host 'nope'"),
    ]:
        calls, result = invoke("nixfleet", "reboot", "--config", str(config), *args, status=2)
        assert message in result.stderr and not reboots(calls), (args, result.stderr)
    kube = root / "kube"
    kube.mkdir()
    (kube / "kubectl").write_text("#!" + sys.executable + "\n" + MOCK)
    (kube / "kubectl").chmod(0o755)
    fleet = root / "fleet.json"
    fleet.write_text(json.dumps({**HOST_DATA, "kubernetesNodes": ["node"], "kubernetesCorePriorityClass": "core"}))
    cluster = {"PATH": f"{kube}:{commands}", "MOCK_NODES": "node"}
    calls, result = invoke("nixfleet", "reboot", "--config", str(fleet), "--all", "--dry-run", **cluster)
    assert "group node:\n  1. node: Kubernetes node with 0 database primaries and core singletons platform/forge, runners forgejo-actions-runner.service\n" in result.stdout, result.stdout
    assert "group solo:\n  1. solo: plain host, runners forgejo-actions-runner.service\n" in result.stdout, result.stdout
    assert "not selected: xyz: the operator host and this machine" in result.stdout and "dry run: nothing was changed" in result.stdout
    assert not reboots(calls) and all(c[0] in {"hostname", "ssh", "kubectl"} for c in calls), calls
    assert ["kubectl", "get", "pods", "-A", "-o", "json", "--request-timeout=10s"] in calls
    assert not [c for c in calls if c[:2] == ["kubectl", "taint"]], calls
    calls, result = invoke("nixfleet", "reboot", "--config", str(fleet), "solo", status=2, **cluster)
    assert "pass --yes" in result.stderr and not reboots(calls), (result.stdout, result.stderr, calls)
    # This machine reboots only from a terminal: the flag without one is
    # refused before anything is read.
    calls, result = invoke("nixfleet", "reboot", "--config", str(fleet), "--all", "--include-self", "--yes", status=2, **cluster)
    assert "--include-self needs a terminal" in result.stderr and all(c[0] == "hostname" for c in calls), (result.stderr, calls)
    # Without kubernetesNodes the cluster's node is refused, and without the
    # cluster no host can be told apart from a node.
    calls, result = invoke("nixfleet", "reboot", "--config", str(config), "edge", "--dry-run", **cluster)
    assert "node: fail: the cluster lists it as node node, but kubernetesNodes does not" in result.stdout, result.stdout
    calls, result = invoke("nixfleet", "reboot", "--config", str(config), "solo", "--dry-run")
    assert "whether it is a Kubernetes node cannot be told: kubectl is not available" in result.stdout, result.stdout
    # status only reads: one boot check per host over SSH. kubectl is not on
    # the search path, so the cluster is reported, not an error.
    report = root / "status.json"
    calls, result = invoke("nixfleet", "status", "--config", str(config), "edge", "solo", "mac", "--json", str(report), MOCK_STATUS_UNREACHABLE="solo")
    rows = [line.split() for line in result.stdout.splitlines()]
    assert rows[0] == ["host", "ssh", "system", "reboot", "node", "runners", "boot"], rows
    assert rows[1] == ["node", "ok", "switched", "yes", "-", "1/1", "-"], rows
    assert rows[2] == ["solo", "unreachable", "-", "-", "-", "-", "-"], rows
    assert rows[3] == ["mac", "ok", "darwin", "-", "-", "-", "-"], rows
    assert "node: reboot needed: kernel differs" in result.stdout and "solo: unreachable: fixture endpoint unavailable" in result.stdout
    assert "cluster: not read: kubectl is not available" in result.stdout
    assert sorted(c[-2] for c in calls if c[0] == "ssh") == ["operator@solo.invalid", "root@mac", "root@node.invalid"]
    assert all(c[0] in {"hostname", "ssh"} for c in calls) and all("BatchMode=yes" in c for c in calls if c[0] == "ssh"), calls
    data = json.loads(report.read_text())
    assert data["version"] == version and data["cluster"] == {"reachable": False, "error": "kubectl is not available", "nodes": 0}
    assert [(h["host"], h["reachable"], h["rebootNeeded"]) for h in data["hosts"]] == [("node", True, True), ("solo", False, None), ("mac", True, None)]
    assert data["hosts"][0]["rebootDiffers"] == ["kernel"] and data["hosts"][0]["runners"]["active"] == 1
    _, result = invoke("nixfleet", "status", "--config", str(config), "--all", "node", status=2)
    assert "--all and host names cannot be combined" in result.stderr
    _, result = invoke("nixfleet", "status", "--config", str(config), "nope", status=2)
    assert "unknown host 'nope'" in result.stderr

print("Deployment contract: 122 mocked CLI cases and 38 invalid inventories passed, and 27 nixfleet dispatch, reboot and status cases; no deployment commands executed")
