{ lib, buildGoModule }:

buildGoModule {
  pname = "forge-mirror";
  version = "0.1.0";

  src = ./.;

  vendorHash = null;

  meta = {
    description = "Manage GitHub→Forgejo mirrors and dual-push configuration";
    mainProgram = "forge-mirror";
  };
}
