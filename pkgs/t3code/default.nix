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
  forkChannel ? null,
}:

let
  source = builtins.fromJSON (builtins.readFile ./source.json);
  withPatch = forkChannel != null;
  fork = if !withPatch then null else source.forks.${forkChannel};
  quotaPatch = ./patches/claude-quota-recovery.patch;
  patchHash = builtins.hashFile "sha256" quotaPatch;
  patchId = builtins.substring 0 10 patchHash;
  version =
    if withPatch then
      "${fork.version}-fork.${toString fork.patchRevision}+p${patchId}"
    else
      source.version;
  upstreamSrc = fetchFromGitHub {
    owner = "pingdotgg";
    repo = "t3code";
    rev = source.revision;
    hash = source.hash;
  };
  forkSrc = fetchFromGitHub {
    owner = "alcxyz";
    repo = "t3code";
    rev = fork.revision;
    hash = fork.hash;
  };
  src =
    if withPatch then
      applyPatches {
        name = "t3code-${version}-source";
        src = forkSrc;
        # Temporary quota recovery, maintained independently of the title
        # feature carried by the tested fork source.
        patches = [ quotaPatch ];
      }
    else
      upstreamSrc;
in
import ./build.nix {
  inherit
    claude-code
    codex-cli
    fetchFromGitHub
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
  cargoHash = if withPatch then fork.cargoHash else source.cargoHash;
  pnpmDepsHash = if withPatch then fork.pnpmDepsHash else source.pnpmDepsHash;
  changelog =
    if withPatch then
      "https://github.com/alcxyz/t3code/commit/${fork.revision}"
    else
      "https://github.com/pingdotgg/t3code/releases/tag/v${source.version}";
  sourceRevision = if withPatch then fork.revision else source.revision;
  upstreamRevision = if withPatch then fork.upstreamRevision else source.revision;
  variant = if withPatch then "fork-${forkChannel}" else "upstream";
  patchHash = if withPatch then patchHash else null;
  patchRevision = if withPatch then fork.patchRevision else null;
}
