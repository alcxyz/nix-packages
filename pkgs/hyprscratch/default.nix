{
  lib,
  rustPlatform,
  fetchFromGitHub,
  runCommand,
  python3,
}:

let
  src = fetchFromGitHub {
    owner = "sashetophizika";
    repo = "hyprscratch";
    rev = "1f7bec62c1a3085fc3f899f90a63e9bcc6c13b9d";
    hash = "sha256-nSHwM7TGeEdoGlcpF75PWM4i6jM1pLkUVcQUFXVuGZ4=";
  };
  vendor = rustPlatform.importCargoLock { lockFile = ./Cargo.lock; };
  cargoDeps = runCommand "cargo-vendor-dir" { nativeBuildInputs = [ python3 ]; } ''
    cp -rL ${vendor} $out
    chmod -R u+w $out
    python3 ${./patch-vendor.py} $out
  '';
in
rustPlatform.buildRustPackage {
  pname = "hyprscratch";
  version = "0.6.5-hyprland-0.57";
  inherit src cargoDeps;
  patches = [ ./workspace-addresses.patch ];

  # Upstream's tests open terminals and manipulate a live compositor.
  doCheck = false;
  nativeInstallCheckInputs = [ python3 ];
  doInstallCheck = true;
  installCheckPhase = ''
    runHook preInstallCheck
    python3 ${./test-ipc.py} $out/bin/hyprscratch
    runHook postInstallCheck
  '';

  meta = {
    description = "Scratchpad manager with Hyprland 0.57 workspace compatibility";
    homepage = "https://github.com/sashetophizika/hyprscratch";
    license = lib.licenses.mit;
    platforms = lib.platforms.linux;
    mainProgram = "hyprscratch";
  };
}
