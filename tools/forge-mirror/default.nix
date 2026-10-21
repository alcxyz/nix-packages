{
  lib,
  buildGoModule,
  git,
  writableTmpDirAsHomeHook,
}:

buildGoModule {
  pname = "forge-mirror";
  version = "0.1.0";

  src = ./.;

  vendorHash = null;

  # The go command writes telemetry under $HOME, which unsandboxed CI builds
  # would create as /homeless-shelter (#603).
  nativeBuildInputs = [ writableTmpDirAsHomeHook ];

  nativeCheckInputs = [ git ];

  meta = {
    description = "Manage Forgejo-first remotes, GitHub mirrors, and drift auditing";
    mainProgram = "forge-mirror";
  };
}
