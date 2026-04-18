{
  lib,
  stdenv,
  fetchurl,
  appimageTools,
  undmg,
  system ? stdenv.hostPlatform.system,
}:

let
  pname = "t3code";
  version = "0.0.20";

  linuxSrc = fetchurl {
    url = "https://github.com/pingdotgg/t3code/releases/download/v${version}/T3-Code-${version}-x86_64.AppImage";
    hash = "sha256-glYnF8UA5s4rrpUJuvk4HlQtyMikbckIkmMIhnJugO4=";
  };

  darwinSrc = fetchurl {
    url = "https://github.com/pingdotgg/t3code/releases/download/v${version}/T3-Code-${version}-arm64.dmg";
    hash = "sha256-bT7cQrkxbITiEjh+tGtnC9VwN3FSHfQr+1aP0pzhTto=";
  };

  appimageContents = appimageTools.extractType2 { inherit pname version; src = linuxSrc; };
in
if lib.hasPrefix "x86_64-linux" system then
  appimageTools.wrapType2 {
    inherit pname version;
    src = linuxSrc;

    extraInstallCommands = ''
      desktop="$(find ${appimageContents} -maxdepth 5 -name '*.desktop' | head -n1)"
      if [ -n "$desktop" ]; then
        install -Dm444 "$desktop" "$out/share/applications/${pname}.desktop"
        substituteInPlace "$out/share/applications/${pname}.desktop" \
          --replace-warn 'Exec=AppRun' 'Exec=${pname}'
      fi

      icon="$(find ${appimageContents} -path '*/hicolor/*/apps/*.png' | head -n1)"
      if [ -n "$icon" ]; then
        size="$(echo "$icon" | grep -Eo '/[0-9]+x[0-9]+/' | tr -d /)"
        install -Dm444 "$icon" \
          "$out/share/icons/hicolor/$size/apps/${pname}.png"
      fi
    '';

    meta = with lib; {
      description = "T3 Code — AI coding assistant desktop app";
      homepage = "https://github.com/pingdotgg/t3code";
      license = licenses.mit;
      platforms = [ "x86_64-linux" ];
      mainProgram = "t3code";
    };
  }

else if lib.hasPrefix "aarch64-darwin" system then
  stdenv.mkDerivation {
    inherit pname version;
    src = darwinSrc;

    nativeBuildInputs = [ undmg ];

    unpackPhase = ''
      undmg $src
    '';

    installPhase = ''
      mkdir -p "$out/Applications" "$out/bin"
      cp -r *.app "$out/Applications/"
      ln -s "$out/Applications/T3 Code.app/Contents/MacOS/T3 Code" "$out/bin/t3code"
    '';

    meta = with lib; {
      description = "T3 Code — AI coding assistant desktop app";
      homepage = "https://github.com/pingdotgg/t3code";
      license = licenses.mit;
      platforms = [ "aarch64-darwin" ];
      mainProgram = "t3code";
    };
  }

else
  throw "t3code: unsupported system ${system}"
