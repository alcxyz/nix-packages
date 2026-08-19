{
  lib,
  buildNpmPackage,
  fetchzip,
}:
buildNpmPackage (finalAttrs: {
  pname = "codex-cli";
  version = "0.149.0-alpha.1";

  src = fetchzip {
    url = "https://registry.npmjs.org/@openai/codex/-/codex-${finalAttrs.version}.tgz";
    hash = "sha256-TkZGBU/IHtWLHPU4VN83jlFzXIGP/GDrrC4qyVkunKk=";
  };

  npmDepsHash = "sha256-8Tcj+K6v6cFkQ2lbhQfTdLmuKx0hnrohUCIZb0r0hAw=";

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
