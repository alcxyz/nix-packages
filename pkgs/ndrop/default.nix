{ lib, stdenvNoCC }:

stdenvNoCC.mkDerivation rec {
  pname = "ndrop";
  version = "vendored-${builtins.substring 0 7 (builtins.readFile ./src/upstream/.git/HEAD or "unknown")}";

  src = ./src/upstream;

  installPhase = ''
    runHook preInstall

    mkdir -p $out/bin
    install -m755 ndrop $out/bin/ndrop

    # Optional: install man page if you want it available via man ndrop
    if [ -f ndrop.1.scd ]; then
      mkdir -p $out/share/man/man1
      # If you have scdoc available, you could convert; otherwise, install as-is
      # nixpkgs has scdoc; you can add it to nativeBuildInputs and generate:
      # scdoc < ndrop.1.scd > $out/share/man/man1/ndrop.1
      # For now, just drop the source manpage
      install -m644 ndrop.1.scd $out/share/man/man1/ndrop.1.scd
    fi

    runHook postInstall
  '';

  meta = with lib; {
    description = "Scratchpad-like toggle helper for Wayland compositors (prebuilt binary)";
    homepage = "https://github.com/schweber/ndrop";
    license = licenses.mit;
    platforms = platforms.linux;
    mainProgram = "ndrop";
  };
}
