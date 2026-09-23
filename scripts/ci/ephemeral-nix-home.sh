#!/usr/bin/env bash
# Remove Nix's dummy home only in an explicitly declared ephemeral CI container.

nix_ci_homeless_shelter=/homeless-shelter
nix_ci_container_marker=/.dockerenv
nix_ci_podman_container_marker=/run/.containerenv

nix_ci_container_marker_present() {
  [[ -e "$nix_ci_container_marker" || -e "$nix_ci_podman_container_marker" ]]
}

clean_ephemeral_nix_home() {
  if [[ ! -e "$nix_ci_homeless_shelter" ||
        "${NIX_CI_EPHEMERAL_CONTAINER:-0}" != "1" ]] ||
      ! nix_ci_container_marker_present; then
    return 0
  fi

  rm --recursive --force --one-file-system -- "$nix_ci_homeless_shelter"
}
