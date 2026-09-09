#!/usr/bin/env bash
# Shared workaround for the explicitly opted-in ephemeral CI runner.
clean_homeless_shelter() {
  local _attempt

  # Only the ephemeral CI runner opts into this workaround. Local invocations
  # must never remove another process's home directory.
  if [[ "${T3CODE_CI_CLEAN_HOME:-false}" != true ]]; then
    return 0
  fi
  if [[ "${GITHUB_ACTIONS:-false}" != true || -L /homeless-shelter ]]; then
    echo "Refusing CI home cleanup outside an Actions runner." >&2
    return 1
  fi

  for _attempt in {1..10}; do
    rm -rf /homeless-shelter
    sleep 1
    if [[ ! -e /homeless-shelter ]]; then
      sleep 1
      [[ ! -e /homeless-shelter ]] && return 0
    fi
  done

  echo "Unable to keep /homeless-shelter absent before a non-sandboxed Nix build." >&2
  return 1
}
