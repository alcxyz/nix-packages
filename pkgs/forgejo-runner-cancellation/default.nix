{
  fetchFromGitea,
  forgejo-runner,
  gitMinimal,
  lib,
  writableTmpDirAsHomeHook,
}:
forgejo-runner.overrideAttrs (old: {
  pname = "forgejo-runner-cancellation";
  version = "13.2.0";
  src = fetchFromGitea {
    domain = "code.forgejo.org";
    owner = "forgejo";
    repo = "runner";
    rev = "v13.2.0";
    hash = "sha256-2P3lWzC3yxcyiBltFDC1dwwv8Xx2XxEGSa16MdjnV38=";
  };
  vendorHash = "sha256-iyXO3LYTr4v1OoD9PS5ksEDIkCIq8hgwgtkLR0bzUZg=";
  ldflags = [
    "-s"
    "-w"
    "-X code.forgejo.org/forgejo/runner/v13/internal/pkg/ver.version=v13.2.0"
  ];
  patches = (old.patches or [ ]) ++ [ ./cancellation-cleanup.patch ];
  checkFlags = [
    "-skip ${
      lib.concatStringsSep "|" [
        "TestHandler"
        "TestClone"
        "TestRunner_ReusableWorkflowGitHubInstance"
        "TestInitRepoIfRequired/clone"
        "TestInitRepoIfRequired/clone_different_remote"
      ]
    }"
  ];
  preCheck = ''
    substituteInPlace testutils/test_main.go \
      --replace-fail 'TestFeatureDocker: {},' '// TestFeatureDocker: {},' \
      --replace-fail 'TestFeatureLXC:    {},' '// TestFeatureLXC:    {},'
  '';
  # The go command writes telemetry under $HOME. Building, not only checking,
  # needs a writable one: buildGoModule passes nativeBuildInputs on to the
  # module download, which unsandboxed CI builds would otherwise run with
  # /homeless-shelter (#603).
  nativeBuildInputs = (old.nativeBuildInputs or [ ]) ++ [ writableTmpDirAsHomeHook ];
  nativeCheckInputs = [ gitMinimal ];
})
