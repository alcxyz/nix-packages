#!/usr/bin/env bash
# Shared workaround for the explicitly opted-in ephemeral CI runner.
# shellcheck source=scripts/ci/ephemeral-nix-home.sh
source "$(dirname "${BASH_SOURCE[0]}")/ephemeral-nix-home.sh"

clean_homeless_shelter() {
  local _attempt

  # Only the ephemeral CI runner opts into this workaround. Local invocations
  # must never remove another process's home directory.
  if [[ "${T3CODE_CI_CLEAN_HOME:-false}" != true ]]; then
    return 0
  fi
  if [[ "${NIX_CI_EPHEMERAL_CONTAINER:-0}" != "1" ||
        ! -e "$nix_ci_container_marker" ||
        -L "$nix_ci_homeless_shelter" ]]; then
    echo "Refusing T3 Code home cleanup outside the declared ephemeral CI container." >&2
    return 1
  fi

  for _attempt in {1..10}; do
    clean_ephemeral_nix_home
    sleep 1
    if [[ ! -e "$nix_ci_homeless_shelter" ]]; then
      sleep 1
      [[ ! -e "$nix_ci_homeless_shelter" ]] && return 0
    fi
  done

  echo "Unable to keep ${nix_ci_homeless_shelter} absent before a non-sandboxed Nix build." >&2
  return 1
}
