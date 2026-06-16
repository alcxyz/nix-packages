{
  lib,
  stdenvNoCC,
  fetchurl,
}:

let
  sources = {
    x86_64-linux = {
      artifact = "kdash-linux-musl";
      hash = "sha256-OE56CM035EXwbxO/lu/MtuUmx+HqyHVoasepM4fsziQ=";
    };
    aarch64-linux = {
      artifact = "kdash-aarch64-musl";
      hash = "sha256-Tl4IgFogYdv64qk2kxwGdimgAOojnFpkcKKGBFHjBCU=";
    };
    x86_64-darwin = {
      artifact = "kdash-macos";
      hash = "sha256-FXG0nIsZz/YnP/vtZNxMyvKFnXWY75Zyx1uy8yrVUTU=";
    };
    aarch64-darwin = {
      artifact = "kdash-macos-arm64";
      hash = "sha256-Odngt9uLOmsLC9tzHQiokE5SSweU58mS24MN0HfOcQc=";
    };
  };

  source =
    sources.${stdenvNoCC.hostPlatform.system}
      or (throw "kdash is not supported on ${stdenvNoCC.hostPlatform.system}");
in
stdenvNoCC.mkDerivation (finalAttrs: {
  pname = "kdash";
  version = "2.0.0";

  src = fetchurl {
    url = "https://github.com/kdash-rs/kdash/releases/download/v${finalAttrs.version}/${source.artifact}.tar.gz";
    inherit (source) hash;
  };

  sourceRoot = ".";

  installPhase = ''
    runHook preInstall

    install -Dm755 kdash "$out/bin/kdash"

    runHook postInstall
  '';

  meta = {
    description = "Simple and fast dashboard for Kubernetes";
    homepage = "https://github.com/kdash-rs/kdash";
    license = lib.licenses.mit;
    mainProgram = "kdash";
    platforms = builtins.attrNames sources;
    sourceProvenance = with lib.sourceTypes; [ binaryNativeCode ];
  };
})
