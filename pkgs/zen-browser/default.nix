{
  lib,
  stdenv,
  fetchurl,
  wrapFirefox,
  wrapGAppsHook3,
  autoPatchelfHook,
  patchelfUnstable,
  gtk3,
  alsa-lib,
  adwaita-icon-theme,
  dbus-glib,
  libXtst,
  curl,
  libva,
  pciutils,
  pipewire,
  writeText,
}:

let
  version = "1.24.1b";

  policies = {
    DisableAppUpdate = true;
  };

  policiesJson = writeText "zen-policies.json" (builtins.toJSON { inherit policies; });

  zen-browser-unwrapped = stdenv.mkDerivation {
    pname = "zen-browser-unwrapped";
    inherit version;

    src = fetchurl {
      url = "https://github.com/zen-browser/desktop/releases/download/${version}/zen.linux-x86_64.tar.xz";

      # Upstream (hex) SHA256:
      # c592a31fe8faa5ddfe91d6243b50c3107e1831c1247539fe03cc04f0fd5cb9cd
      hash = "sha256-xZKjH+j6pd3+kdYkO1DDEH4YMcEkdTn+A8wE8P1cuc0=";
    };

    nativeBuildInputs = [
      wrapGAppsHook3
      autoPatchelfHook
      patchelfUnstable
    ];

    buildInputs = [
      gtk3
      adwaita-icon-theme
      alsa-lib
      dbus-glib
      libXtst
    ];

    runtimeDependencies = [
      curl
      libva.out
      pciutils
    ];

    appendRunpaths = [
      "${pipewire}/lib"
    ];

    # Mozilla uses "relrhack" to manually process relocations from a fixed offset
    patchelfFlags = [ "--no-clobber-old-sections" ];

    installPhase = ''
      runHook preInstall

      mkdir -p "$out/lib/zen-${version}"
      cp -r * "$out/lib/zen-${version}"

      mkdir -p "$out/bin"
      ln -s "$out/lib/zen-${version}/zen" "$out/bin/zen"

      mkdir -p "$out/lib/zen-${version}/distribution"
      ln -s ${policiesJson} "$out/lib/zen-${version}/distribution/policies.json"

      runHook postInstall
    '';

    passthru = {
      binaryName = "zen";
      applicationName = "Zen Browser";
      libName = "zen-${version}";
      ffmpegSupport = true;
      gssSupport = true;
      inherit gtk3;
    };

    meta = {
      description = "Privacy-focused Firefox-based web browser";
      homepage = "https://zen-browser.app";
      license = lib.licenses.mpl20;
      platforms = [ "x86_64-linux" ];
      mainProgram = "zen";
      sourceProvenance = with lib.sourceTypes; [ binaryNativeCode ];
    };
  };
in
wrapFirefox zen-browser-unwrapped {
  pname = "zen-browser";
}
