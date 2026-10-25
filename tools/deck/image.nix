# Container image of deck's pages-only mode (ADR-0013), for a cluster without a
# Nix toolchain. The configuration is mounted at /etc/deck/config.toml and
# names the content directory, which a Git sync sidecar fills; the image
# writes nothing, so it runs as non-root on a read-only root filesystem.
{
  deck,
  dockerTools,
  lib,
}:
dockerTools.buildLayeredImage {
  name = "deck";
  tag = deck.version;

  # The CA bundle lets deck fetch its issuer's OIDC discovery document.
  contents = [ dockerTools.caCertificates ];

  config = {
    Entrypoint = [
      (lib.getExe deck)
      "serve"
      "-config"
      "/etc/deck/config.toml"
    ];
    User = "65532:65532";
    Env = [
      "HOME=/nonexistent"
      "SSL_CERT_FILE=/etc/ssl/certs/ca-bundle.crt"
    ];
    ExposedPorts."3775/tcp" = { };
    Labels = {
      "org.opencontainers.image.title" = "deck";
      "org.opencontainers.image.description" = "deck in pages-only mode";
      "org.opencontainers.image.version" = deck.version;
      "org.opencontainers.image.source" = "https://git.alc.xyz/alcxyz/nix-packages";
    };
  };
}
