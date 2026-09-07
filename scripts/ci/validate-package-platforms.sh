#!/usr/bin/env bash
set -euo pipefail

# Force every advertised derivation, including defaults, on every exported
# system. This evaluates foreign-platform outputs; it does not build them.
nix eval .#packages --json --apply '
  builtins.mapAttrs (_: builtins.mapAttrs (_: package: package.drvPath))
' >/dev/null

# The optional ARM Linux Helium asset still has a placeholder hash. Until it is
# verified and explicitly enabled, it must not become a named or default export.
nix eval .#packages.aarch64-linux --json --apply builtins.attrNames \
  | jq -e 'index("helium") == null and index("default") == null' >/dev/null

echo 'All advertised package derivations evaluated; optional Helium platform remains unexported.'
