{
  lib,
  buildGoModule,
  writableTmpDirAsHomeHook,
}:

buildGoModule {
  pname = "zfs-auto-unlock";
  version = "0.1.0";

  src = ./.;

  # temporary; you'll fill this with the real hash from `nix build`
  #vendorHash = lib.fakeHash;
  vendorHash = null;

  # The go command writes telemetry under $HOME, which unsandboxed CI builds
  # would create as /homeless-shelter (#603).
  nativeBuildInputs = [ writableTmpDirAsHomeHook ];
}
