{
  lib,
  stdenvNoCC,
  fetchurl,
}:

let
  sources = {
    x86_64-linux = {
      artifact = "workmux-linux-amd64";
      hash = "sha256-NrlC1vYDSl3hT4613S7/xAhVi/dBXTT7HjsgDo2AJuo=";
    };
    aarch64-linux = {
      artifact = "workmux-linux-arm64";
      hash = "sha256-enu0Lh2GSlTdFnvawDLtZQh1+d13atRyCUZzPR7bypk=";
    };
    x86_64-darwin = {
      artifact = "workmux-darwin-amd64";
      hash = "sha256-MPrSjFGih8WsVxgfqFUcEiWSEwR4BPeY1NzmTQQMEB0=";
    };
    aarch64-darwin = {
      artifact = "workmux-darwin-arm64";
      hash = "sha256-yqu+S8jI0UNvE555GxyozucKLgIO3ITBjzykK/S4zAs=";
    };
  };

  source =
    sources.${stdenvNoCC.hostPlatform.system}
      or (throw "workmux is not supported on ${stdenvNoCC.hostPlatform.system}");
in
stdenvNoCC.mkDerivation (finalAttrs: {
  pname = "workmux";
  version = "0.1.280";

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
