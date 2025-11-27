{ lib, stdenvNoCC, fetchurl, system ? builtins.currentSystem }:

let
  version = "1.5.5";

  asset =
    if lib.hasPrefix "x86_64-linux" system then {
      url = "https://github.com/carapace-sh/carapace-bin/releases/download/v${version}/carapace-bin_${version}_linux_amd64.tar.gz";
      sha256 = "sha256-NorhxTzBcolhb8dHWxIIYD246giriUI61xYfVUgDASY=";
    } else if lib.hasPrefix "aarch64-linux" system then {
      url = "https://github.com/carapace-sh/carapace-bin/releases/download/v${version}/carapace-bin_${version}_linux_arm64.tar.gz";
      sha256 = lib.fakeHash;
    } else if lib.hasPrefix "x86_64-darwin" system then {
      url = "https://github.com/carapace-sh/carapace-bin/releases/download/v${version}/carapace-bin_${version}_darwin_amd64.tar.gz";
      sha256 = lib.fakeHash;
    } else if lib.hasPrefix "aarch64-darwin" system then {
      url = "https://github.com/carapace-sh/carapace-bin/releases/download/v${version}/carapace-bin_${version}_darwin_arm64.tar.gz";
      sha256 = "sha256-oz8xMbykiPwZNBB/f3iTCbqiTDkIH63hnncnT9O/vnc=";
    } else
      throw "Unsupported system ${system}";

in
stdenvNoCC.mkDerivation {
  pname = "carapace-bin";
  inherit version;

  src = fetchurl asset;

  dontUnpack = true;

  installPhase = ''
    runHook preInstall

    mkdir -p $out/bin
    # $src is set automatically by stdenv
    tar -xzf "$src" -C $out/bin

    runHook postInstall
  '';

  meta = with lib; {
    description = "Pre‑built carapace‑bin ${version} binaries (cross‑shell completion engine)";
    homepage    = "https://github.com/carapace-sh/carapace-bin";
    license     = licenses.mit;
    platforms   = platforms.unix;
    mainProgram = "carapace";
  };
}
