{
  lib,
  stdenv,
  fetchurl,
  unzip,
}:

stdenv.mkDerivation rec {
  pname = "omniwm";
  version = "0.5.2.1";

  src = fetchurl {
    url = "https://github.com/BarutSRB/OmniWM/releases/download/v${version}/OmniWM-v${version}.zip";
    hash = "sha256-V0Zj6P94iAou3rYpA+CCz1Vq8Ko3cETuzFtveGD4idc=";
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
