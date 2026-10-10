# The Chrome for Testing headless shell that T3's server-side previews run.
# T3 pins one build in its source and downloads it into its base directory on
# first use; that prebuilt binary cannot find its libraries on NixOS. This
# fetches the same pinned archive and links its libraries from the store, so
# hosts can provide it without exposing those libraries to anything else.
{
  alsa-lib,
  at-spi2-atk,
  at-spi2-core,
  atk,
  autoPatchelfHook,
  dbus,
  expat,
  fetchurl,
  glib,
  lib,
  libgbm,
  libx11,
  libxcb,
  libxcomposite,
  libxdamage,
  libxext,
  libxfixes,
  libxkbcommon,
  libxrandr,
  nspr,
  nss,
  src,
  stdenv,
  systemdLibs,
  unzip,
}:

let
  # T3's release table, keyed by Chrome for Testing platform.
  platform = "linux64";
  pinFile = "apps/server/src/preview/PreviewBrowser.ts";
  # Read the pin from the T3 source being built (import from derivation), so
  # each T3 update brings its own browser and a changed pin layout fails
  # evaluation instead of shipping a mismatched browser. Evaluating t3code
  # therefore fetches its source and needs import from derivation enabled.
  lines = lib.splitString "\n" (builtins.readFile "${src}/${pinFile}");
  only =
    label: matches:
    if lib.length matches == 1 then
      lib.head matches
    else
      throw "t3code-preview-browser: expected one ${label} in ${pinFile}, found ${toString (lib.length matches)}";
  capture =
    label: regex: candidates:
    only label (
      lib.concatMap (
        line:
        let
          match = builtins.match regex line;
        in
        lib.optional (match != null) (lib.head match)
      ) candidates
    );
  version = capture "VERSION" ''const VERSION = "([0-9]+\.[0-9]+\.[0-9]+\.[0-9]+)";'' lines;
  platformStart = only "${platform} entry" (
    lib.concatLists (
      lib.imap0 (
        index: line: lib.optional (builtins.match "[[:space:]]*${platform}: \\{" line != null) index
      ) lines
    )
  );
  # The entry's byte count and hash follow its opening line.
  sha256 = capture "${platform} sha256" ''[[:space:]]*sha256: "([0-9a-f]{64})",'' (
    lib.sublist (platformStart + 1) 2 lines
  );
in
stdenv.mkDerivation {
  pname = "t3code-preview-browser";
  inherit version;

  src = fetchurl {
    url = "https://storage.googleapis.com/chrome-for-testing-public/${version}/${platform}/chrome-headless-shell-${platform}.zip";
    inherit sha256;
  };
  sourceRoot = "chrome-headless-shell-${platform}";

  nativeBuildInputs = [
    autoPatchelfHook
    unzip
  ];
  buildInputs = [
    alsa-lib
    at-spi2-atk
    at-spi2-core
    atk
    dbus
    expat
    glib
    libgbm
    libx11
    libxcb
    libxcomposite
    libxdamage
    libxext
    libxfixes
    libxkbcommon
    libxrandr
    nspr
    nss
    systemdLibs
  ];

  dontConfigure = true;
  dontBuild = true;

  # Mirror T3's install layout below its tools/chrome-headless-shell directory.
  installPhase = ''
    runHook preInstall
    mkdir -p "$out/${platform}"
    cp -r . "$out/${platform}/${version}"
    runHook postInstall
  '';

  doInstallCheck = true;
  installCheckPhase = ''
    runHook preInstallCheck
    test -x "$out/${platform}/${version}/chrome-headless-shell"
    runHook postInstallCheck
  '';

  passthru = {
    inherit platform;
  };

  meta = {
    description = "Chrome for Testing headless shell pinned by T3 Code for previews";
    homepage = "https://developer.chrome.com/blog/chrome-headless-shell";
    license = lib.licenses.unfreeRedistributable;
    platforms = [ "x86_64-linux" ];
    sourceProvenance = [ lib.sourceTypes.binaryNativeCode ];
  };
}
