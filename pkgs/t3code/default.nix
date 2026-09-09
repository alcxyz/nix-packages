{
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
  version = "0.0.38";
  src = fetchFromGitHub {
    owner = "pingdotgg";
    repo = "t3code";
    tag = "v${version}";
    hash = "sha256-lbAOIlNwVxrjXA5jJGzmOm7Fe2ZcsnFuDzaSEt6R7G4=";
  };
  cargoHash = "sha256-5cmG2daM1bVOA23gjjoalbx0fEL1hmqV6WZov0sUZp8=";
  pnpmDeps = {
    fetcherVersion = 4;
    hash = "sha256-t/hmpXdYPnBFx18A6NrSL4zSvVnUDIjIPtLjGOzoaDk=";
  };
in
import ./build.nix {
  inherit
    cargoHash
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
  changelog = "https://github.com/pingdotgg/t3code/releases/tag/v${version}";
  pnpmDepsHash = pnpmDeps.hash;
  sourceRevision = "v${version}";
  variant = "upstream";
}
