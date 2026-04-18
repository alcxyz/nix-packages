# pkgs/ghostty/default.nix
# macOS only — official universal DMG (aarch64 + x86_64 combined).
# Linux: use nixpkgs ghostty (source-built, tracks unstable).
{
  lib,
  stdenvNoCC,
  fetchurl,
  undmg,
  makeWrapper,
  system ? stdenvNoCC.hostPlatform.system,
}:

let
  pname = "ghostty";
  version = "1.3.1";
in
if lib.hasPrefix "aarch64-darwin" system || lib.hasPrefix "x86_64-darwin" system then
  stdenvNoCC.mkDerivation {
    inherit pname version;

    src = fetchurl {
      url = "https://release.files.ghostty.org/${version}/Ghostty.dmg";

      # Upstream (hex) SHA256:
      # 18cff2b0a6cee90eead9c7d3064e808a252a40baf214aa752c1ecb793b8f5f69
      hash = "sha256-GM/ysKbO6Q7q2cfTBk6AiiUqQLryFKp1LB7LeTuPX2k=";
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
      cp -R "$app" "$out/Applications/Ghostty.app"

      exe="$(find "$out/Applications/Ghostty.app/Contents/MacOS" \
        -maxdepth 1 -type f -perm -111 -print -quit)"
      if [ -z "$exe" ]; then
        echo "Could not find executable in Ghostty.app/Contents/MacOS"
        exit 1
      fi

      mkdir -p "$out/bin"
      makeWrapper "$exe" "$out/bin/ghostty"

      runHook postInstall
    '';

    meta = with lib; {
      description = "Ghostty terminal emulator";
      homepage = "https://ghostty.org";
      license = licenses.mit;
      platforms = [ "aarch64-darwin" "x86_64-darwin" ];
      mainProgram = "ghostty";
    };
  }

else
  throw "ghostty: use nixpkgs ghostty on Linux; macOS only here (${system})"
