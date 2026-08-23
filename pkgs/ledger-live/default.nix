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
  version = "4.17.0";

  linuxAsset =
    if lib.hasPrefix "x86_64-linux" system then {
      url =
        "https://download.live.ledger.com/ledger-live-desktop-${version}-linux-x86_64.AppImage";

      # Upstream (hex) SHA256:
      # a598a6a7a1fa5b30d4a0bdf595586c8e009b370c26ff4ed1bc26e321421e228a
      hash = "sha256-pZimp6H6WzDUoL31lVhsjgCbNwwm/07RvCbjIUIeIoo=";
    } else
      null;

  darwinAsset =
    if lib.hasPrefix "aarch64-darwin" system || lib.hasPrefix "x86_64-darwin" system then {
      url =
        "https://download.live.ledger.com/ledger-live-desktop-${version}-mac.dmg";

      # Upstream (hex) SHA256:
      # db0dc7b907fae62674f217da3a2f3fe1d9d8eaf043bbad316a137415eb16073c
      hash = "sha256-2w3HuQf65iZ08hfaOi8/4dnY6vBDu60xahN0FesWBzw=";
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
