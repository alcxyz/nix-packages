{ k3s, testers }:
let
  vip = "10.250.0.10";
  stable = "10.250.0.11";
in
testers.runNixOSTest {
  name = "k3s-flannel-stable-vxlan-source";

  nodes.machine = { pkgs, ... }: {
    system.stateVersion = "25.11";
    virtualisation.memorySize = 4096;
    virtualisation.cores = 2;
    virtualisation.diskSize = 4096;
    networking.firewall.enable = false;
    networking.interfaces.testunderlay.useDHCP = false;
    environment.systemPackages = [
      pkgs.iproute2
      pkgs.jq
      k3s
    ];

    # The first IPv4 address on the selected interface is deliberately the VIP.
    # This must complete before K3s starts on every boot.
    systemd.services.flannel-test-addresses = {
      description = "Assign VIP before stable node address for Flannel regression";
      wantedBy = [ "multi-user.target" ];
      before = [ "k3s.service" ];
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
      };
      script = ''
        ${pkgs.iproute2}/bin/ip link add testunderlay type dummy
        ${pkgs.iproute2}/bin/ip addr add ${vip}/24 dev testunderlay
        ${pkgs.iproute2}/bin/ip addr add ${stable}/24 dev testunderlay
        ${pkgs.iproute2}/bin/ip link set testunderlay up
      '';
    };
    systemd.services.k3s = {
      requires = [ "flannel-test-addresses.service" ];
      after = [ "flannel-test-addresses.service" ];
    };

    services.k3s = {
      enable = true;
      role = "server";
      package = k3s;
      extraFlags = [
        "--node-ip=${stable}"
        "--flannel-iface=testunderlay"
        "--disable=coredns"
        "--disable=local-storage"
        "--disable=metrics-server"
        "--disable=servicelb"
        "--disable=traefik"
      ];
    };
  };

  testScript = ''
    import json

    def verify(label):
        machine.wait_for_unit("k3s.service")
        machine.wait_until_succeeds("k3s kubectl get --raw=/readyz >/dev/null")
        addresses = machine.succeed("ip -o -4 addr show dev testunderlay")
        assert addresses.index("${vip}/24") < addresses.index("${stable}/24"), addresses

        machine.wait_until_succeeds("ip -d -j link show dev flannel.1 | jq -e '.[0].linkinfo.info_data.local != null' >/dev/null")
        machine.wait_until_succeeds("k3s kubectl get node machine -o json | jq -e '.metadata.annotations[\"flannel.alpha.coreos.com/public-ip\"] != null' >/dev/null")

        def check_flannel():
            links = json.loads(machine.succeed("ip -d -j link show dev flannel.1"))
            local = links[0]["linkinfo"]["info_data"].get("local")
            assert local == "${stable}", f"{label}: VXLAN local={local}, wanted ${stable}"

            node = json.loads(machine.succeed("k3s kubectl get node machine -o json"))
            public_ip = node["metadata"]["annotations"].get("flannel.alpha.coreos.com/public-ip")
            assert public_ip == "${stable}", f"{label}: node public-ip={public_ip}, wanted ${stable}"

        check_flannel()
        print(f"{label}: VIP first; flannel.1 local and public-ip annotation = ${stable}")

    machine.start(allow_reboot=True)
    verify("initial boot")
    first_pid = machine.succeed("systemctl show -p MainPID --value k3s.service").strip()
    machine.succeed("systemctl restart k3s.service")
    restarted_pid = machine.succeed("systemctl show -p MainPID --value k3s.service").strip()
    assert restarted_pid != first_pid and restarted_pid != "0", (first_pid, restarted_pid)
    verify("service restart")
    machine.reboot()
    verify("reboot")
  '';
}
