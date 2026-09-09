{
  bash,
  coreutils,
  hostname,
  jq,
  lib,
  makeWrapper,
  gnugrep,
  gnused,
  openssh,
  parallel,
  stdenv,
}:
stdenv.mkDerivation {
  pname = "nix-deploy";
  version = "0.2.0";

  src = ./.;

  dontBuild = true;

  nativeBuildInputs = [ makeWrapper ];

  installPhase = ''
    install -Dm755 deploy $out/bin/deploy
    wrapProgram $out/bin/deploy \
      --prefix PATH : ${
        lib.makeBinPath [
          bash
          coreutils
          hostname
          jq
          gnugrep
          gnused
          openssh
          parallel
        ]
      }
  '';

  meta = with lib; {
    description = "Inventory-driven NixOS/darwin and Home Manager deploy tool";
    mainProgram = "deploy";
    platforms = platforms.unix;
  };
}
