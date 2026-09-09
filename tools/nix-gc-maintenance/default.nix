{
  bash,
  coreutils,
  findutils,
  gawk,
  lib,
  makeWrapper,
  nix,
  openssh,
  stdenvNoCC,
}:
stdenvNoCC.mkDerivation {
  pname = "nix-gc-maintenance";
  version = "0.1.0";

  src = ./nix-gc-maintenance.sh;
  dontUnpack = true;

  nativeBuildInputs = [ makeWrapper ];

  installPhase = ''
    install -Dm755 "$src" "$out/libexec/nix-gc-maintenance"
    patchShebangs "$out/libexec/nix-gc-maintenance"
    makeWrapper "$out/libexec/nix-gc-maintenance" "$out/bin/nix-gc-maintenance" \
      --set NIX_GC_SCRIPT_SOURCE "$out/libexec/nix-gc-maintenance" \
      --prefix PATH : ${
        lib.makeBinPath [
          bash
          coreutils
          findutils
          gawk
          nix
          openssh
        ]
      }
  '';

  meta = {
    description = "Conservative manual Nix profile retention and capped garbage collection";
    mainProgram = "nix-gc-maintenance";
    platforms = lib.platforms.unix;
  };
}
