{
  fetchFromGitea,
  forgejo-runner,
  gitMinimal,
  lib,
  writableTmpDirAsHomeHook,
}:
forgejo-runner.overrideAttrs (old: {
  pname = "forgejo-runner-cancellation";
  version = "13.2.1-alc.1";
  src = fetchFromGitea {
    domain = "git.alc.xyz";
    owner = "alcxyz";
    repo = "forgejo-runner";
    rev = "v13.2.1-alc.1";
    hash = "sha256-DbZ3hXgldWzXiMkxMxBS1HVSO+FK01TXchmIEbxqUqc=";
  };
  vendorHash = "sha256-6ao0x5rPZmyrvvGVLTSTjtrV3UbTqeUeV0OpHDqriHc=";
  meta = (old.meta or { }) // {
    homepage = "https://git.alc.xyz/alcxyz/forgejo-runner";
    changelog = "https://git.alc.xyz/alcxyz/forgejo-runner/releases/tag/v13.2.1-alc.1";
  };
  ldflags = [
    "-s"
    "-w"
    "-X code.forgejo.org/forgejo/runner/v13/internal/pkg/ver.version=v13.2.1-alc.1"
  ];
  # Cancellation cleanup and daemon pause/drain support live in the maintained
  # source distribution (ADR-0017); keep inherited packaging patches only.
  patches = old.patches or [ ];
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
