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
      hash = "sha256-GBgXlv1ilht/+RjkGPM4TAo84m97LjKmO4MNytqEOOA=";
    };
    aarch64-linux = {
      target = "aarch64-unknown-linux-musl";
      hash = "sha256-XDbEGs3S8/IGgyzxUkZio6ZW+B8T8+rHcQWPfUGvTxw=";
    };
    x86_64-darwin = {
      target = "x86_64-apple-darwin";
      hash = "sha256-TjWUT5xeeNl9C9hX5oVYHTw3ApGYAO4cu5jJEp3Svxg=";
    };
    aarch64-darwin = {
      target = "aarch64-apple-darwin";
      hash = "sha256-eu2kdwG/jcFGmQUGqIHUO2ln3iro+JxiH3TFnlBOGYk=";
    };
  };

  source =
    sources.${stdenv.hostPlatform.system}
      or (throw "codex-app-server is not supported on ${stdenv.hostPlatform.system}");
in
stdenv.mkDerivation (finalAttrs: {
  pname = "codex-app-server";
  version = "0.152.0";

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
