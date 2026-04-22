{
  description = "Custom packages and flake inputs";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-25.05";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs {
          inherit system;
          config.allowUnfree = true;
        };
        lib = pkgs.lib;
        isLinux = pkgs.stdenv.hostPlatform.isLinux;

        # All packages — some are platform-specific
        allPackages = {
          pihole-sync = pkgs.callPackage ./tools/pihole-sync { };
          helium = pkgs.callPackage ./pkgs/helium { };
          ghostty = pkgs.callPackage ./pkgs/ghostty { };
          claude-code = pkgs.callPackage ./pkgs/claude-code { };
        }
        // lib.optionalAttrs (isLinux || system == "aarch64-darwin") {
          t3code = pkgs.callPackage ./pkgs/t3code { };
        }
        // lib.optionalAttrs (system == "aarch64-darwin") {
          omniwm = pkgs.callPackage ./pkgs/omniwm { };
        }
        // lib.optionalAttrs isLinux {
          ledger-live = pkgs.callPackage ./pkgs/ledger-live { };
          ndrop = pkgs.callPackage ./pkgs/ndrop { };
          zen-browser = pkgs.callPackage ./pkgs/zen-browser { };
          zfs-auto-unlock = pkgs.callPackage ./tools/zfs-auto-unlock { };
          leantime-tidy = pkgs.callPackage ./tools/leantime-tidy { };
        };
      in
      {
        packages = allPackages;
        defaultPackage = allPackages.helium or allPackages.pihole-sync;
      }
    );
}
