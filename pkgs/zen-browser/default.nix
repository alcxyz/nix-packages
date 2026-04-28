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
  version = "1.19.10b";

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
      # afba63e56a638d0c6f2dffcc16e59c20055c06ab2df95270daf70a463028b973
      hash = "sha256-r7pj5WpjjQxvLf/MFuWcIAVcBqst+VJw2vcKRjAouXM=";
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
