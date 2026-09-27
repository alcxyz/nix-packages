# K3s Flannel node source patch

This package function patches the K3s release selected by its caller. Apply it
to the consumer's `pkgs.k3s` with `pkgs.callPackage` so the patch does not pull a
different K3s version from this repository's own nixpkgs lock:

```nix
pkgs.callPackage (inputs.nix-packages + "/pkgs/k3s-flannel-node-source") { }
```

When K3s has an explicit Flannel interface, Flannel uses K3s's resolved IPv4
node address as its interface and advertised address. It checks that the
selected interface actually owns the address and fails if it does not. The
default-gateway path remains unchanged.

The patch is tied to K3s source layout. On each K3s update, apply and build it
against the consumer's pin, run its Flannel tests, and verify the VXLAN local
address and node annotation. Remove this patch when upstream reliably selects
the configured node address.
