{
  claude-code,
  codex-cli,
  fetchFromGitHub,
  fetchPnpmDeps,
  lib,
  pnpm_11,
  stdenv,
  t3code,
}:

let
  upstreamVersion = "0.0.30";
  version = "${upstreamVersion}-rpc-ping-60s.1";
in
(t3code.override {
  inherit claude-code;
  codex = codex-cli;
  enableClaude = true;
}).overrideAttrs
  (
    finalAttrs: previousAttrs: {
      inherit version;

      src = fetchFromGitHub {
        owner = "pingdotgg";
        repo = "t3code";
        tag = "v${upstreamVersion}";
        hash = "sha256-8N/TbKjaeog5+fbFr1o/Hs0xgbJijsZigo2FdOFtMco=";
      };

      VP_SKIP_INSTALL = "1";
      postPatch = ''
        substituteInPlace apps/web/vite.config.ts \
          --replace-fail \
            'const host = explicitHost || "localhost";' \
            'const host = explicitHost || "127.0.0.1";'
        substituteInPlace package.json \
          --replace-fail \
            '"prepare": "effect-tsgo patch && vp config --no-agent"' \
            '"prepare": "effect-tsgo patch"'
        printf '\nverifyDepsBeforeRun: false\n' >> pnpm-workspace.yaml
      '';

      nativeBuildInputs = map (
        input: if lib.getName input == "pnpm" then pnpm_11 else input
      ) previousAttrs.nativeBuildInputs;

      pnpmDeps = fetchPnpmDeps {
        pnpm = pnpm_11;
        inherit (finalAttrs)
          pname
          version
          src
          pnpmWorkspaces
          ;
        fetcherVersion = 4;
        hash = "sha256-Qiwbg1EPjcVvt8YGc0YYP+1NbgBIxMkwIyTq5f3gtl4=";
      };

      postInstall =
        (previousAttrs.postInstall or "")
        + ''
          rpcClient="$(find "$out/libexec/t3code" \
            -type f -path '*/effect/dist/unstable/rpc/RpcClient.js' \
            -print -quit)"
          if [ -z "$rpcClient" ]; then
            echo "Could not find Effect RPC client in the T3 Code output"
            exit 1
          fi

          if grep -Fq 'Effect.delay("60 seconds")' "$rpcClient"; then
            echo "Effect RPC ping interval is already 60 seconds"
          else
            substituteInPlace "$rpcClient" \
              --replace-fail \
                'Effect.delay("5 seconds")' \
                'Effect.delay("60 seconds")'
          fi
        ''
        + lib.optionalString stdenv.hostPlatform.isLinux ''
          wrapProgram "$out/bin/t3code-desktop" --add-flags "--ozone-platform=x11"
        '';

      meta = previousAttrs.meta // {
        changelog = "https://github.com/pingdotgg/t3code/releases/tag/v${upstreamVersion}";
      };
    }
  )
