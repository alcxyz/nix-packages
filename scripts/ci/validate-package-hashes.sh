#!/usr/bin/env bash
set -euo pipefail

if grep -RInE 'hash = "sha256-";|hash = "sha256-"[[:space:]]*;' pkgs tools; then
  echo "Found an empty SRI hash. Updater scripts must write complete sha256-<base64> hashes." >&2
  exit 1
fi
