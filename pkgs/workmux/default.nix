{
  lib,
  stdenvNoCC,
  fetchurl,
}:

let
  sources = {
    x86_64-linux = {
      artifact = "workmux-linux-amd64";
      hash = "sha256-umv6wqENaEmZLtjnHBQXy9Bmb2S+Hv9EpliVzsaEYaA=";
    };
    aarch64-linux = {
      artifact = "workmux-linux-arm64";
      hash = "sha256-+sjecZhHiHn2x+oYos8e6RYaTgYG7tb+l6lMEJimBLQ=";
    };
    x86_64-darwin = {
      artifact = "workmux-darwin-amd64";
      hash = "sha256-oOiBBzUR054Z6BHu4zScfDbE4YKJrKkBMjZA/Rb33Xo=";
    };
    aarch64-darwin = {
      artifact = "workmux-darwin-arm64";
      hash = "sha256-bSzgoXThFtcAPTXOQgIb/V5lZN2uJc2Qb47bSSC59ls=";
    };
  };

  source =
    sources.${stdenvNoCC.hostPlatform.system}
      or (throw "workmux is not supported on ${stdenvNoCC.hostPlatform.system}");
in
stdenvNoCC.mkDerivation (finalAttrs: {
  pname = "workmux";
  version = "0.1.278";

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
