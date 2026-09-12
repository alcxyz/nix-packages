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
  patchRevision = 4;
  patchHash = builtins.hashFile "sha256" ./patches/automatic-thread-titles.patch;
  patchId = builtins.substring 0 10 patchHash;
  version =
    source.version + lib.optionalString withPatch "-fork.${toString patchRevision}+p${patchId}";
  upstreamSrc = fetchFromGitHub {
    owner = "pingdotgg";
    repo = "t3code";
    rev = source.revision;
    hash = source.hash;
  };
  src =
    if withPatch then
      applyPatches {
        name = "t3code-${version}-source";
        src = upstreamSrc;
        patches = [ ./patches/automatic-thread-titles.patch ];
      }
    else
      upstreamSrc;
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
  inherit (source) cargoHash pnpmDepsHash;
  changelog = "https://github.com/pingdotgg/t3code/releases/tag/v${source.version}";
  sourceRevision = source.revision;
  variant = if withPatch then "fork" else "upstream";
  patchHash = if withPatch then patchHash else null;
  patchRevision = if withPatch then patchRevision else null;
}
