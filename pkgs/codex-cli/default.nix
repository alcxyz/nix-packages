{
  lib,
  buildNpmPackage,
  fetchzip,
}:
buildNpmPackage (finalAttrs: {
  pname = "codex-cli";
  version = "0.168.0";

  src = fetchzip {
    url = "https://registry.npmjs.org/@openai/codex/-/codex-${finalAttrs.version}.tgz";
    hash = "sha256-S6ncoJv5G+1k5J+EQax1HZZdEMfwe1ObZhe8qLyNmio=";
  };

  npmDepsHash = "sha256-GEIRsRsdfZoYghfGo3SDhXsAthUeZpTBRJYWiSdR1ns=";

  strictDeps = true;

  postPatch = ''
    cp ${./package-lock.json} package-lock.json
  '';

  dontNpmBuild = true;

  meta = {
    description = "Lightweight coding agent that runs in your terminal";
    homepage = "https://github.com/openai/codex";
    license = lib.licenses.asl20;
    mainProgram = "codex";
    platforms = lib.platforms.linux ++ lib.platforms.darwin;
  };
})
