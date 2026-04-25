{ lib, buildGoModule }:

buildGoModule {
  pname = "devlog";
  version = "0.1.0";

  src = ./.;

  vendorHash = null;

  meta = {
    description = "Daily and weekly devlog generator from GitHub activity";
    mainProgram = "devlog";
  };
}
