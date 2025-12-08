{ lib, buildGoModule }:

buildGoModule {
  pname = "zfs-auto-unlock";
  version = "0.1.0";

  src = ./.;

  # temporary; you'll fill this with the real hash from `nix build`
  #vendorHash = lib.fakeHash;
  vendorHash = null;
}
