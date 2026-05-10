{
  lib,
  stdenvNoCC,
  fetchurl,
}:

let
  sources = {
    x86_64-linux = {
      artifact = "kdash-linux-musl";
      hash = "sha256-pXvS652v6S7CjTLzLyQKT2cmIlV64v6iOd2Bj1k82Ks=";
    };
    aarch64-linux = {
      artifact = "kdash-aarch64-musl";
      hash = "sha256-dOrX0kXljvFJnBmrASOBPbXFrvre78MqE0hmywQSIJY=";
    };
    x86_64-darwin = {
      artifact = "kdash-macos";
      hash = "sha256-BoyuqeRduHaLelxfkQrJNbsOaus57S50d/FrHSLFS/c=";
    };
    aarch64-darwin = {
      artifact = "kdash-macos-arm64";
      hash = "sha256-R8ivJnG8I/mnSpp1TESXjyFK7duZCGe2ZH4CFUSagg4=";
    };
  };

  source =
    sources.${stdenvNoCC.hostPlatform.system}
      or (throw "kdash is not supported on ${stdenvNoCC.hostPlatform.system}");
in
stdenvNoCC.mkDerivation (finalAttrs: {
  pname = "kdash";
  version = "1.1.1";

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
