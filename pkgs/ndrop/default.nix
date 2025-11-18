{ lib, stdenvNoCC, fetchFromGitHub, scdoc }:

stdenvNoCC.mkDerivation rec {
  pname = "ndrop";
  version = "0feb899-unstable";

  src = fetchFromGitHub {
    owner = "schweber";
    repo = "ndrop";
    rev = "0feb899f34609e4afc0ec166de4f309e2b9c9f02";
    hash = "sha256-hh0mrLsp0qj1IqBERqV9fS/KCsTi++seFNmy2Ej9Vpg=";
  };

  nativeBuildInputs = [ scdoc ];

  # Only unpack + install, no build
  phases = [ "unpackPhase" "installPhase" ];

  installPhase = ''
    runHook preInstall

    mkdir -p $out/bin
    install -m755 ndrop $out/bin/ndrop

    if [ -f ndrop.1.scd ]; then
      mkdir -p $out/share/man/man1
      scdoc < ndrop.1.scd > $out/share/man/man1/ndrop.1
    fi

    runHook postInstall
  '';

  meta = with lib; {
    description = "Scratchpad-like toggle helper for Wayland compositors";
    homepage = "https://github.com/schweber/ndrop";
    license = licenses.mit;
    platforms = platforms.linux;
    mainProgram = "ndrop";
  };
}
