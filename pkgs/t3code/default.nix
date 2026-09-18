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
  withPatch ? false,
}:

let
  source = builtins.fromJSON (builtins.readFile ./source.json);
  quotaPatch = ./patches/claude-quota-recovery.patch;
  patchHash = builtins.hashFile "sha256" quotaPatch;
  patchId = builtins.substring 0 10 patchHash;
  version =
    if withPatch then
      "${source.forkVersion}-fork.${toString source.forkPatchRevision}+p${patchId}"
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
    rev = source.forkRevision;
    hash = source.forkHash;
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
  cargoHash = if withPatch then source.forkCargoHash else source.cargoHash;
  pnpmDepsHash = if withPatch then source.forkPnpmDepsHash else source.pnpmDepsHash;
  changelog =
    if withPatch then
      "https://github.com/alcxyz/t3code/commit/${source.forkRevision}"
    else
      "https://github.com/pingdotgg/t3code/releases/tag/v${source.version}";
  sourceRevision = if withPatch then source.forkRevision else source.revision;
  upstreamRevision = if withPatch then source.forkUpstreamRevision else source.revision;
  variant = if withPatch then "fork" else "upstream";
  patchHash = if withPatch then patchHash else null;
  patchRevision = if withPatch then source.forkPatchRevision else null;
}
