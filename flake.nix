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
    flake-utils.lib.eachDefaultSystem (
      system:
      let
        pkgs = import nixpkgs {
          inherit system;
          config.allowUnfree = true;
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
          helium = pkgs.callPackage ./pkgs/helium { };
          kdash = pkgs.callPackage ./pkgs/kdash { };
          claude-code = pkgs.callPackage ./pkgs/claude-code { };
          codex-app-server = pkgs.callPackage ./pkgs/codex-app-server { };
          codex-cli = pkgs.callPackage ./pkgs/codex-cli { };
          nix-deploy = pkgs.callPackage ./tools/nix-deploy { };
          xonsh-direnv = pkgs.callPackage ./pkgs/xonsh-direnv { };
          xonsh-with-direnv = pkgs.callPackage ./pkgs/xonsh-with-direnv {
            inherit (allPackages) xonsh-direnv;
          };
        }
        // lib.optionalAttrs pkgs.stdenv.hostPlatform.isDarwin {
          ghostty = pkgs.callPackage ./pkgs/ghostty { };
          paneru = unstablePkgs.callPackage ./pkgs/paneru { };
        }
        // lib.optionalAttrs (system == "x86_64-linux" || system == "aarch64-darwin") {
          t3code = pkgs.callPackage ./pkgs/t3code { };
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
          ndrop = pkgs.callPackage ./pkgs/ndrop { };
          stash = pkgs.callPackage ./pkgs/stash { };
          zfs-auto-unlock = pkgs.callPackage ./tools/zfs-auto-unlock { };
          devlog = pkgs.callPackage ./tools/devlog { };
        };
      in
      {
        packages = allPackages;
        defaultPackage = allPackages.helium;
      }
    );
}
