{
  lib,
  stdenv,
  fetchurl,
  makeWrapper,
}:

let
  sources = {
    x86_64-linux = {
      target = "x86_64-unknown-linux-musl";
      hash = "sha256-wRxmq/luKMS8Zi1i/raPxMiJxqVtRKBCXnDh0yR6BjM=";
    };
    aarch64-linux = {
      target = "aarch64-unknown-linux-musl";
      hash = "sha256-suTjzPt7SIegMfeshc0RYHUa36MYMMzvPoHb29qtCmI=";
    };
    x86_64-darwin = {
      target = "x86_64-apple-darwin";
      hash = "sha256-0g2UqI8Cb+yu7uct0KQOYaURa9WLjEkt4DKizmyPdBo=";
    };
    aarch64-darwin = {
      target = "aarch64-apple-darwin";
      hash = "sha256-VkB0FsYc+sF7eIOT/qC91tbxZqcr/9KBUaWbCsN/Ib8=";
    };
  };

  source =
    sources.${stdenv.hostPlatform.system}
      or (throw "codex-app-server is not supported on ${stdenv.hostPlatform.system}");
in
stdenv.mkDerivation (finalAttrs: {
  pname = "codex-app-server";
  version = "0.144.1";

  src = fetchurl {
    url = "https://github.com/openai/codex/releases/download/rust-v${finalAttrs.version}/codex-app-server-package-${source.target}.tar.gz";
    inherit (source) hash;
  };

  nativeBuildInputs = [ makeWrapper ];

  dontUnpack = true;

  installPhase = ''
    runHook preInstall

    install_root="$out/lib/codex-app-server"
    mkdir -p "$install_root" "$out/bin"
    tar -xzf "$src" -C "$install_root"

    chmod +x "$install_root/bin/codex-app-server"
    makeWrapper "$install_root/bin/codex-app-server" "$out/bin/codex-app-server" \
      --prefix PATH : "$install_root/codex-path"

    runHook postInstall
  '';

  meta = {
    description = "Codex app server for GUI integrations";
    homepage = "https://github.com/openai/codex";
    license = lib.licenses.asl20;
    mainProgram = "codex-app-server";
    platforms = builtins.attrNames sources;
  };
})
