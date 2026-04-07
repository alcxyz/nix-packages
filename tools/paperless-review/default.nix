{ lib, stdenv }:

stdenv.mkDerivation {
  pname = "paperless-review";
  version = "0.1.0";

  src = ./.;

  dontBuild = true;

  installPhase = ''
    install -Dm755 paperless-review $out/bin/paperless-review
  '';

  meta = with lib; {
    description = "AI review tool for Paperless-NGX inbox documents";
    mainProgram = "paperless-review";
    platforms = [ "x86_64-linux" ];
  };
}
