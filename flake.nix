{
  description = "Custom packages and flake inputs";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-25.05";
    nixpkgs-unstable.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs =
    {
      self,
      nixpkgs,
      nixpkgs-unstable,
      flake-utils,
    }:
    let
      openzfs71Overlay =
        final: prev:
        prev.lib.optionalAttrs prev.stdenv.hostPlatform.isLinux {
          openzfs_7_1 = final.callPackage ./pkgs/openzfs-7_1 {
            nixpkgsPath = final.path;
          };
        };
    in
    {
      overlays.openzfs-7-1 = openzfs71Overlay;
    }
    // flake-utils.lib.eachDefaultSystem (
      system:
      let
        pkgs = import nixpkgs {
          inherit system;
          config.allowUnfree = true;
          overlays = [ openzfs71Overlay ];
        };
        unstablePkgs = import nixpkgs-unstable {
          inherit system;
          config.allowUnfree = true;
        };
        lib = pkgs.lib;
        isLinux = pkgs.stdenv.hostPlatform.isLinux;

        # All packages — some are platform-specific
        allPackages = {
          agent-sync-check = pkgs.callPackage ./tools/agent-sync-check { };
          forge-mirror = pkgs.callPackage ./tools/forge-mirror { };
          kdash = pkgs.callPackage ./pkgs/kdash { };
          claude-code = pkgs.callPackage ./pkgs/claude-code { };
          codex-app-server = pkgs.callPackage ./pkgs/codex-app-server { };
          codex-cli = pkgs.callPackage ./pkgs/codex-cli { };
          k8s-node-reboot = pkgs.callPackage ./tools/k8s-node-reboot { };
          nix-deploy = pkgs.callPackage ./tools/nix-deploy { };
          nix-gc-maintenance = pkgs.callPackage ./tools/nix-gc-maintenance { };
          xonsh-direnv = pkgs.callPackage ./pkgs/xonsh-direnv { };
          xonsh-with-direnv = pkgs.callPackage ./pkgs/xonsh-with-direnv {
            inherit (allPackages) xonsh-direnv;
          };
        }
        //
          lib.optionalAttrs
            (builtins.elem system [
              "x86_64-linux"
              "aarch64-linux"
              "aarch64-darwin"
            ])
            {
              # The pinned unstable Darwin SDK dependency does not support Intel.
              herdr = unstablePkgs.callPackage ./pkgs/herdr { };
            }
        //
          lib.optionalAttrs
            (builtins.elem system [
              "x86_64-linux"
              "aarch64-darwin"
              "x86_64-darwin"
            ])
            {
              # ARM Linux has no verified asset; do not advertise its placeholder.
              helium = pkgs.callPackage ./pkgs/helium { };
            }
        // lib.optionalAttrs pkgs.stdenv.hostPlatform.isDarwin {
          ghostty = pkgs.callPackage ./pkgs/ghostty { };
        }
        // lib.optionalAttrs (system == "x86_64-linux" || system == "aarch64-darwin") {
          t3code = unstablePkgs.callPackage ./pkgs/t3code {
            inherit (allPackages) claude-code codex-cli;
          };
        }
        // lib.optionalAttrs (system == "aarch64-darwin") {
          omniwm = pkgs.callPackage ./pkgs/omniwm { };
        }
        // lib.optionalAttrs (system == "x86_64-linux") {
          ledger-live = pkgs.callPackage ./pkgs/ledger-live { };
          zen-browser = pkgs.callPackage ./pkgs/zen-browser { };
          wcap = pkgs.callPackage ./tools/wcap { };
        }
        // lib.optionalAttrs isLinux {
          jean = pkgs.callPackage ./pkgs/jean { };
          ndrop = pkgs.callPackage ./pkgs/ndrop { };
          inherit (pkgs) openzfs_7_1;
          stash = pkgs.callPackage ./pkgs/stash { };
          zfs-auto-unlock = pkgs.callPackage ./tools/zfs-auto-unlock { };
          devlog = pkgs.callPackage ./tools/devlog { };
        };
      in
      {
        packages =
          allPackages
          // lib.optionalAttrs (allPackages ? helium) {
            default = allPackages.helium;
          };
        checks.nix-gc-maintenance-contract =
          pkgs.runCommand "nix-gc-maintenance-contract"
            {
              nativeBuildInputs = [
                pkgs.bash
                pkgs.coreutils
                pkgs.diffutils
                pkgs.findutils
                pkgs.gawk
                pkgs.gnugrep
                pkgs.shellcheck
              ];
              source = lib.cleanSource ./tools/nix-gc-maintenance;
            }
            ''
              shellcheck "$source/nix-gc-maintenance.sh" "$source/test"
              MAINTENANCE="$source/nix-gc-maintenance.sh" bash "$source/test"
              touch "$out"
            '';
        checks.k8s-node-reboot-contract =
          pkgs.runCommand "k8s-node-reboot-contract"
            {
              nativeBuildInputs = [
                pkgs.bash
                pkgs.coreutils
                pkgs.gnugrep
                pkgs.gnused
                pkgs.jq
                pkgs.shellcheck
              ];
              source = lib.cleanSource ./tools/k8s-node-reboot;
            }
            ''
              cd "$source"
              shellcheck scripts/ops/*.sh scripts/checks/*.sh
              bash scripts/checks/test-k8s-node-reboot-workload-phases.sh
              bash scripts/checks/test-k8s-node-reboot-network-audits.sh
              bash scripts/checks/test-k8s-node-network-audit.sh
              touch "$out"
            '';
        checks.nix-deploy-contract =
          pkgs.runCommand "nix-deploy-contract"
            {
              nativeBuildInputs = with pkgs; [
                bash
                coreutils
                gawk
                gnugrep
                gnused
                jq
                python3
                shellcheck
              ];
            }
            ''
              shellcheck ${./tools/nix-deploy/deploy}
              python3 ${./scripts/ci/test-nix-deploy.py} ${./tools/nix-deploy/deploy}
              touch "$out"
            '';
      }
    );
}
