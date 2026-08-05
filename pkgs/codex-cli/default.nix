{
  lib,
  buildNpmPackage,
  fetchzip,
}:
buildNpmPackage (finalAttrs: {
  pname = "codex-cli";
  version = "0.147.0-alpha.6.4";

  src = fetchzip {
    url = "https://registry.npmjs.org/@openai/codex/-/codex-${finalAttrs.version}.tgz";
    hash = "sha256-HBAMuPpmG5tZVsfZb7wZDFmUS+gbeMay+ab+Cvsh1eM=";
  };

  npmDepsHash = "sha256-r1pn5CJ4xV0CVoqtQRUGUUGdnlhYh+X+fmDxIbRglHo=";

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
