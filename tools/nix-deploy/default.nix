{ lib, stdenv }:

stdenv.mkDerivation {
  pname = "nix-deploy";
  version = "0.1.0";

  src = ./.;

  dontBuild = true;

  installPhase = ''
    install -Dm755 deploy $out/bin/deploy
  '';

  meta = with lib; {
    description = "Unified NixOS/darwin + home-manager deploy tool with parallel multi-host support";
    mainProgram = "deploy";
    platforms = platforms.unix;
  };
}
