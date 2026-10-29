{
  callPackage,
  cargoHash,
  changelog,
  claude-code,
  codex-cli,
  fetchFromGitHub,
  fetchPnpmDeps,
  gh-app-gh,
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
  version,
}:

let
  browserSecretArch = if stdenv.hostPlatform.isx86_64 then "x64" else "arm64";
  spdxLicenseListData = fetchFromGitHub {
    owner = "spdx";
    repo = "license-list-data";
    rev = "c4a7237ec8f4654e867546f9f409749300f1bf4c";
    hash = "sha256-FbeeEBAg9ih6DkAsXdU6ruZwkC7A2u2zYBvblpl54q0=";
  };
  resourceMonitor = rustPlatform.buildRustPackage {
    pname = "t3-resource-monitor";
    inherit version src cargoHash;
    sourceRoot = "${src.name}/native/resource-monitor";
    # Without a sandbox its build creates /homeless-shelter, and Nix then
    # refuses to start the t3code build that follows it.
    preConfigure = ''
      export HOME="$TMPDIR"
    '';
  };
  previewBrowser = callPackage ./preview-browser.nix { inherit src; };
  # Upstream's release builds embed these public client values; without them
  # T3 Connect (relay access and mobile notifications) is compiled out.
  connect = lib.importJSON ./connect.json;
in
(t3code.override {
  inherit claude-code;
  codex = codex-cli;
  # gh-app's gh runs plain gh until the App is configured in T3's environment.
  gh = gh-app-gh;
  enableClaude = true;
}).overrideAttrs
  (
    finalAttrs: previousAttrs: {
      inherit version src;

      VP_SKIP_INSTALL = "1";
      # The web, server and desktop builds read these from the environment
      # through scripts/lib/public-config.ts; with structured attributes only
      # `env` is exported. Relay telemetry stays unset.
      env = (previousAttrs.env or { }) // {
        T3CODE_RELAY_URL = connect.relayUrl;
        T3CODE_CLERK_PUBLISHABLE_KEY = connect.clerkPublishableKey;
        T3CODE_CLERK_JWT_TEMPLATE = connect.clerkJwtTemplate;
        T3CODE_CLERK_CLI_OAUTH_CLIENT_ID = connect.clerkCliOAuthClientId;
      };
      postPatch = ''
        substituteInPlace apps/web/vite.config.ts \
          --replace-fail \
            'const host = explicitHost || "localhost";' \
            'const host = explicitHost || "127.0.0.1";'
        substituteInPlace package.json \
          --replace-fail \
            '"prepare": "vp config --no-agent"' \
            '"prepare": "true"'
        printf '\nverifyDepsBeforeRun: false\n' >> pnpm-workspace.yaml

        # Strict release builds generate third-party notices from this pinned
        # SPDX revision. Warm the generator's cache so the sandboxed build does
        # not try to download license templates.
        mkdir -p .generated/third-party-licenses/spdx
        ln -s ${spdxLicenseListData}/json/details \
          .generated/third-party-licenses/spdx/v3.28.0
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

      # Build each deliverable directly. The workspace task runner can hide
      # nested build failures and the package already needs these artifacts in
      # a fixed order: web assets, the server bundle that embeds them, then the
      # desktop bundle.
      buildPhase = ''
        runHook preBuild

        pnpm --dir apps/web exec vp build
        pnpm --dir apps/server exec node scripts/cli.ts build --verbose
        pnpm --dir apps/desktop exec node scripts/build-browser-secret.mjs
        pnpm --dir apps/desktop exec node scripts/build-preview-annotation-css.mjs
        pnpm --dir apps/desktop exec vp pack

        runHook postBuild
      '';

      postInstall =
        (previousAttrs.postInstall or "")
        + ''
          install -Dm755 ${resourceMonitor}/bin/t3-resource-monitor \
            "$out/libexec/t3code/apps/desktop/prod-resources/resource-monitor/t3-resource-monitor"
          install -Dm755 ${resourceMonitor}/bin/t3-resource-monitor \
            "$out/libexec/t3code/apps/server/dist/resource-monitor/t3-resource-monitor"
          # Fail rather than ship a build without T3 Connect if upstream stops
          # reading the public values from the environment.
          grep -rqF -- 'normalizeSecureRelayUrl("${connect.relayUrl}")' "$out/libexec/t3code/apps/server/dist"
        ''
        + lib.optionalString stdenv.hostPlatform.isLinux ''
          if [[ -f apps/desktop/scripts/build-browser-secret.mjs ]]; then
            install -Dm755 native/browser-secret/build/${browserSecretArch}/t3-browser-secret \
              "$out/libexec/t3code/apps/desktop/prod-resources/browser-secret/t3-browser-secret"
          fi
          wrapProgram "$out/bin/t3code-desktop" --add-flags "--ozone-platform=x11"
          # T3 never looks here itself; host modules link this into each base
          # directory's tools/chrome-headless-shell so previews skip the download.
          ln -s ${previewBrowser} "$out/libexec/t3code/preview-browser"
        '';

      passthru = (previousAttrs.passthru or { }) // {
        inherit previewBrowser resourceMonitor sourceRevision;
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
