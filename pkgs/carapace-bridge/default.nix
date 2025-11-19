{ lib, stdenvNoCC, fetchurl, system ? builtins.currentSystem }:

let
  version = "1.5.5";

  asset =
    if lib.hasPrefix "x86_64-linux" system then {
      url = "https://github.com/carapace-sh/carapace-bin/releases/download/v${version}/carapace-bin_${version}_linux_amd64.tar.gz";
      hash = "sha256-NorhxTzBcolhb8dHWxIIYD246giriUI61xYfVUgDASY=";
    } else if lib.hasPrefix "aarch64-linux" system then {
      url = "https://github.com/carapace-sh/carapace-bin/releases/download/v${version}/carapace-bin_${version}_linux_arm64.tar.gz";
      hash = lib.fakeHash;
    } else if lib.hasPrefix "x86_64-darwin" system then {
      url = "https://github.com/carapace-sh/carapace-bin/releases/download/v${version}/carapace-bin_${version}_darwin_amd64.tar.gz";
      hash = lib.fakeHash;
    } else if lib.hasPrefix "aarch64-darwin" system then {
      url = "https://github.com/carapace-sh/carapace-bin/releases/download/v${version}/carapace-bin_${version}_darwin_arm64.tar.gz";
      hash = lib.fakeHash;
    } else
      throw "Unsupported system ${system}";
in
stdenvNoCC.mkDerivation {
  pname = "carapace-bridge";
  inherit version;

  src = fetchurl asset;
  dontUnpack = true;

  installPhase = ''
    runHook preInstall
    mkdir -p $out/bin
    tar -xzf "$src" -C $out/bin
    # the release archive contains all bridges;
    # install those and skip the main `carapace` binary
    rm -f $out/bin/carapace || true
    runHook postInstall
  '';

  meta = with lib; {
    description = "Pre‑built carapace‑bridge binaries for shell integration";
    homepage    = "https://github.com/carapace-sh/carapace-bin";
    license     = licenses.mit;
    platforms   = platforms.unix;
  };
}
