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
      in
      {
        packages = {
          ndrop = pkgs.callPackage ./pkgs/ndrop { };
          carapace = pkgs.callPackage ./pkgs/carapace { };
          carapace-bridge = pkgs.callPackage ./pkgs/carapace-bridge { };
          zfs-auto-unlock = pkgs.callPackage ./tools/zfs-auto-unlock { };
          pihole-sync = pkgs.callPackage ./tools/pihole-sync { };
          helium = pkgs.callPackage ./pkgs/helium { };
          t3code = pkgs.callPackage ./pkgs/t3code { };
          claude-code = pkgs.callPackage ./pkgs/claude-code { };
        };

        defaultPackage = self.packages.${system}.ndrop;
      }
    );
}
