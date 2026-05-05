{
  lib,
  stdenv,
  buildNpmPackage,
  fetchzip,
  bubblewrap,
  procps,
  socat,
}:
buildNpmPackage (finalAttrs: {
  pname = "claude-code";
  version = "2.1.128";

  src = fetchzip {
    url = "https://registry.npmjs.org/@anthropic-ai/claude-code/-/claude-code-${finalAttrs.version}.tgz";
    hash = "sha256-cR6qGXBxU7rgzJUcxsmCUKqEcRQBWmFJgPxtSsXSW64=";
  };

  npmDepsHash = "sha256-pncZCDdsezpNtExE6wVXvdTYAWbD3TZmvqSpCFo/KUA=";

  strictDeps = true;

  postPatch = ''
    cp ${./package-lock.json} package-lock.json

    substituteInPlace cli.js \
          --replace-fail '#!/bin/sh' '#!/usr/bin/env sh'
  '';

  dontNpmBuild = true;

  env.AUTHORIZED = "1";

  postInstall = ''
    wrapProgram $out/bin/claude \
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
