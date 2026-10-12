{
  bash,
  buildGoModule,
  coreutils,
  hostname,
  lib,
  makeWrapper,
  openssh,
  python3,
}:
buildGoModule {
  pname = "nix-deploy";
  version = "0.4.0";

  src = ./.;

  vendorHash = null; # no external dependencies

  nativeBuildInputs = [ makeWrapper ];
  nativeCheckInputs = [
    bash
    python3
  ];

  # The black-box contract mocks every external command, so it runs against
  # the unwrapped binary before the runtime PATH is added (ADR-0006).
  postCheck = ''
    python3 ${./test-deploy.py} "$GOPATH/bin/nix-deploy"
  '';

  postInstall = ''
    mv $out/bin/nix-deploy $out/bin/deploy
    wrapProgram $out/bin/deploy \
      --prefix PATH : ${
        lib.makeBinPath [
          bash
          coreutils
          hostname
          openssh
        ]
      }
  '';

  meta = with lib; {
    description = "Inventory-driven NixOS/darwin and Home Manager deploy tool";
    mainProgram = "deploy";
    platforms = platforms.unix;
  };
}
