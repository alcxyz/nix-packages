{ lib, buildGoModule, git }:

buildGoModule {
  pname = "forge-mirror";
  version = "0.1.0";

  src = ./.;

  vendorHash = null;

  nativeCheckInputs = [ git ];

  meta = {
    description = "Manage Forgejo-first remotes, GitHub mirrors, and drift auditing";
    mainProgram = "forge-mirror";
  };
}
