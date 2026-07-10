{
  fetchFromGitHub,
  fetchPnpmDeps,
  lib,
  pnpm_11,
  t3code,
}:

let
  version = "0.0.28-alc.1";
  rev = "ec230183c1d25bfacbed8bb2c494fe18fef319d6";
  src = fetchFromGitHub {
    owner = "alcxyz";
    repo = "t3code";
    inherit rev;
    hash = "sha256-icB5AykYXyOw0QDHUskzm+ltTP01eet/RdNm3edCTs8=";
  };
in
t3code.overrideAttrs (
  finalAttrs: previousAttrs: {
    inherit version src;

    nativeBuildInputs = map (
      input: if lib.getName input == "pnpm" then pnpm_11 else input
    ) previousAttrs.nativeBuildInputs;

    pnpmDeps = fetchPnpmDeps {
      pnpm = pnpm_11;
      inherit (finalAttrs) pname;
      inherit version src;
      inherit (previousAttrs) pnpmWorkspaces;
      fetcherVersion = 4;
      hash = "sha256-JmOs6j0Tx8EgZFgvYhhnIPLmEcXirk0AlLvY+onNZhQ=";
    };

    buildPhase = ''
      runHook preBuild

      viteCli="$(node -e '
        const { createRequire } = require("node:module");
        const { dirname, join } = require("node:path");
        const localRequire = createRequire(require.resolve("vite-plus/package.json"));
        process.stdout.write(join(dirname(localRequire.resolve("@voidzero-dev/vite-plus-core")), "cli.js"));
      ')"

      (cd apps/web && node "$viteCli" build)
      (cd apps/server && node ../../node_modules/vite-plus/dist/pack-bin.js)
      cp --recursive apps/web/dist apps/server/dist/client
      node scripts/apply-web-brand-assets.ts development apps/server/dist/client
      (cd apps/desktop && node scripts/build-preview-annotation-css.mjs && node ../../node_modules/vite-plus/dist/pack-bin.js)

      runHook postBuild
    '';

    postBuild = "";

    meta = previousAttrs.meta // {
      changelog = "https://github.com/alcxyz/t3code/commit/${rev}";
    };
  }
)
