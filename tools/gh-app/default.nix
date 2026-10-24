{
  lib,
  buildGoModule,
  git,
}:

buildGoModule {
  pname = "gh-app";
  version = "0.2.0";

  # The module replaces sopsstore with ../../lib/sopsstore, so the source
  # is the repository root, built from this module's directory.
  src = lib.fileset.toSource {
    root = ../..;
    fileset = lib.fileset.unions [
      ./.
      ../../lib/sopsstore
    ];
  };
  modRoot = "tools/gh-app";

  vendorHash = null;
  # With vendorHash = null, buildGoModule forces -mod=vendor unless proxyVendor
  # is set, and there is no vendor directory to satisfy the sopsstore replace.
  proxyVendor = true;

  nativeCheckInputs = [ git ];

  # The go command writes its telemetry under $HOME/.config. Without a sandbox
  # that creates /homeless-shelter, and Nix then refuses to start the next
  # derivation (gh-app-gh) in the same build.
  preConfigure = ''
    export HOME="$TMPDIR"
  '';

  meta = {
    description = "Route gh and Git through a GitHub App's installation token for its installed repositories";
    mainProgram = "gh-app";
  };
}
