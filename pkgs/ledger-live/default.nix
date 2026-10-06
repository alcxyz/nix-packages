# nix-packages/pkgs/ledger-live/default.nix
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
  pname = "ledger-live";
  version = "4.23.0";

  linuxAsset =
    if lib.hasPrefix "x86_64-linux" system then {
      url =
        "https://download.live.ledger.com/ledger-live-desktop-${version}-linux-x86_64.AppImage";

      # Upstream (hex) SHA256:
      # 079e1bf8defdf3afc5ba662268bc0437b6b7c1317969e5c5b17e09ba16f29cae
      hash = "sha256-B54b+N7986/FumYiaLwEN7a3wTF5aeXFsX4JuhbynK4=";
    } else
      null;

  darwinAsset =
    if lib.hasPrefix "aarch64-darwin" system || lib.hasPrefix "x86_64-darwin" system then {
      url =
        "https://download.live.ledger.com/ledger-live-desktop-${version}-mac.dmg";

      # Upstream (hex) SHA256:
      # d57f5bf7965a62133f5ab0203f061fa48284ff96cf5769dc99069af6248b9787
      hash = "sha256-1X9b95ZaYhM/WrAgPwYfpIKE/5bPV2ncmQaa9iSLl4c=";
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
      desktop="$(find ${appimageContents} -maxdepth 5 -name '*.desktop' | head -n1)"
      if [ -n "$desktop" ]; then
        install -Dm444 "$desktop" "$out/share/applications/${pname}.desktop"
        substituteInPlace "$out/share/applications/${pname}.desktop" \
          --replace-fail 'Exec=AppRun' 'Exec=${pname}'
      fi

      icon="$(find ${appimageContents} -path '*/hicolor/*/apps/*.png' | head -n1)"
      if [ -n "$icon" ]; then
        size="$(basename "$(dirname "$(dirname "$icon")")")"
        install -Dm444 "$icon" \
          "$out/share/icons/hicolor/$size/apps/${pname}.png"
      fi
    '';

    meta = with lib; {
      description = "Ledger Wallet - companion app for Ledger hardware wallets";
      homepage = "https://www.ledger.com/ledger-live";
      license = licenses.mit;
      platforms = [ "x86_64-linux" ];
      mainProgram = "ledger-live";
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
      cp -R "$app" "$out/Applications/Ledger Live.app"

      exe="$(find "$out/Applications/Ledger Live.app/Contents/MacOS" \
        -maxdepth 1 -type f -perm -111 -print -quit)"
      if [ -z "$exe" ]; then
        echo "Could not find an executable in Ledger Live.app/Contents/MacOS"
        exit 1
      fi

      mkdir -p "$out/bin"
      makeWrapper "$exe" "$out/bin/ledger-live"

      runHook postInstall
    '';

    meta = with lib; {
      description = "Ledger Wallet - companion app for Ledger hardware wallets";
      homepage = "https://www.ledger.com/ledger-live";
      license = licenses.mit;
      platforms = [ "aarch64-darwin" "x86_64-darwin" ];
      mainProgram = "ledger-live";
    };
  }
else
  throw "Unsupported system ${system}"
