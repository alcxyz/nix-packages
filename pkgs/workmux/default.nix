{
  lib,
  stdenvNoCC,
  fetchurl,
}:

let
  sources = {
    x86_64-linux = {
      artifact = "workmux-linux-amd64";
      hash = "sha256-cRB/na0YG6K//2Nny9OWKVWZ768p7OcEs+o0eIomA8k=";
    };
    aarch64-linux = {
      artifact = "workmux-linux-arm64";
      hash = "sha256-Jyiw3uofAZrqu0lpNSEMgkGLdgvext+dPbiTTWQMCCI=";
    };
    x86_64-darwin = {
      artifact = "workmux-darwin-amd64";
      hash = "sha256-J6ts2wYSZXDGHulvApGy17fx2CBvT3as9974z8oKhVo=";
    };
    aarch64-darwin = {
      artifact = "workmux-darwin-arm64";
      hash = "sha256-iBLo8oPTYjH2rPCJIDXoAHTC+ywgGHjGQFA7tp2ZdYc=";
    };
  };

  source =
    sources.${stdenvNoCC.hostPlatform.system}
      or (throw "workmux is not supported on ${stdenvNoCC.hostPlatform.system}");
in
stdenvNoCC.mkDerivation (finalAttrs: {
  pname = "workmux";
  version = "0.1.284";

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
