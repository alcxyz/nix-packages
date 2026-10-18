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
          forge-mirror = pkgs.callPackage ./tools/forge-mirror { };
          gh-app = pkgs.callPackage ./tools/gh-app { };
          # gh routed through gh-app.
          gh-app-gh = pkgs.callPackage ./tools/gh-app/gh.nix {
            inherit (allPackages) gh-app;
          };
          git-identity-guard = pkgs.callPackage ./tools/git-identity-guard { };
          kdash = pkgs.callPackage ./pkgs/kdash { };
          claude-code = pkgs.callPackage ./pkgs/claude-code { };
          codex-app-server = pkgs.callPackage ./pkgs/codex-app-server { };
          codex-cli = pkgs.callPackage ./pkgs/codex-cli { };
          k8s-node-reboot = pkgs.callPackage ./tools/k8s-node-reboot { };
          nixfleet = pkgs.callPackage ./tools/nix-deploy { };
          # The same package under its earlier name, whose main program stays
          # deploy, so `nix run .#nix-deploy` keeps its command line.
          nix-deploy = lib.addMetaAttrs { mainProgram = "deploy"; } allPackages.nixfleet;
          nix-gc-maintenance = pkgs.callPackage ./tools/nix-gc-maintenance { };
          revkit = pkgs.callPackage ./tools/revkit { };
          workmux = pkgs.callPackage ./pkgs/workmux { };
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
            ])
            {
              workspace-sync = pkgs.callPackage ./tools/workspace-sync { };
            }
        // lib.optionalAttrs pkgs.stdenv.hostPlatform.isDarwin {
          ghostty = pkgs.callPackage ./pkgs/ghostty { };
        }
        // lib.optionalAttrs (system == "x86_64-linux") {
          t3code = unstablePkgs.callPackage ./pkgs/t3code {
            inherit (allPackages) claude-code codex-cli;
            # Keep the unstable gh that t3code wrapped before, which the
            # exported gh-app-gh cannot use: unstable has no Intel Darwin gh.
            gh-app-gh = allPackages.gh-app-gh.override { inherit (unstablePkgs) gh; };
          };
          # Profile-installable AI stack, so consumers can update T3 and its
          # providers without rebuilding anything else.
          ai-stack-upstream = pkgs.buildEnv {
            name = "ai-stack-upstream";
            paths = [
              allPackages.t3code
              allPackages.claude-code
              allPackages.codex-cli
              allPackages.codex-app-server
            ];
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
          forgejo-runner-cancellation = unstablePkgs.callPackage ./pkgs/forgejo-runner-cancellation { };
          forgejo-runner-orphan-check = pkgs.callPackage ./tools/forgejo-runner-orphan-check { };
          ndrop = pkgs.callPackage ./pkgs/ndrop { };
          inherit (pkgs) openzfs_7_1;
          stash = pkgs.callPackage ./pkgs/stash { };
          zfs-auto-unlock = pkgs.callPackage ./tools/zfs-auto-unlock { };
          devlog = pkgs.callPackage ./tools/devlog { };
        };
      in
      {
        packages = allPackages;
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
                pkgs.ripgrep
                pkgs.shellcheck
              ];
              source = lib.cleanSource ./tools/k8s-node-reboot;
            }
            ''
              cd "$source"
              shellcheck scripts/ops/*.sh scripts/checks/*.sh
              bash scripts/checks/test-k8s-node-reboot-workload-phases.sh
              bash scripts/checks/test-k8s-node-reboot-fence-gates.sh
              bash scripts/checks/test-k8s-node-reboot-network-audits.sh
              bash scripts/checks/test-k8s-node-network-audit.sh
              touch "$out"
            '';
        # The package build runs the Go tests and the black-box contract
        # against the unwrapped binary.
        checks.nix-deploy-contract = allPackages.nix-deploy;
        # The package build runs revkit's Go tests, with fake reviewer clients.
        checks.revkit = allPackages.revkit;
        # The package build runs gh-app's Go tests, with a fake GitHub API.
        checks.gh-app = allPackages.gh-app;
        # Unconfigured, the wrapped gh is plain gh.
        checks.gh-app-gh = pkgs.runCommand "gh-app-gh-passthrough" { } ''
          export HOME="$TMPDIR"
          ${lib.getExe allPackages.gh-app-gh} --version | grep -q '^gh version'
          touch "$out"
        '';
        checks.${if allPackages ? workspace-sync then "workspace-sync-contract" else null} =
          allPackages.workspace-sync;
        checks.git-identity-guard-contract =
          pkgs.runCommand "git-identity-guard-contract"
            {
              nativeBuildInputs = [
                pkgs.git
                pkgs.python3
              ];
            }
            ''
              python3 ${./tools/git-identity-guard/test_git_identity_guard.py} \
                ${allPackages.git-identity-guard}/share/git-identity-guard/hooks
              touch "$out"
            '';
      }
    );
}
