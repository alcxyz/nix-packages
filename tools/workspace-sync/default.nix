{
  lib,
  bash,
  buildGoModule,
  coreutils,
  git,
  jq,
  makeWrapper,
  ripgrep,
  writableTmpDirAsHomeHook,
}:

buildGoModule {
  pname = "workspace-sync";
  version = "0.1.0";

  src = ./.;
  vendorHash = null;

  # The go command writes telemetry under $HOME, which unsandboxed CI builds
  # would create as /homeless-shelter (#603).
  nativeBuildInputs = [
    writableTmpDirAsHomeHook
    makeWrapper
  ];
  nativeCheckInputs = [
    bash
    coreutils
    git
    jq
    ripgrep
  ];

  postInstall = ''
    wrapProgram $out/bin/workspace-sync \
      --prefix PATH : ${
        lib.makeBinPath [
          coreutils
          jq
        ]
      } \
      --suffix PATH : ${lib.makeBinPath [ bash ]}
  '';

  # Run the same parity suite against the installed Go binary. A separate
  # fixture checks the wrapper with a minimal PATH and incompatible coreutils.
  # Git comes from the consumer.
  doInstallCheck = true;
  nativeInstallCheckInputs = [
    bash
    coreutils
    git
    jq
    ripgrep
  ];
  installCheckPhase = ''
    runHook preInstallCheck
    WORKSPACE_SYNC_TEST_BINARY="$out/bin/.workspace-sync-wrapped" \
      WORKSPACE_SYNC_WRAPPED_BINARY="$out/bin/workspace-sync" \
      go test -count=1 ./...
    runHook postInstallCheck
  '';

  meta = {
    description = "Preserve workspace checkouts and reconcile missing directories and relative links";
    mainProgram = "workspace-sync";
    platforms = [
      "x86_64-linux"
      "aarch64-darwin"
    ];
  };
}
