# desk

Starts and ends a remote session on a host in one step, from the machine
you sit at. The host runs a VNC server on its loopback and a `remote-desk
on|off|status` command that moves its desktop to a virtual output for the
viewer and back; `desk` only orchestrates them:

1. opens an SSH tunnel from `127.0.0.1:5900` to the host's loopback VNC
   server;
2. runs `remote-desk on` on the host;
3. starts the VNC viewer on `127.0.0.1::5900` and waits for it to exit
   (IPv4 like the forward, so nothing listening on `[::1]` can answer
   instead);
4. runs `remote-desk off` and closes the tunnel, also after Ctrl-C or when
   the terminal closes.

```sh
desk xyz                  # a session; close the viewer to end it
desk xyz -- -FullScreen   # viewer options go after --
desk off xyz              # remote-desk off alone, after a session that ended without it
desk status xyz
```

`remote-desk off` may restart the VNC server, so a viewer that is still
connected drops at the end; that is expected.

## Routes

`desk HOST` tries `HOST-lan` first when the SSH configuration defines that
name (`ssh -G` gives it a host name of its own), and `HOST` when it does not
answer within `-lan-timeout` (2 seconds). Every name but the last is tried
with strict host key checking, because a LAN address on another network may
belong to a different machine; give the LAN name the host's key alias
(`HostKeyAlias`) so it shares the known host key. `-via a,b` replaces the
list.

The tunnel is a backgrounded ssh control master that only holds the
forward. `remote-desk on` and `off` run on connections of their own, with
connection sharing off. `on` runs once, through the route the tunnel took.
`off` (and `desk off`, `desk status`) starts there and moves on to the next
route, each attempt with its own deadline, when ssh fails to connect or
stalls; a failure of `remote-desk` itself is not repeated. If `on` was
interrupted, it may still finish on the host after `off`: `desk` then says
to run `desk off HOST` should the displays stay off.

TigerVNC pins the server's RSA-AES key per address; the first session
through `desk` asks once to accept it for `127.0.0.1`.

## Credentials

`desk` uses ssh as it is configured: its keys, agent and known hosts. It
never sees the VNC password, which the viewer asks for.

## Viewer

`-viewer PATH`, else `vncviewer` on `PATH`, else TigerVNC's macOS app
(`/Applications/TigerVNC.app` or `~/Applications/TigerVNC.app`).

## Not yet

- Reconnecting a dropped link without turning the desk off.
