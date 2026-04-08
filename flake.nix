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
          claude-code = pkgs.callPackage ./pkgs/claude-code { };
        }
        // lib.optionalAttrs (isLinux || system == "aarch64-darwin") {
          t3code = pkgs.callPackage ./pkgs/t3code { };
        }
        // lib.optionalAttrs isLinux {
          ndrop = pkgs.callPackage ./pkgs/ndrop { };
          carapace = pkgs.callPackage ./pkgs/carapace { };
          carapace-bridge = pkgs.callPackage ./pkgs/carapace-bridge { };
          zfs-auto-unlock = pkgs.callPackage ./tools/zfs-auto-unlock { };
          paperless-review = pkgs.callPackage ./tools/paperless-review { };
          paperless-filetype-index = pkgs.callPackage ./tools/paperless-filetype-index { };
        };
      in
      {
        packages = allPackages;
        defaultPackage = allPackages.helium or allPackages.pihole-sync;
      }
    );
}
