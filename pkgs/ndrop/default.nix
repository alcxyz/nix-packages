{ lib, rustPlatform, fetchFromGitHub }:

rustPlatform.buildRustPackage rec {
  pname = "ndrop";
  version = "unstable-2025-01-15";
  
  src = fetchFromGitHub {
    owner = "schweber";
    repo = "ndrop";
    rev = "0feb899f34609e4afc0ec166de4f309e2b9c9f02";
    sha256 = lib.fakeHash;
  };

  cargoHash = lib.fakeHash;

  meta = with lib; {
    description = "Scratchpad utility for Wayland";
    homepage = "https://github.com/schweber/ndrop";
    license = licenses.mit;
  };
}
