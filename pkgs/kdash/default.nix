{
  lib,
  stdenvNoCC,
  fetchurl,
}:

let
  sources = {
    x86_64-linux = {
      artifact = "kdash-linux-musl";
      hash = "sha256-fDEwpH9o8dMLqFhjXl7eM7s2CXLQITvZARYVbgyeIII=";
    };
    aarch64-linux = {
      artifact = "kdash-aarch64-musl";
      hash = "sha256-K7iY9ekHBFFGJMy86W5VChrEIxci9jnbnFztVpa6J6w=";
    };
    x86_64-darwin = {
      artifact = "kdash-macos";
      hash = "sha256-sayY2EB+W/4B/A3jC/SDdkDXkhRjc2wKJN3xAMdZlAA=";
    };
    aarch64-darwin = {
      artifact = "kdash-macos-arm64";
      hash = "sha256-liQZP0uJt/6jhmHuzi+LriaTi8xQe845YfEfSnStM/Y=";
    };
  };

  source =
    sources.${stdenvNoCC.hostPlatform.system}
      or (throw "kdash is not supported on ${stdenvNoCC.hostPlatform.system}");
in
stdenvNoCC.mkDerivation (finalAttrs: {
  pname = "kdash";
  version = "1.1.2";

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
