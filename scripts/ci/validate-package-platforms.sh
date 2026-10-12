#!/usr/bin/env bash
set -euo pipefail

# Force every advertised derivation on every exported system. This evaluates foreign-platform outputs; it does not build them.
nix eval .#packages --json --apply '
  builtins.mapAttrs (_: builtins.mapAttrs (_: package: package.drvPath))
' >/dev/null

echo 'All advertised package derivations evaluated.'
