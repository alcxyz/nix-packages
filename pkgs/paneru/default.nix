{
  apple-sdk,
  fetchFromGitHub,
  lib,
  rustPlatform,
}:

rustPlatform.buildRustPackage rec {
  pname = "paneru";
  version = "0.4.1";

  src = fetchFromGitHub {
    owner = "karinushka";
    repo = "paneru";
    tag = "v${version}";
    hash = "sha256-34DZal5YMFR6/sgxsXHO48FvIfwN8LTwDXgwAYRwn3k=";
  };

  patches = [
    ./patches/center-narrow-strip.patch
  ];

  cargoPatches = [
    ./patches/update-cargo-lock.patch
  ];

  postPatch = ''
    substituteInPlace build.rs --replace-fail \
      'let sdk_dir = "/Library/Developer/CommandLineTools/SDKs";' \
      'let sdk_dir = "${apple-sdk}/Platforms/MacOSX.platform/Developer/SDKs";'
  '';

  cargoHash = "sha256-W4Hrrtnt8yPpKkqROxa9fG4wpk2XBHem0byyDDL9Jvo=";

  buildInputs = [
    apple-sdk.privateFrameworksHook
  ];

  doCheck = false;

  meta = with lib; {
    description = "Sliding, tiling window manager for macOS";
    homepage = "https://github.com/karinushka/paneru";
    license = licenses.mit;
    mainProgram = "paneru";
    platforms = platforms.darwin;
  };
}
