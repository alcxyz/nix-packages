{ lib, buildGoModule, git }:

buildGoModule {
  pname = "devlog";
  version = "0.1.0";

  src = ./.;

  vendorHash = null;

  # The journal sync tests drive real repositories.
  nativeCheckInputs = [ git ];

  meta = {
    description = "Daily and weekly devlog generator from GitHub activity";
    mainProgram = "devlog";
  };
}
