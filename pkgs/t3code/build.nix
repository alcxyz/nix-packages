{
  cargoHash,
  changelog,
  claude-code,
  codex-cli,
  fetchPnpmDeps,
  lib,
  libsecret,
  pnpmDepsHash,
  pnpm_11,
  pkg-config,
  rustPlatform,
  sourceRevision,
  src,
  stdenv,
  t3code,
  variant,
  version,
  patchHash ? null,
  patchRevision ? null,
}:

let
  browserSecretArch = if stdenv.hostPlatform.isx86_64 then "x64" else "arm64";
  resourceMonitor = rustPlatform.buildRustPackage {
    pname = "t3-resource-monitor";
    inherit version src cargoHash;
    sourceRoot = "${src.name}/native/resource-monitor";
  };
in
(t3code.override {
  inherit claude-code;
  codex = codex-cli;
  enableClaude = true;
}).overrideAttrs
  (
    finalAttrs: previousAttrs: {
      inherit version src;

      VP_SKIP_INSTALL = "1";
      postPatch = ''
        substituteInPlace apps/web/vite.config.ts \
          --replace-fail \
            'const host = explicitHost || "localhost";' \
            'const host = explicitHost || "127.0.0.1";'
        substituteInPlace package.json \
          --replace-fail \
            ' && vp config --no-agent' \
            ""
        printf '\nverifyDepsBeforeRun: false\n' >> pnpm-workspace.yaml
      '';

      nativeBuildInputs =
        map (input: if lib.getName input == "pnpm" then pnpm_11 else input) previousAttrs.nativeBuildInputs
        ++ lib.optionals stdenv.hostPlatform.isLinux [ pkg-config ];

      buildInputs =
        (previousAttrs.buildInputs or [ ]) ++ lib.optionals stdenv.hostPlatform.isLinux [ libsecret ];

      pnpmDeps = fetchPnpmDeps {
        pnpm = pnpm_11;
        inherit (finalAttrs)
          pname
          version
          src
          pnpmWorkspaces
          ;
        fetcherVersion = 4;
        hash = pnpmDepsHash;
      };

      postInstall =
        (previousAttrs.postInstall or "")
        + ''
          install -Dm755 ${resourceMonitor}/bin/t3-resource-monitor \
            "$out/libexec/t3code/apps/desktop/prod-resources/resource-monitor/t3-resource-monitor"
          install -Dm755 ${resourceMonitor}/bin/t3-resource-monitor \
            "$out/libexec/t3code/apps/server/dist/resource-monitor/t3-resource-monitor"
        ''
        + lib.optionalString stdenv.hostPlatform.isLinux ''
          if [[ -f apps/desktop/scripts/build-browser-secret.mjs ]]; then
            install -Dm755 native/browser-secret/build/${browserSecretArch}/t3-browser-secret \
              "$out/libexec/t3code/apps/desktop/prod-resources/browser-secret/t3-browser-secret"
          fi
          wrapProgram "$out/bin/t3code-desktop" --add-flags "--ozone-platform=x11"
        '';

      passthru = (previousAttrs.passthru or { }) // {
        inherit resourceMonitor;
        inherit
          patchHash
          patchRevision
          sourceRevision
          variant
          ;
        embeddedProviderVersions = {
          claudeCode = claude-code.version;
          codexCli = codex-cli.version;
        };
      };

      meta = previousAttrs.meta // {
        inherit changelog;
      };
    }
  )
