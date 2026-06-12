{
  lib,
  stdenv,
  fetchurl,
  unzip,
}:

stdenv.mkDerivation rec {
  pname = "omniwm";
  version = "0.4.9.7";

  src = fetchurl {
    url = "https://github.com/BarutSRB/OmniWM/releases/download/v${version}/OmniWM-v${version}.zip";
    hash = "sha256-O2Tl8uMCug9A3Dlfh56nMe1iQlP/562EGurm8dKC1Fk=";
  };

  nativeBuildInputs = [ unzip ];

  unpackPhase = ''
    unzip $src
  '';

  installPhase = ''
    mkdir -p "$out/Applications"
    cp -r OmniWM.app "$out/Applications/"
  '';

  meta = with lib; {
    description = "Niri-style scrolling tiling window manager for macOS";
    homepage = "https://github.com/BarutSRB/OmniWM";
    license = licenses.unfree;
    platforms = [ "aarch64-darwin" ];
  };
}
