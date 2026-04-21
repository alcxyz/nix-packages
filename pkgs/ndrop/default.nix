{ lib, stdenvNoCC, fetchFromGitHub, scdoc }:

stdenvNoCC.mkDerivation rec {
  pname = "ndrop";
  version = "f2fb1c6-unstable";

  src = fetchFromGitHub {
    owner = "schweber";
    repo = "ndrop";
    rev = "f2fb1c611811c48b48cd0f0fecab4f3f935e7405";
    hash = "sha256-/Xco1sr76+F3mAIGq29yp5Y6FPcXS/AVXDpwZ1+rLQk=";
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
