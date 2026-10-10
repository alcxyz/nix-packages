{
  callPackage,
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
  source = builtins.fromJSON (builtins.readFile ./source.json);
in
import ./build.nix {
  inherit
    callPackage
    claude-code
    codex-cli
    fetchFromGitHub
    fetchPnpmDeps
    lib
    libsecret
    pnpm_11
    pkg-config
    rustPlatform
    stdenv
    t3code
    ;
  inherit (source) version cargoHash pnpmDepsHash;
  src = fetchFromGitHub {
    owner = "pingdotgg";
    repo = "t3code";
    rev = source.revision;
    hash = source.hash;
  };
  changelog = "https://github.com/pingdotgg/t3code/releases/tag/v${source.version}";
  sourceRevision = source.revision;
}
