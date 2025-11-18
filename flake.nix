{
  description = "Custom packages and flake inputs";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-25.05";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs { inherit system; };
      in
      {
        packages = {
          ndrop = pkgs.callPackage ./pkgs/ndrop { };
          # Add more custom packages here
          # other-app = pkgs.callPackage ./pkgs/other-app { };
        };

        defaultPackage = self.packages.${system}.ndrop;
      }
    );
}
