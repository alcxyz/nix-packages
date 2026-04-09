{ lib, stdenv }:

stdenv.mkDerivation {
  pname = "leantime-tidy";
  version = "0.1.0";

  src = ./.;

  dontBuild = true;

  installPhase = ''
    install -Dm755 leantime-tidy $out/bin/leantime-tidy
  '';

  meta = with lib; {
    description = "Leantime ticket cleanup tool with AI-powered suggestions and web review UI";
    mainProgram = "leantime-tidy";
    platforms = [ "x86_64-linux" ];
  };
}
