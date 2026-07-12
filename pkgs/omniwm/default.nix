{
  lib,
  stdenv,
  fetchurl,
  unzip,
}:

stdenv.mkDerivation rec {
  pname = "omniwm";
  version = "0.5.5";

  src = fetchurl {
    url = "https://github.com/BarutSRB/OmniWM/releases/download/v${version}/OmniWM-v${version}.zip";
    hash = "sha256-9JEvkAV7KRPgejx88ycdzDE+maLNFh5PDz0Gp4LaZjI=";
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
