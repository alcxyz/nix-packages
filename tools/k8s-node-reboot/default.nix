{
  jq,
  kubectl-cnpg,
  lib,
  makeWrapper,
  stdenv,
}:
stdenv.mkDerivation {
  pname = "k8s-node-reboot";
  version = "0.7.4";

  src = ./scripts/ops;

  dontBuild = true;

  nativeBuildInputs = [ makeWrapper ];

  installPhase = ''
    install -Dm755 k8s-node-reboot.sh $out/libexec/k8s-node-reboot
    install -Dm755 k8s-node-network-audit.sh $out/libexec/k8s-node-network-audit
    makeWrapper $out/libexec/k8s-node-reboot $out/bin/kreboot \
      --set K8S_NODE_POWER_ACTION reboot \
      --set K8S_NODE_NETWORK_AUDIT_SCRIPT $out/libexec/k8s-node-network-audit \
      --prefix PATH : ${
        lib.makeBinPath [
          jq
          kubectl-cnpg
        ]
      }
    makeWrapper $out/libexec/k8s-node-reboot $out/bin/koff \
      --set K8S_NODE_POWER_ACTION off \
      --set K8S_NODE_NETWORK_AUDIT_SCRIPT $out/libexec/k8s-node-network-audit \
      --prefix PATH : ${
        lib.makeBinPath [
          jq
          kubectl-cnpg
        ]
      }
    makeWrapper $out/libexec/k8s-node-reboot $out/bin/kon \
      --set K8S_NODE_POWER_ACTION on \
      --set K8S_NODE_NETWORK_AUDIT_SCRIPT $out/libexec/k8s-node-network-audit \
      --prefix PATH : ${
        lib.makeBinPath [
          jq
          kubectl-cnpg
        ]
      }
  '';

  passthru.networkAuditScript = ./scripts/ops/k8s-node-network-audit.sh;

  meta = with lib; {
    description = "Cordon, drain, reboot, and uncordon a Kubernetes node";
    mainProgram = "kreboot";
    platforms = platforms.unix;
  };
}
