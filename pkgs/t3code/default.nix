{
  claude-code,
  codex-cli,
  fetchFromGitHub,
  fetchPnpmDeps,
  lib,
  pnpm_11,
  rustPlatform,
  stdenv,
  t3code,
}: let
  version = "0.0.33";
  src = fetchFromGitHub {
    owner = "pingdotgg";
    repo = "t3code";
    tag = "v${version}";
    hash = "sha256-qZi9hMGzqpmnpqvvVtsQvkZIiVqTgOMWv1y15MiSAYg=";
  };
  resourceMonitor = rustPlatform.buildRustPackage {
    pname = "t3-resource-monitor";
    inherit version src;
    sourceRoot = "${src.name}/native/resource-monitor";
    cargoHash = "sha256-5cmG2daM1bVOA23gjjoalbx0fEL1hmqV6WZov0sUZp8=";
  };
in
  (t3code.override {
    inherit claude-code;
    codex = codex-cli;
    enableClaude = true;
  }).overrideAttrs
  (
    finalAttrs: previousAttrs: {
      inherit version;

      inherit src;

      VP_SKIP_INSTALL = "1";
      postPatch = ''
        substituteInPlace apps/web/vite.config.ts \
          --replace-fail \
            'const host = explicitHost || "localhost";' \
            'const host = explicitHost || "127.0.0.1";'
        substituteInPlace package.json \
          --replace-fail \
            '"prepare": "node scripts/clean-tsgo-backups.mjs && effect-tsgo patch && vp config --no-agent"' \
            '"prepare": "node scripts/clean-tsgo-backups.mjs && effect-tsgo patch"'
        printf '\nverifyDepsBeforeRun: false\n' >> pnpm-workspace.yaml
      '';

      nativeBuildInputs =
        map (
          input:
            if lib.getName input == "pnpm"
            then pnpm_11
            else input
        )
        previousAttrs.nativeBuildInputs;

      pnpmDeps = fetchPnpmDeps {
        pnpm = pnpm_11;
        inherit
          (finalAttrs)
          pname
          version
          src
          pnpmWorkspaces
          ;
        fetcherVersion = 4;
        hash = "sha256-i/K5bj7CS7PGIX5hfayxAJ7ngNib92w3SDKGXTVWccA=";
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
          wrapProgram "$out/bin/t3code-desktop" --add-flags "--ozone-platform=x11"
        '';

      passthru =
        (previousAttrs.passthru or {})
        // {
          inherit resourceMonitor;
          embeddedProviderVersions = {
            claudeCode = claude-code.version;
            codexCli = codex-cli.version;
          };
        };

      meta =
        previousAttrs.meta
        // {
          changelog = "https://github.com/pingdotgg/t3code/releases/tag/v${version}";
        };
    }
  )
