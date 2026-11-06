"""Exercise the packaged daemon against synthetic Hyprland 0.57 IPC."""

import json
import os
from pathlib import Path
import socket
import subprocess
import sys
import tempfile
import threading
import time


def workspace(address, display_name=None):
    return {"address": address, "type": "special" if address.startswith("special:") else "normal",
            "name": display_name or "renamed-" + address}


def client(name, ws, display_name=None):
    return {
        "address": "0x1234" if name == "dropterm" else "0x5678",
        "at": [0, 0], "size": [800, 600], "workspace": workspace(ws, display_name),
        "floating": True, "fullscreen": 0, "fullscreenClient": 0,
        "monitor": 0, "initialClass": name, "class": name,
        "initialTitle": name, "title": name, "pid": 1, "xwayland": False,
        "pinned": False, "grouped": [], "mapped": True,
        "swallowing": None, "focusHistoryID": 0,
    }


def wait_for(predicate):
    deadline = time.monotonic() + 5
    while not predicate():
        if time.monotonic() > deadline:
            raise AssertionError("IPC operation did not complete")
        time.sleep(0.02)


def main(binary):
    with tempfile.TemporaryDirectory(prefix="hyprscratch-ipc-") as temporary:
        root = Path(temporary)
        sockets = root / "hypr" / "compat"
        sockets.mkdir(parents=True)
        stub = root / "hyprctl"
        stub.write_text("#!/bin/sh\nprintf 'configProvider: lua\\n'\n")
        stub.chmod(0o755)
        config = root / "scratch.conf"
        config.write_text('daemon_options = clean\ndropterm {\n title = dropterm\n command = foot --app-id dropterm\n options = persist\n}\n')
        daemon_socket = str(root / "scratch.sock")
        state = {"active": "alpha", "clients": [], "window": {}}
        dispatches = []
        errors = []
        streams = []
        stop = threading.Event()

        def serve(path, events=False):
            with socket.socket(socket.AF_UNIX) as server:
                server.bind(str(path))
                server.listen()
                server.settimeout(0.1)
                while not stop.is_set():
                    try:
                        connection, _ = server.accept()
                    except TimeoutError:
                        continue
                    if events:
                        streams.append(connection)
                        continue
                    with connection:
                        try:
                            request = connection.recv(65536).decode().removeprefix("j/")
                            monitor = {
                                "id": 0, "name": "TEST-1", "description": "Test display",
                                "width": 1920, "height": 1080, "refreshRate": 60,
                                "x": 0, "y": 0, "activeWorkspace": workspace(state["active"], state.get("label")),
                                "specialWorkspace": {"address": "", "type": "", "name": ""},
                                "reserved": [0, 0, 0, 0], "scale": 1, "transform": 0,
                                "focused": True, "dpmsStatus": True, "vrr": False, "disabled": False,
                            }
                            active = dict(workspace(state["active"], state.get("label")), monitor="TEST-1", monitorID=0,
                                          windows=1, hasfullscreen=False, lastwindow="0x1234", lastwindowtitle="test")
                            data = {"monitors all": [monitor], "activeworkspace": active,
                                    "clients": state["clients"], "activewindow": state["window"]}
                            if request in data:
                                response = json.dumps(data[request])
                            else:
                                dispatches.append(request)
                                response = "ok"
                            connection.sendall(response.encode())
                        except Exception as error:
                            errors.append(str(error))
                            connection.sendall(b"error")

        threads = []
        for name, events in ((".socket.sock", False), (".socket2.sock", True)):
            thread = threading.Thread(target=serve, args=(sockets / name, events), daemon=True)
            thread.start()
            threads.append(thread)
        wait_for(lambda: all((sockets / name).exists() for name in (".socket.sock", ".socket2.sock")))
        environment = dict(os.environ, XDG_RUNTIME_DIR=str(root), HYPRLAND_INSTANCE_SIGNATURE="compat",
                           PATH=str(root) + os.pathsep + os.environ["PATH"])
        with (root / "daemon.log").open("w+") as log:
            daemon = subprocess.Popen([binary, "init", "config", str(config), "socket", daemon_socket],
                                      env=environment, stdout=log, stderr=log)
            try:
                wait_for(lambda: Path(daemon_socket).exists() and streams)

                def toggle():
                    dispatches.clear()
                    subprocess.run([binary, "toggle", "dropterm", "socket", daemon_socket],
                                   env=environment, check=True, timeout=5, capture_output=True)
                    try:
                        wait_for(lambda: any("bring_to_top" in item for item in dispatches))
                    except AssertionError:
                        log.seek(0)
                        print(log.read(), dispatches, errors)
                        raise
                    assert not errors, errors

                toggle()
                assert any("exec_cmd" in item for item in dispatches), dispatches

                # Distinct named workspaces both lack an ID. Summon rather than
                # mistakenly refocusing the terminal on its old workspace.
                state["clients"] = [client("dropterm", "beta")]
                state["window"] = client("editor", "alpha")
                toggle()
                assert any('workspace="alpha"' in item for item in dispatches), dispatches
                assert all("name:alpha" not in item for item in dispatches), dispatches

                # A renamed numbered workspace keeps address "1". Display
                # names are neither move destinations nor workspace identity.
                state["active"] = "1"
                state["label"] = "Web"
                state["clients"] = [client("dropterm", "beta", "Web")]
                state["window"] = client("editor", "1", "Web")
                toggle()
                assert any('workspace="1"' in item for item in dispatches), dispatches
                assert all('workspace="Web"' not in item for item in dispatches), dispatches

                state["active"] = "alpha"
                state.pop("label")
                state["clients"] = [client("dropterm", "alpha")]
                state["window"] = state["clients"][0]
                toggle()
                assert any('workspace="special:dropterm"' in item for item in dispatches), dispatches

                # V2 events carry an addressable name where an integer once was.
                # The clean callback must remain alive and hide the terminal.
                state["active"] = "beta"
                state["window"] = {}
                dispatches.clear()
                streams[0].sendall(b"createworkspacev2>>beta,renamed-beta\nworkspacev2>>beta,renamed-beta\n")
                wait_for(lambda: any('workspace="special:dropterm"' in item for item in dispatches))
                assert daemon.poll() is None
                print("Hyprland 0.57 IPC: spawn, summon, hide and workspace-change cleanup passed")
            finally:
                daemon.terminate()
                try:
                    daemon.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    daemon.kill()
                    daemon.wait()
                stop.set()
                for stream in streams:
                    stream.close()
                for thread in threads:
                    thread.join(timeout=2)
                log.seek(0)
                output = log.read()
                assert "invalid integer" not in output and "missing field" not in output, output


if __name__ == "__main__":
    main(sys.argv[1])
