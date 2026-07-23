{
  lib,
  stdenvNoCC,
  fetchurl,
}:

let
  sources = {
    x86_64-linux = {
      artifact = "kdash-linux-musl";
      hash = "sha256-VbMtfZU/9A89xv+EJb2VOR+m0sPpNZUEV7wDMgHlyfQ=";
    };
    aarch64-linux = {
      artifact = "kdash-aarch64-musl";
      hash = "sha256-8bH79IYWIWqkLSP/PDaZLVJ3QFtYy7xkMayhrri5JsM=";
    };
    x86_64-darwin = {
      artifact = "kdash-macos";
      hash = "sha256-QVklB7bGNzqwz+bOM33ADQh5N8K3rWkaN4RCxAZzhuk=";
    };
    aarch64-darwin = {
      artifact = "kdash-macos-arm64";
      hash = "sha256-kJCpTW/dMcEUIjgzCqmhdfpeMCBwbV7VA7oy6eihWr8=";
    };
  };

  source =
    sources.${stdenvNoCC.hostPlatform.system}
      or (throw "kdash is not supported on ${stdenvNoCC.hostPlatform.system}");
in
stdenvNoCC.mkDerivation (finalAttrs: {
  pname = "kdash";
  version = "2.1.1";

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
