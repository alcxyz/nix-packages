{
  lib,
  buildGoModule,
  makeWrapper,
  openssh,
}:

buildGoModule {
  pname = "forgejo-deploy-key";
  version = "0.1.0";

  # The module replaces sopsstore with ../../lib/sopsstore, so the source
  # is the repository root, built from this module's directory.
  src = lib.fileset.toSource {
    root = ../..;
    fileset = lib.fileset.unions [
      ./.
      ../../lib/sopsstore
    ];
  };
  modRoot = "tools/forgejo-deploy-key";

  vendorHash = null;
  # With vendorHash = null, buildGoModule forces -mod=vendor unless proxyVendor
  # is set, and there is no vendor directory to satisfy the sopsstore replace.
  proxyVendor = true;

  nativeCheckInputs = [ openssh ];

  # The go command writes its telemetry under $HOME/.config; keep it in the
  # build directory, as gh-app does.
  preConfigure = ''
    export HOME="$TMPDIR"
  '';

  nativeBuildInputs = [ makeWrapper ];

  # Suffixed, so the user's own ssh and ssh -G configuration are found first.
  # sops is not wrapped; it comes from the user's PATH, as for gh-app.
  postInstall = ''
    wrapProgram $out/bin/forgejo-deploy-key \
      --suffix PATH : ${lib.makeBinPath [ openssh ]}
  '';

  meta = {
    description = "Create a Forgejo write deploy key and store its private half straight into a new SOPS file";
    mainProgram = "forgejo-deploy-key";
  };
}
