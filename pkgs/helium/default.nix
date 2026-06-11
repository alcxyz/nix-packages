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
  version = "0.13.2.1";

  linuxAsset =
    if lib.hasPrefix "x86_64-linux" system then {
      url =
        "https://github.com/imputnet/helium-linux/releases/download/${version}/helium-${version}-x86_64.AppImage";

      # Upstream (hex) SHA256:
      # 23d56a5c4db414d8c4cfd172bdc099f19a9165b3c853e42d18f6e501dc09464f
      hash = "sha256-I9VqXE20FNjEz9FyvcCZ8ZqRZbPIU+QtGPblAdwJRk8=";
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
      # d15c39899e08ae8d061c338e221e44e1c25b5576aa63479d7981af58301dc3c1
      hash = "sha256-0Vw5iZ4Iro0GHDOOIh5E4cJbVXaqY0edeYGvWDAdw8E=";
    } else if lib.hasPrefix "x86_64-darwin" system then {
      url =
        "https://github.com/imputnet/helium-macos/releases/download/${version}/helium_${version}_x86_64-macos.dmg";

      # Upstream (hex) SHA256:
      # 92a2fe581e412d0af0769bd38d4b774319927c9473906ce5b49984856dff7e12
      hash = "sha256-kqL+WB5BLQrwdpvTjUt3QxmSfJRzkGzltJmEhW3/fhI=";
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
