{
  bash,
  buildGoModule,
  coreutils,
  hostname,
  kubectl-cnpg,
  lib,
  makeWrapper,
  openssh,
  python3,
}:
buildGoModule (finalAttrs: {
  pname = "nix-deploy";
  # Bump with every change to the tool and record it in CHANGELOG.md.
  version = "0.9.0";

  src = ./.;

  vendorHash = null; # no external dependencies

  ldflags = [ "-X main.version=${finalAttrs.version}" ];

  nativeBuildInputs = [ makeWrapper ];
  nativeCheckInputs = [
    bash
    python3
  ];

  # The black-box contract mocks every external command, so it runs against
  # the unwrapped binary before the runtime PATH is added (ADR-0006).
  postCheck = ''
    python3 ${./test-deploy.py} "$GOPATH/bin/nix-deploy"
    [ "$("$GOPATH/bin/nix-deploy" --version)" = "deploy ${finalAttrs.version}" ]
    [ "$("$GOPATH/bin/k8s-node-power" --version)" = "k8s-node-power ${finalAttrs.version}" ]
  '';

  # k8s-node-power, the Go port of k8s-node-reboot (#548), runs the network
  # audit helper that package ships on every node over SSH. kubectl comes from
  # the operator's environment, as for kreboot; ssh falls back to OpenSSH.
  postInstall = ''
    mv $out/bin/nix-deploy $out/bin/deploy
    wrapProgram $out/bin/deploy \
      --prefix PATH : ${
        lib.makeBinPath [
          bash
          coreutils
          hostname
          openssh
        ]
      }
    install -Dm755 ${../k8s-node-reboot/scripts/ops/k8s-node-network-audit.sh} \
      $out/libexec/k8s-node-network-audit
    wrapProgram $out/bin/k8s-node-power \
      --set K8S_NODE_NETWORK_AUDIT_SCRIPT $out/libexec/k8s-node-network-audit \
      --prefix PATH : ${lib.makeBinPath [ kubectl-cnpg ]} \
      --suffix PATH : ${lib.makeBinPath [ openssh ]}
  '';

  meta = with lib; {
    description = "Inventory-driven NixOS/darwin and Home Manager deploy tool";
    mainProgram = "deploy";
    platforms = platforms.unix;
  };
})
