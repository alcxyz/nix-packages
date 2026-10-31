{
  acl,
  bash,
  buildGoModule,
  coreutils,
  git,
  hostname,
  kubectl-cnpg,
  lib,
  makeWrapper,
  openssh,
  python3,
  stdenv,
  writableTmpDirAsHomeHook,
}:
let
  deployPath = lib.makeBinPath [
    bash
    coreutils
    hostname
    openssh
  ];
in
buildGoModule (finalAttrs: {
  pname = "nixfleet";
  # Bump with every change to the tool and record it in CHANGELOG.md.
  version = "0.25.0";

  src = ./.;

  vendorHash = null; # no external dependencies

  ldflags = [
    "-X main.version=${finalAttrs.version}"
    "-X main.reportACLInspector=${coreutils}/bin/ls"
  ];

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
    python3
  ]
  ++ lib.optionals stdenv.hostPlatform.isLinux [ acl ];

  # One program answers to nixfleet, deploy and k8s-node-power, by the name it
  # is invoked as. The black-box contract mocks deployment commands, so it
  # runs against the unwrapped binary, under those names, before the runtime
  # PATH is added (ADR-0006).
  postCheck = ''
    names=$(mktemp -d)
    for name in nixfleet deploy k8s-node-power; do
      ln -s "$GOPATH/bin/nix-deploy" "$names/$name"
    done
    python3 ${./test-deploy.py} "$names/deploy"
    [ "$("$names/deploy" --version)" = "deploy ${finalAttrs.version}" ]
    [ "$("$names/k8s-node-power" --version)" = "k8s-node-power ${finalAttrs.version}" ]
    [ "$("$names/nixfleet" --version)" = "nixfleet ${finalAttrs.version}" ]
  '';

  # Each name is a wrapper that sets argv[0], so the program knows which
  # command it is wherever a consumer links or wraps it. deploy gets its
  # runtime commands. k8s-node-power and nixfleet, which serves both, also
  # get the network audit helper that the k8s-node-reboot package ships on
  # every node over SSH and kubectl-cnpg; kubectl comes from the operator's
  # environment, as for kreboot. All three use deploy's search path, so they
  # run the same ssh. kreboot, koff and kon
  # stay with the k8s-node-reboot package until the Go port replaces it.
  postInstall = ''
    mkdir -p $out/libexec
    mv $out/bin/nix-deploy $out/libexec/nixfleet
    # nixfleet triage finds its playbook at <exe dir>/../share/nixfleet.
    install -Dm644 ${./triage-playbook.md} $out/share/nixfleet/triage-playbook.md
    install -Dm755 ${../k8s-node-reboot/scripts/ops/k8s-node-network-audit.sh} \
      $out/libexec/k8s-node-network-audit
    makeWrapper $out/libexec/nixfleet $out/bin/deploy \
      --argv0 deploy \
      --prefix PATH : ${deployPath}
    makeWrapper $out/libexec/nixfleet $out/bin/k8s-node-power \
      --argv0 k8s-node-power \
      --set K8S_NODE_NETWORK_AUDIT_SCRIPT $out/libexec/k8s-node-network-audit \
      --prefix PATH : ${deployPath} \
      --prefix PATH : ${lib.makeBinPath [ kubectl-cnpg ]}
    makeWrapper $out/libexec/nixfleet $out/bin/nixfleet \
      --argv0 nixfleet \
      --set K8S_NODE_NETWORK_AUDIT_SCRIPT $out/libexec/k8s-node-network-audit \
      --prefix PATH : ${deployPath} \
      --prefix PATH : ${lib.makeBinPath [ kubectl-cnpg ]}
  '';

  meta = with lib; {
    description = "Inventory-driven NixOS/darwin and Home Manager deploy, node power and fleet status tool";
    mainProgram = "nixfleet";
    platforms = platforms.unix;
  };
})
