{
  lib,
  buildNpmPackage,
  fetchzip,
}:
buildNpmPackage (finalAttrs: {
  pname = "codex-cli";
  version = "0.163.0";

  src = fetchzip {
    url = "https://registry.npmjs.org/@openai/codex/-/codex-${finalAttrs.version}.tgz";
    hash = "sha256-9QnUKccLPXYQCNOXi5i++mCdbAcUbO14Z1AqQ2Ol6fg=";
  };

  npmDepsHash = "sha256-y89zII5qN/lKvUBbT0V2XIVATeql+dFfvPe/rxbQB0c=";

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
