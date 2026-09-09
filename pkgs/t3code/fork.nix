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
}:

let
  revision = "6c583620ff7ad3235b135af7107c0543467eecfa";
  patchRevision = 1;
  patchHash = builtins.hashFile "sha256" ./patches/automatic-thread-titles.patch;
  patchId = builtins.substring 0 10 patchHash;
  version = "0.0.40-fork.${toString patchRevision}+g${builtins.substring 0 10 revision}.p${patchId}";
  upstreamSrc = fetchFromGitHub {
    owner = "pingdotgg";
    repo = "t3code";
    rev = revision;
    hash = "sha256-rQtc2IfbUwoe4LKI6hyjL8i7VvQKN8/BAeLojQUnA6E=";
  };
  src = applyPatches {
    name = "t3code-${version}-source";
    src = upstreamSrc;
    patches = [ ./patches/automatic-thread-titles.patch ];
  };
in
import ./build.nix {
  inherit
    claude-code
    codex-cli
    fetchPnpmDeps
    lib
    libsecret
    pnpm_11
    pkg-config
    rustPlatform
    src
    stdenv
    t3code
    version
    ;
  cargoHash = "sha256-5cmG2daM1bVOA23gjjoalbx0fEL1hmqV6WZov0sUZp8=";
  changelog = "https://github.com/pingdotgg/t3code/commit/${revision}";
  inherit patchHash patchRevision;
  pnpmDepsHash = "sha256-EO844JyOlqtUG+mGWOeXlVtQjRpFFgwiXRvTgfEh7ao=";
  sourceRevision = revision;
  variant = "fork";
}
