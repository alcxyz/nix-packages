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
  version = "0.0.21";

  linuxSrc = fetchurl {
    url = "https://github.com/pingdotgg/t3code/releases/download/v${version}/T3-Code-${version}-x86_64.AppImage";
    hash = "sha256-eQCfskpl+JJOyaYY7ogYCi0ZCuWNRcEpseWMniS/LCQ=";
  };

  darwinSrc = fetchurl {
    url = "https://github.com/pingdotgg/t3code/releases/download/v${version}/T3-Code-${version}-arm64.dmg";
    hash = "sha256-8GsNf5n1sh1X444KxnIAQmFCKG3SUbMWK6pyXzUOp9w=";
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

      app="$(find . -maxdepth 3 -name '*.app' -print -quit)"
      if [ -z "$app" ]; then
        echo "Could not find .app inside dmg"
        exit 1
      fi
      cp -r "$app" "$out/Applications/"

      appName="$(basename "$app")"
      exe="$(find "$out/Applications/$appName/Contents/MacOS" \
        -maxdepth 1 -type f -perm -111 -print -quit)"
      if [ -z "$exe" ]; then
        echo "Could not find executable in $appName/Contents/MacOS"
        exit 1
      fi
      ln -s "$exe" "$out/bin/t3code"
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
