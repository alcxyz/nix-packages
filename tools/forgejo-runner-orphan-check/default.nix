{ lib, buildGoModule }:

buildGoModule {
  pname = "forgejo-runner-orphan-check";
  version = "0.1.0";

  src = ./.;

  vendorHash = null;

  meta = {
    description = "Detect Forgejo Actions containers whose tasks are terminal";
    mainProgram = "forgejo-runner-orphan-check";
    platforms = lib.platforms.linux;
  };
}
