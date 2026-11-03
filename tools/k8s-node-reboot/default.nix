{
  lib,
  makeWrapper,
  nixfleet,
  stdenvNoCC,
}:
# kreboot, koff and kon are the names of nixfleet's Go node power command,
# k8s-node-power reboot, off and on (nix-packages#552). The package keeps
# the network audit helper, at libexec/k8s-node-network-audit and as
# passthru.networkAuditScript, which nix-config installs on each k3s node.
stdenvNoCC.mkDerivation {
  pname = "k8s-node-reboot";
  version = "1.0.0";

  dontUnpack = true;

  nativeBuildInputs = [ makeWrapper ];

  installPhase = ''
    install -Dm755 ${./scripts/ops/k8s-node-network-audit.sh} $out/libexec/k8s-node-network-audit
    for command in kreboot:reboot koff:off kon:on; do
      makeWrapper ${lib.getExe' nixfleet "k8s-node-power"} "$out/bin/''${command%%:*}" \
        --add-flags "''${command#*:}"
    done
  '';

  doInstallCheck = true;
  installCheckPhase = ''
    for command in kreboot koff kon; do
      "$out/bin/$command" --help | grep -q '^Usage: k8s-node-power reboot'
      "$out/bin/$command" --version | grep -q '^k8s-node-power '
    done
    test -x "$out/libexec/k8s-node-network-audit"
  '';

  passthru.networkAuditScript = ./scripts/ops/k8s-node-network-audit.sh;

  meta = {
    description = "kreboot, koff and kon: nixfleet's guarded Kubernetes node reboot, power off and power on";
    mainProgram = "kreboot";
    platforms = lib.platforms.unix;
  };
}
