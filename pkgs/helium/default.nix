# nix-packages/pkgs/helium/default.nix
{
  lib,
  stdenv,
  stdenvNoCC,
  fetchurl,
  undmg,
  makeWrapper,
  appimageTools,
  system ? stdenv.hostPlatform.system,
}:

let
  pname = "helium";
  version = "0.11.7.1";

  linuxAsset =
    if lib.hasPrefix "x86_64-linux" system then {
      url =
        "https://github.com/imputnet/helium-linux/releases/download/${version}/helium-${version}-x86_64.AppImage";

      # Upstream (hex) SHA256:
      # ab3735df920fe45d9bb71b4e31418ccfed1acc98582fd288d2570f1b62a37315
      hash = "sha256-qzc135IP5F2btxtOMUGMz+0azJhYL9KI0lcPG2KjcxU=";
    } else if lib.hasPrefix "aarch64-linux" system then {
      url =
        "https://github.com/imputnet/helium-linux/releases/download/${version}/helium_${version}_arm64.AppImage";

      # Upstream (hex) SHA256:
      # unknown
      hash = lib.fakeHash;
    } else
      null;

  darwinAsset =
    if lib.hasPrefix "aarch64-darwin" system then {
      url =
        "https://github.com/imputnet/helium-macos/releases/download/${version}/helium_${version}_arm64-macos.dmg";

      # Upstream (hex) SHA256:
      # 8dbbbfc37fc6602eb41acd2e79e102adb7830d22c0a1714c6a7aae229c3456f3
      hash = "sha256-jbu/w3/GYC60Gs0ueeECrbeDDSLAoXFManquIpw0VvM=";
    } else if lib.hasPrefix "x86_64-darwin" system then {
      url =
        "https://github.com/imputnet/helium-macos/releases/download/${version}/helium_${version}_x86_64-macos.dmg";

      # Upstream (hex) SHA256:
      # 545dc4b2186c6b1fcd241047f3cfc2cb3e0500f2152e50035a0fbee1b5fb4dbe
      hash = "sha256-VF3Eshhsax/NJBBH88/Cyz4FAPIVLlADWg++4bX7Tb4=";
    } else
      null;
in
if linuxAsset != null then
  let
    src = fetchurl {
      inherit (linuxAsset) url hash;
    };

    appimageContents = appimageTools.extractType2 { inherit pname version src; };
  in
  appimageTools.wrapType2 {
    inherit pname version src;

    extraInstallCommands = ''
      # Try to install desktop entry + icon if present in the AppImage
      desktop="$(find ${appimageContents} -maxdepth 5 -name '*.desktop' | head -n1)"
      if [ -n "$desktop" ]; then
        install -Dm444 "$desktop" "$out/share/applications/${pname}.desktop"
        substituteInPlace "$out/share/applications/${pname}.desktop" \
          --replace 'Exec=AppRun' 'Exec=${pname}' || true
      fi

      icon="$(find ${appimageContents} -maxdepth 8 -name '*.png' \
        | grep -E '/(128|256|512)x(128|256|512)/' | head -n1)"
      if [ -n "$icon" ]; then
        size="$(echo "$icon" | grep -Eo '/[0-9]{2,3}x[0-9]{2,3}/' | tr -d /)"
        install -Dm444 "$icon" \
          "$out/share/icons/hicolor/$size/apps/${pname}.png"
      fi
    '';

    meta = with lib; {
      description = "Helium Browser (AppImage)";
      homepage = "https://github.com/imputnet/helium";
      license = with licenses; [ gpl3Only bsd3 ];
      platforms = [ "x86_64-linux" "aarch64-linux" ];
      mainProgram = "helium";
    };
  }
else if darwinAsset != null then
  stdenvNoCC.mkDerivation {
    inherit pname version;

    src = fetchurl {
      inherit (darwinAsset) url hash;
    };

    nativeBuildInputs = [ undmg makeWrapper ];
    dontUnpack = true;

    installPhase = ''
      runHook preInstall

      undmg "$src"

      app="$(find . -maxdepth 3 -name '*.app' -print -quit)"
      if [ -z "$app" ]; then
        echo "Could not find .app inside dmg"
        exit 1
      fi

      mkdir -p "$out/Applications"
      cp -R "$app" "$out/Applications/Helium.app"

      exe="$(find "$out/Applications/Helium.app/Contents/MacOS" \
        -maxdepth 1 -type f -perm -111 -print -quit)"
      if [ -z "$exe" ]; then
        echo "Could not find an executable in Helium.app/Contents/MacOS"
        exit 1
      fi

      mkdir -p "$out/bin"
      makeWrapper "$exe" "$out/bin/helium"

      runHook postInstall
    '';

    meta = with lib; {
      description = "Helium Browser (macOS .app from dmg)";
      homepage = "https://github.com/imputnet/helium";
      license = with licenses; [ gpl3Only bsd3 ];
      platforms = [ "aarch64-darwin" "x86_64-darwin" ];
      mainProgram = "helium";
    };
  }
else
  throw "Unsupported system ${system}"
