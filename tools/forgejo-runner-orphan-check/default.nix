{
  lib,
  buildGoModule,
  writableTmpDirAsHomeHook,
}:

buildGoModule {
  pname = "forgejo-runner-orphan-check";
  version = "0.1.0";

  src = ./.;

  vendorHash = null;

  # The go command writes telemetry under $HOME, which unsandboxed CI builds
  # would create as /homeless-shelter (#603).
  nativeBuildInputs = [ writableTmpDirAsHomeHook ];

  meta = {
    description = "Detect Forgejo Actions containers whose tasks are terminal";
    mainProgram = "forgejo-runner-orphan-check";
    platforms = lib.platforms.linux;
  };
}
