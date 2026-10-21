{
  lib,
  buildGoModule,
  git,
  writableTmpDirAsHomeHook,
}:

buildGoModule {
  pname = "devlog";
  version = "0.1.0";

  src = ./.;

  vendorHash = null;

  # The go command writes telemetry under $HOME, which unsandboxed CI builds
  # would create as /homeless-shelter (#603).
  nativeBuildInputs = [ writableTmpDirAsHomeHook ];

  # The journal sync tests drive real repositories.
  nativeCheckInputs = [ git ];

  meta = {
    description = "Daily and weekly devlog generator from Forgejo and GitHub activity";
    mainProgram = "devlog";
  };
}
