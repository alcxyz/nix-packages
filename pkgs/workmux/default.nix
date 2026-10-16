{
  lib,
  stdenvNoCC,
  fetchurl,
}:

let
  sources = {
    x86_64-linux = {
      artifact = "workmux-linux-amd64";
      hash = "sha256-cjGIAo2uhWSPDMnGh9Dd+k+yrJTckTucTNI9IqA3ZsA=";
    };
    aarch64-linux = {
      artifact = "workmux-linux-arm64";
      hash = "sha256-MAyvE3FOk4lD/3G6X4B4CY/IGCB4NN9p+0ygFwoLUEI=";
    };
    x86_64-darwin = {
      artifact = "workmux-darwin-amd64";
      hash = "sha256-jOJOXTPbNSH0RVxo9qCrM0zskkkldt3g+xCfCPtZGMk=";
    };
    aarch64-darwin = {
      artifact = "workmux-darwin-arm64";
      hash = "sha256-Uid/EV41Ddjn2cFXy/JzOVpaSfjbwdME21tzHpUT/Yw=";
    };
  };

  source =
    sources.${stdenvNoCC.hostPlatform.system}
      or (throw "workmux is not supported on ${stdenvNoCC.hostPlatform.system}");
in
stdenvNoCC.mkDerivation (finalAttrs: {
  pname = "workmux";
  version = "0.1.276";

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
