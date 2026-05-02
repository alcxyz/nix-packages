{ lib, stdenv }:

stdenv.mkDerivation {
  pname = "agent-sync-check";
  version = "0.1.0";

  src = ./.;

  dontBuild = true;

  installPhase = ''
    install -Dm755 check-agent-sync $out/bin/check-agent-sync
  '';

  meta = with lib; {
    description = "Check that AGENTS.md and Claude compatibility instructions stay in sync";
    mainProgram = "check-agent-sync";
    platforms = platforms.unix;
  };
}
