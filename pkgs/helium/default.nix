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
  version = "0.11.5.1";

  linuxAsset =
    if lib.hasPrefix "x86_64-linux" system then {
      url =
        "https://github.com/imputnet/helium-linux/releases/download/${version}/helium-${version}-x86_64.AppImage";

      # Upstream (hex) SHA256:
      # 362ec867d50169fafeb2cd017106912aa9a5989238215d6344027c8e17287658
      hash = "sha256-Ni7IZ9UBafr+ss0BcQaRKqmlmJI4IV1jRAJ8jhcodlg=";
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
      # 3f9897b574b4e6ebb9432f633e179701b8dec27ea329372a739b85db2668cff9
      hash = "sha256-P5iXtXS05uu5Qy9jPheXAbjewn6jKTcqc5uF2yZoz/k=";
    } else if lib.hasPrefix "x86_64-darwin" system then {
      url =
        "https://github.com/imputnet/helium-macos/releases/download/${version}/helium_${version}_x86_64-macos.dmg";

      # Upstream (hex) SHA256:
      # 608d1ce430ed3e2cebfe6b830a2f1b5aedf053de960f1c3acbc6e608e6fab30d
      hash = "sha256-YI0c5DDtPizr/muDCi8bWu3wU96WDxw6y8bmCOb6sw0=";
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
