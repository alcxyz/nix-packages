#!/usr/bin/env bash
set -euo pipefail

mode="${PACKAGE_BUILD_MODE:-selected}"
shard_count="${PACKAGE_BUILD_SHARD_COUNT:-1}"
shard_index="${PACKAGE_BUILD_SHARD_INDEX:-0}"
plan_only="${PACKAGE_BUILD_PLAN_ONLY:-0}"
plan_file="${PACKAGE_BUILD_PLAN_FILE:-}"
baseline_attrs=(agent-sync-check forge-mirror nix-deploy zfs-auto-unlock devlog wcap)

case "$mode" in
  all|baseline|selected) ;;
  *)
    echo "PACKAGE_BUILD_MODE must be all, baseline, or selected (got: ${mode})." >&2
    exit 2
    ;;
esac

if [[ ! "$shard_count" =~ ^([1-9]|[1-5][0-9]|6[0-4])$ ]]; then
  echo "PACKAGE_BUILD_SHARD_COUNT must be a canonical integer from 1 to 64 (got: ${shard_count})." >&2
  exit 2
fi
if [[ ! "$shard_index" =~ ^(0|[1-9]|[1-5][0-9]|6[0-3])$ ]] || ((shard_index >= shard_count)); then
  echo "PACKAGE_BUILD_SHARD_INDEX must be between 0 and $((shard_count - 1)) (got: ${shard_index})." >&2
  exit 2
fi
if [[ "$mode" != selected && ("$shard_count" != 1 || "$shard_index" != 0) ]]; then
  echo "Package shards are only valid in selected mode." >&2
  exit 2
fi
if [[ "$plan_only" != 0 && "$plan_only" != 1 ]]; then
  echo "PACKAGE_BUILD_PLAN_ONLY must be 0 or 1 (got: ${plan_only})." >&2
  exit 2
fi
if [[ "$plan_only" == 1 && -z "$plan_file" ]]; then
  echo "PACKAGE_BUILD_PLAN_FILE is required in plan-only mode." >&2
  exit 2
fi

if [[ "$plan_only" == 1 && "$mode" == baseline ]]; then
  printf '%s\n' "${baseline_attrs[@]}" >"$plan_file"
  exit 0
fi

homeless_shelter=/homeless-shelter
container_marker=/.dockerenv
podman_container_marker=/run/.containerenv
can_clean_homeless_shelter=false
if [[ "${NIX_CI_EPHEMERAL_CONTAINER:-0}" == "1" &&
      ( -e "$container_marker" || -e "$podman_container_marker" ) ]]; then
  can_clean_homeless_shelter=true
fi

clean_homeless_shelter() {
  local attempt

  if [[ ! -e "$homeless_shelter" || "$can_clean_homeless_shelter" != true ]]; then
    return 0
  fi

  for attempt in {1..10}; do
    rm --recursive --force --one-file-system -- "$homeless_shelter"
    sleep 1
    if [[ ! -e "$homeless_shelter" ]]; then
      sleep 1
      [[ ! -e "$homeless_shelter" ]] && return 0
    fi
  done

  echo "Unable to keep ${homeless_shelter} absent before a non-sandboxed Nix build." >&2
  return 1
}

nix_build() {
  local attempt
  local status

  for attempt in 1 2 3; do
    clean_homeless_shelter
    if nix build "$@" -L; then
      return 0
    else
      status=$?
    fi

    if [[ "$can_clean_homeless_shelter" != true || ! -e "$homeless_shelter" || "$attempt" -eq 3 ]]; then
      return "$status"
    fi

    echo "Retrying Nix build after ${homeless_shelter} was recreated (attempt $((attempt + 1))/3)." >&2
  done
}

if [[ "$mode" != selected && "$plan_only" != 1 ]]; then
  for attr in "${baseline_attrs[@]}"; do
    echo "::group::nix build ${attr}"
    nix_build ".#${attr}"
    echo "::endgroup::"
  done
fi

[[ "$mode" == baseline ]] && exit 0

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

changed_files=$(git diff --name-only "${base}"...HEAD)
changed_attrs=()
full_matrix=false
while IFS= read -r path; do
  case "$path" in
    scripts/ci/verify-t3code-providers.sh|scripts/ci/t3code-nix-home.sh|scripts/ci/ephemeral-nix-home.sh)
      changed_attrs+=(t3code t3code-fork)
      ;;
    ""|docs/*|README.md|AGENTS.md|LICENSE*|.forgejo/*|scripts/ci/*|scripts/forgejo/*|scripts/update-packages/*) ;;
    # Keep these reverse dependencies aligned with the explicit package inputs
    # in flake.nix. A wrapper must be validated when its packaged input changes.
    pkgs/claude-code/*)
      changed_attrs+=(claude-code t3code t3code-fork)
      ;;
    pkgs/codex-cli/*)
      changed_attrs+=(codex-cli t3code t3code-fork)
      ;;
    pkgs/xonsh-direnv/*)
      changed_attrs+=(xonsh-direnv xonsh-with-direnv)
      ;;
    pkgs/helium/*)
      # The default export aliases Helium on supported systems.
      changed_attrs+=(default helium)
      ;;
    pkgs/t3code/fork.nix|pkgs/t3code/patches/*)
      changed_attrs+=(t3code-fork)
      ;;
    pkgs/t3code/*)
      # Both exports share this source pin and recipe; the fork adds a patch.
      changed_attrs+=(t3code t3code-fork)
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

if [[ "$plan_only" == 1 ]]; then
  # This pre-Nix plan is a cheap workflow gate. The normal invocation repeats
  # selection and validates exports before evaluating or building them.
  : >"$plan_file"
  if "$full_matrix"; then
    printf '*\n' >"$plan_file"
  else
    plan_attrs=("${changed_attrs[@]}")
    [[ "$mode" == all ]] && plan_attrs+=("${baseline_attrs[@]}")
    if ((${#plan_attrs[@]} > 0)); then
      printf '%s\n' "${plan_attrs[@]}" | sort -u >"$plan_file"
    fi
  fi
  exit 0
fi

if ! "$full_matrix" && ((${#changed_attrs[@]} == 0)); then
  echo "No package-affecting changes detected."
  exit 0
fi

# Attribute names do not force derivations. Decide platform membership before
# evaluating drvPath so a broken Linux export cannot look like a Darwin-only one.
exports=$(nix eval .#packages --json --apply 'builtins.mapAttrs (_: builtins.attrNames)')

if "$full_matrix"; then
  echo "Shared inputs changed; validating the complete exported package matrix."
  selected=$(jq -r '[.[][]] | unique[]' <<<"$exports")
else
  selected=$(printf '%s\n' "${changed_attrs[@]}" | sort -u)
fi

selected_index=0
while IFS= read -r attr; do
  [[ -z "$attr" ]] && continue
  if ((selected_index % shard_count != shard_index)); then
    ((selected_index += 1))
    continue
  fi
  ((selected_index += 1))
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
      if [[ "$attr" == t3code || "$attr" == t3code-fork ]]; then
        # These dependency builds may create Nix's dummy home on unsandboxed
        # runners. Finish them separately so nix_build cleans between phases.
        nix_build "${package}.pnpmDeps" --no-link
        nix_build "${package}.resourceMonitor" --no-link
      fi
      nix_build "$package"
    else
      echo "${system}: derivation evaluated only; no native build on this runner."
    fi
  done <<<"$systems"
  echo "::endgroup::"
done <<<"$selected"
