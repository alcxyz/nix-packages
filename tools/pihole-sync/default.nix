{ lib, buildGoModule }:

buildGoModule {
  pname = "pihole-sync";
  version = "0.1.0";

  src = ./.;

  # This project has external dependencies (github.com/BurntSushi/toml),
  # so we use Go module vendoring. First build will tell us the hash.
  vendorHash = "sha256-CVycV7wxo7nOHm7qjZKfJrIkNcIApUNzN1mSIIwQN0g=";
}
