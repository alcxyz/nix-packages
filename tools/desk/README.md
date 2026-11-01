# desk

Starts and ends a remote session on a host in one step, from the machine
you sit at. The host runs a VNC server on a Unix socket only its user can
reach (or on its loopback) and a `remote-desk on|off|status|socket` command
that moves its desktop to a virtual output for the viewer and back and names
the socket; `desk` only orchestrates them:

1. opens an SSH tunnel from `127.0.0.1:5900` to the host's VNC server: to
   the Unix socket that `remote-desk socket` prints, or to the host's
   loopback port 5900 (`-remote-port`) when `remote-desk` has no `socket`
   command;
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

`desk HOST` tries `HOST-lan`, then `HOST-wan`, then `HOST`, skipping the
first two unless the SSH configuration defines them (`ssh -G` gives them a
host name of their own). A name that does not answer within `-lan-timeout`
(2 seconds) passes to the next. The usual setup is the host's LAN address as
`HOST-lan`, a public address that the router forwards to its SSH port as
`HOST-wan`, and an overlay network such as NetBird as `HOST`. Every name but
the last is tried with strict host key checking, because a LAN address on
another network may belong to a different machine; give those names the
host's key alias (`HostKeyAlias`) so they share the known host key. `-via
a,b` replaces the list.

The tunnel is a backgrounded ssh control master that only holds the
forward. `remote-desk on` and `off` run on connections of their own, with
connection sharing off. `on` runs once, through the route the tunnel took.
`off` (and `desk off`, `desk status`) starts there and moves on to the next
route, each attempt with its own deadline, when ssh fails to connect or
stalls; a failure of `remote-desk` itself is not repeated. If `on` was
interrupted, it may still finish on the host after `off`: `desk` then says
to run `desk off HOST` should the displays stay off.

On the host, a socket in a directory only the user can open lets only that
user's processes, and the SSH tunnel, reach the server, so the server can
skip its own authentication. The tunnel's local end is still a TCP port on
`127.0.0.1` for the length of the session, which any local process on the
client can reach; without VNC authentication that suits a single-user
client. With a TCP port and RSA-AES, TigerVNC pins the server's key per
address and asks once to accept it for `127.0.0.1`.

## Credentials

`desk` uses ssh as it is configured: its keys, agent and known hosts. It
never sees a VNC password; the viewer asks for one if the server wants it.

## Viewer

`-viewer PATH`, else `vncviewer` on `PATH`, else TigerVNC's macOS app
(`/Applications/TigerVNC.app` or `~/Applications/TigerVNC.app`).

## Not yet

- Reconnecting a dropped link without turning the desk off.
