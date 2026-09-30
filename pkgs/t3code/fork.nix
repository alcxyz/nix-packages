{
  applyPatches,
  claude-code,
  codex-cli,
  fetchFromGitHub,
  fetchPnpmDeps,
  lib,
  libsecret,
  pnpm_11,
  pkg-config,
  rustPlatform,
  stdenv,
  t3code,
  forkChannel ? "nightly",
}@args:
import ./default.nix (args // { inherit forkChannel; })
