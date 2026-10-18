{ buildGoModule, git }:

buildGoModule {
  pname = "gh-app";
  version = "0.1.0";

  src = ./.;

  vendorHash = null;

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
