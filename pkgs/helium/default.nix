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
  version = "0.12.5.1";

  linuxAsset =
    if lib.hasPrefix "x86_64-linux" system then {
      url =
        "https://github.com/imputnet/helium-linux/releases/download/${version}/helium-${version}-x86_64.AppImage";

      # Upstream (hex) SHA256:
      # b9465ab8dada957ea46ad9a73bd5432c4b3e77e1c88648e4795dfa0f0d9e5263
      hash = "sha256-uUZauNralX6katmnO9VDLEs+d+HIhkjkeV36Dw2eUmM=";
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
      # bb0b3a3944f257afc48e8d45a859e93521b7b5350614c35e96b7b1da6d5899dd
      hash = "sha256-uws6OUTyV6/Ejo1FqFnpNSG3tTUGFMNelrex2m1Ymd0=";
    } else if lib.hasPrefix "x86_64-darwin" system then {
      url =
        "https://github.com/imputnet/helium-macos/releases/download/${version}/helium_${version}_x86_64-macos.dmg";

      # Upstream (hex) SHA256:
      # 889495e52f4b33b56fa67e20d9c747ce0240a948c1bd47eefa2cd487937798a2
      hash = "sha256-iJSV5S9LM7Vvpn4g2cdHzgJAqUjBvUfu+izUh5N3mKI=";
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
