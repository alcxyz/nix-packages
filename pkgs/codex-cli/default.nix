{
  lib,
  buildNpmPackage,
  fetchzip,
}:
buildNpmPackage (finalAttrs: {
  pname = "codex-cli";
  version = "0.149.0-alpha.3";

  src = fetchzip {
    url = "https://registry.npmjs.org/@openai/codex/-/codex-${finalAttrs.version}.tgz";
    hash = "sha256-CQ8nJK5U0yduQjwF1pybwRR+puQ3PkD5RMxmBQvN3mE=";
  };

  npmDepsHash = "sha256-s/tvnhB1zzlcBc/nefzGZCCU0Bl07ok0nmHzyqJm5Rg=";

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
