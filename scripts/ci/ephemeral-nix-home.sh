#!/usr/bin/env bash
# Remove Nix's dummy home only in an explicitly declared ephemeral CI container.

nix_ci_homeless_shelter=/homeless-shelter
nix_ci_container_marker=/.dockerenv

clean_ephemeral_nix_home() {
  if [[ ! -e "$nix_ci_homeless_shelter" ||
        "${NIX_CI_EPHEMERAL_CONTAINER:-0}" != "1" ||
        ! -e "$nix_ci_container_marker" ]]; then
    return 0
  fi

  rm --recursive --force --one-file-system -- "$nix_ci_homeless_shelter"
}
