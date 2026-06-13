{
  lib,
  stdenv,
  buildNpmPackage,
  fetchzip,
  bubblewrap,
  procps,
  socat,
}:
let
  platformSuffix =
    {
      x86_64-linux = "linux-x64";
      aarch64-linux = "linux-arm64";
      x86_64-darwin = "darwin-x64";
      aarch64-darwin = "darwin-arm64";
    }.${stdenv.hostPlatform.system}
      or (throw "claude-code is not packaged for ${stdenv.hostPlatform.system}");
in
buildNpmPackage (finalAttrs: {
  pname = "claude-code";
  version = "2.1.177";

  src = fetchzip {
    url = "https://registry.npmjs.org/@anthropic-ai/claude-code/-/claude-code-${finalAttrs.version}.tgz";
    hash = "sha256-uzSTB+4sbK/mpMbN8q/gpjjV5abYF5x19KUN5fSRcrw=";
  };

  npmDepsHash = "sha256-hSz2Ho53Qan9h9pqP+/1DbrPhtu44fLFycysepIl/q8=";

  strictDeps = true;

  postPatch = ''
    cp ${./package-lock.json} package-lock.json
  '';

  dontNpmBuild = true;

  env.AUTHORIZED = "1";

  postInstall = ''
    nativeBinary="$out/lib/node_modules/@anthropic-ai/claude-code/node_modules/@anthropic-ai/claude-code-${platformSuffix}/claude"

    if [ ! -x "$nativeBinary" ]; then
      echo "missing native claude binary: $nativeBinary" >&2
      exit 1
    fi

    rm -f "$out/bin/claude" "$out/bin/.claude-wrapped"
    makeWrapper "$nativeBinary" "$out/bin/claude" \
      --set DISABLE_AUTOUPDATER 1 \
      --set-default FORCE_AUTOUPDATE_PLUGINS 1 \
      --set DISABLE_INSTALLATION_CHECKS 1 \
      --unset DEV \
      --prefix PATH : ${
        lib.makeBinPath (
          [
            procps
          ]
          ++ lib.optionals stdenv.hostPlatform.isLinux [
            bubblewrap
            socat
          ]
        )
      }
  '';

  meta = {
    description = "Agentic coding tool that lives in your terminal";
    homepage = "https://github.com/anthropics/claude-code";
    license = lib.licenses.unfree;
    mainProgram = "claude";
  };
})
