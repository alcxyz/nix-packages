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
  version = "1.21.3b";

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
      # a0ceeaf931c15d7c37de4faef4ea247eeb10e4b85a9e44b6304277bf3884ae4f
      hash = "sha256-oM7q+THBXXw33k+u9OokfusQ5LhankS2MEJ3vziErk8=";
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
