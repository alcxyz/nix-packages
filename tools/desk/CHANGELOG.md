# desk changelog

`desk -version` names the running release. Bump `version` in `default.nix`
with every change to the tool and add an entry here:

- major: an incompatible change to the command line;
- minor: a new command or option, or a change in what a session does;
- patch: a fix that keeps the intended behaviour.

## 0.1.0 (2026-10-31)

- First release: `desk HOST` opens an SSH tunnel to the host's loopback VNC
  server (through `HOST-lan` first when the SSH configuration defines it),
  runs `remote-desk on`, starts the VNC viewer and, when the viewer exits or
  on Ctrl-C, runs `remote-desk off` and closes the tunnel. `desk off HOST`
  and `desk status HOST` run those actions alone.
