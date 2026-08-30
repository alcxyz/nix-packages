{
  autoPatchelfHook,
  fetchurl,
  lib,
  openssl,
  stdenv,
  xz,
}:

let
  sources = {
    x86_64-linux = {
      target = "amd64";
      hash = "sha256-JVBkhN0YodCJGhJLI5DkmEmcTOJEaehKxt7IlGUce6k=";
    };
    aarch64-linux = {
      target = "arm64";
      hash = "sha256-WMpn1r8LB7cl6Ggy3H8jzg0K+eyFYcyasnMjOI6AlcY=";
    };
  };

  source =
    sources.${stdenv.hostPlatform.system}
      or (throw "Jean server is not supported on ${stdenv.hostPlatform.system}");
in
stdenv.mkDerivation (finalAttrs: {
  pname = "jean-server";
  version = "0.1.73";

  src = fetchurl {
    url = "https://github.com/coollabsio/jean/releases/download/v${finalAttrs.version}/jean-server-linux-${source.target}-${finalAttrs.version}.tar.gz";
    inherit (source) hash;
  };

  nativeBuildInputs = [ autoPatchelfHook ];
  buildInputs = [
    openssl
    xz
    stdenv.cc.cc.lib
  ];

  dontUnpack = true;

  installPhase = ''
    runHook preInstall

    mkdir -p "$out/bin"
    tar -xzf "$src" -C "$out/bin"
    mv "$out/bin/jean-server-linux-${source.target}" "$out/bin/jean-server"
    chmod +x "$out/bin/jean-server"

    runHook postInstall
  '';

  meta = {
    description = "Headless web server for the Jean AI agent environment";
    homepage = "https://jean.build";
    changelog = "https://github.com/coollabsio/jean/releases/tag/v${finalAttrs.version}";
    license = lib.licenses.asl20;
    mainProgram = "jean-server";
    platforms = builtins.attrNames sources;
  };
})
