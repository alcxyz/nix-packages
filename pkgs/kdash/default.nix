{
  lib,
  stdenvNoCC,
  fetchurl,
}:

let
  sources = {
    x86_64-linux = {
      artifact = "kdash-linux-musl";
      hash = "sha256-fSv6ZQXBiyDry8gMMabuGgLg4tHAtptJAn9x9SKiaSA=";
    };
    aarch64-linux = {
      artifact = "kdash-aarch64-musl";
      hash = "sha256-fxOyMsLQToKBOFYd3vQNFvUmnf5r+NyUxLVLZNVI2PQ=";
    };
    x86_64-darwin = {
      artifact = "kdash-macos";
      hash = "sha256-AwRHx23CuBj/i+YctDlz7CMwYN9qDSNGiv2RWVmxCtc=";
    };
    aarch64-darwin = {
      artifact = "kdash-macos-arm64";
      hash = "sha256-o31wCrHTaAxg5mkNhBQ+cOCOxOfqjt/wAOmR87macIw=";
    };
  };

  source =
    sources.${stdenvNoCC.hostPlatform.system}
      or (throw "kdash is not supported on ${stdenvNoCC.hostPlatform.system}");
in
stdenvNoCC.mkDerivation (finalAttrs: {
  pname = "kdash";
  version = "2.0.2";

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
