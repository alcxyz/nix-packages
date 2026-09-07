#!/usr/bin/env bash
set -euo pipefail

clean_homeless_shelter() {
  local attempt

  for attempt in {1..10}; do
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

nix_build() {
  local attempt
  local status

  for attempt in 1 2 3; do
    clean_homeless_shelter
    if nix build "$@"; then
      return 0
    else
      status=$?
    fi

    if [[ ! -e /homeless-shelter || "$attempt" -eq 3 ]]; then
      return "$status"
    fi

    echo "Retrying Nix build after /homeless-shelter was recreated (attempt $((attempt + 1))/3)." >&2
  done
}

for attr in agent-sync-check forge-mirror nix-deploy zfs-auto-unlock devlog wcap; do
  echo "::group::nix build ${attr}"
  nix_build ".#${attr}"
  echo "::endgroup::"
done

base_ref="${GITHUB_BASE_REF:-${GITEA_BASE_REF:-${FORGEJO_BASE_REF:-}}}"
ref_name="${GITHUB_REF_NAME:-${GITEA_REF_NAME:-${FORGEJO_REF_NAME:-}}}"

if [ -n "$base_ref" ]; then
  git fetch origin "$base_ref"
  base="origin/${base_ref}"
elif [ "$ref_name" = "main" ]; then
  base="$(git rev-parse HEAD^)"
elif git rev-parse --verify origin/main >/dev/null 2>&1; then
  base="origin/main"
else
  base="$(git rev-parse HEAD^)"
fi

echo "Detecting changed packages against ${base}"

# Attribute names do not force derivations. Decide platform membership before
# evaluating drvPath so a broken Linux export cannot look like a Darwin-only one.
exports=$(nix eval .#packages --json --apply 'builtins.mapAttrs (_: builtins.attrNames)')
changed_files=$(git diff --name-only "${base}"...HEAD)
changed_attrs=()
full_matrix=false
while IFS= read -r path; do
  case "$path" in
    ""|docs/*|README.md|AGENTS.md|LICENSE*) ;;
    # These packages are also inputs to other packages in flake.nix.
    pkgs/claude-code/*|pkgs/codex-cli/*|pkgs/xonsh-direnv/*)
      full_matrix=true
      ;;
    pkgs/*/*|tools/*/*)
      attr=${path#*/}
      attr=${attr%%/*}
      # The overlay attribute uses an underscore in its version suffix.
      [[ "$attr" == openzfs-7_1 ]] && attr=openzfs_7_1
      changed_attrs+=("$attr")
      ;;
    *)
      # Root flake files and shared scripts/inputs can affect any package.
      full_matrix=true
      ;;
  esac
done <<<"$changed_files"

if "$full_matrix"; then
  echo "Shared inputs changed; validating the complete exported package matrix."
  selected=$(jq -r '[.[][]] | unique[]' <<<"$exports")
else
  selected=$(printf '%s\n' "${changed_attrs[@]}" | sort -u)
fi

while IFS= read -r attr; do
  [[ -z "$attr" ]] && continue
  echo "::group::changed package ${attr}"
  systems=$(jq -r --arg attr "$attr" 'to_entries[] | select(.value | index($attr)) | .key' <<<"$exports")
  if [[ -z "$systems" ]]; then
    if [[ -e "pkgs/${attr}" || -e "tools/${attr}" ]]; then
      echo "${attr} has source files but no exported package; check its flake mapping." >&2
      exit 1
    fi
    echo "${attr} has no remaining export; skipping removed package."
  fi
  while IFS= read -r system; do
    [[ -z "$system" ]] && continue
    package=".#packages.${system}.${attr}"
    echo "Evaluating ${package}.drvPath"
    nix eval "${package}.drvPath" >/dev/null
    if [[ "$system" == x86_64-linux ]]; then
      nix_build "$package"
    else
      echo "${system}: derivation evaluated only; no native build on this runner."
    fi
  done <<<"$systems"
  echo "::endgroup::"
done <<<"$selected"
