{
  lib,
  stdenvNoCC,
  fetchurl,
}:

let
  sources = {
    x86_64-linux = {
      artifact = "workmux-linux-amd64";
      hash = "sha256-9g1c1nI/LhVmV0YFR1vsbhMsp54P6u/zXCQJ1T5FVHc=";
    };
    aarch64-linux = {
      artifact = "workmux-linux-arm64";
      hash = "sha256-SPK2AdLpe+7nSINgVZHgQQ4d6xsomnid/KcQK8fqUmM=";
    };
    x86_64-darwin = {
      artifact = "workmux-darwin-amd64";
      hash = "sha256-SNcqG0hPdhGOljN2gvgCdSv8xSear5Gnl60eWvWzlkc=";
    };
    aarch64-darwin = {
      artifact = "workmux-darwin-arm64";
      hash = "sha256-jVxEUirUsqt/cNyVO+JeN8kYiBku2p5nZmW+MTY0yZ8=";
    };
  };

  source =
    sources.${stdenvNoCC.hostPlatform.system}
      or (throw "workmux is not supported on ${stdenvNoCC.hostPlatform.system}");
in
stdenvNoCC.mkDerivation (finalAttrs: {
  pname = "workmux";
  version = "0.1.283";

  src = fetchurl {
    url = "https://github.com/raine/workmux/releases/download/v${finalAttrs.version}/${source.artifact}.tar.gz";
    inherit (source) hash;
  };

  sourceRoot = ".";

  # Upstream ships prebuilt binaries; tmux and git come from PATH.
  installPhase = ''
    runHook preInstall

    install -Dm755 workmux "$out/bin/workmux"

    runHook postInstall
  '';

  meta = {
    description = "Git worktrees and tmux windows for parallel development";
    homepage = "https://github.com/raine/workmux";
    changelog = "https://github.com/raine/workmux/releases/tag/v${finalAttrs.version}";
    license = lib.licenses.mit;
    mainProgram = "workmux";
    platforms = builtins.attrNames sources;
    sourceProvenance = with lib.sourceTypes; [ binaryNativeCode ];
  };
})
