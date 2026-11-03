# shellcheck shell=bash
# Collect the hash of a fixed-output derivation from a placeholder-hash build.
#
# The shared CI runners run cheap checks only (ADR-0003), so an updater may
# realise a derivation only to learn a fixed-output hash, and only when that is
# a fetch: every derivation the build would run must be fixed-output. Anything
# that would compile (a package, or a fetcher whose inputs need a local build)
# is refused before Nix builds it.

# require_fetch_only INSTALLABLE: fail unless every derivation Nix would build
# for INSTALLABLE is fixed-output. Substituted paths are fetches and allowed.
require_fetch_only() {
  local installable=$1 plan compiled
  local -a drvs
  if ! plan=$(nix build --dry-run --no-link "$installable" 2>&1); then
    printf '%s\n' "$plan" >&2
    echo "Unable to plan ${installable}." >&2
    return 1
  fi
  mapfile -t drvs < <(awk '
    /will be built:$/ { listing = 1; next }
    /^[^[:space:]]/ { listing = 0 }
    listing && $1 ~ /\.drv$/ { print $1 }
  ' <<<"$plan")
  ((${#drvs[@]} > 0)) || return 0
  compiled=$(nix derivation show "${drvs[@]}" | jq -r '
    (.derivations // .) | to_entries[]
    | select(any(.value.outputs[]; has("hash") | not)) | .key
  ') || return 1
  if [[ -n "$compiled" ]]; then
    {
      echo "Refusing to build ${installable} on CI: it needs derivations that are not fixed-output:"
      printf '  %s\n' "${compiled//$'\n'/$'\n'  }"
      echo "Compute this hash locally (ADR-0003)."
    } >&2
    return 1
  fi
}

# prepare_fetch_build: clear the dummy Nix home before a non-sandboxed build,
# using whichever opt-in CI cleanup the caller sourced.
prepare_fetch_build() {
  if declare -F clean_homeless_shelter >/dev/null; then
    clean_homeless_shelter
  elif declare -F clean_ephemeral_nix_home >/dev/null; then
    clean_ephemeral_nix_home
  fi
}

# realise_fetch_only INSTALLABLE [EXPECTED_HASH]: realise the fixed-output
# INSTALLABLE so Nix checks the hash pinned for it, refusing anything that would
# compile. With EXPECTED_HASH, first fail unless that is the pinned hash, since
# a stale pin can name a path the store already holds.
realise_fetch_only() {
  local installable=$1 expected=${2:-} pinned
  if [[ -n "$expected" ]]; then
    pinned=$(nix eval --raw "${installable}.outputHash") || return 1
    if [[ "$pinned" != "$expected" ]]; then
      echo "${installable} pins ${pinned}, expected ${expected}." >&2
      return 1
    fi
  fi
  require_fetch_only "$installable" || return 1
  prepare_fetch_build || return 1
  nix build --no-link "$installable"
}

# collect_fod_hash INSTALLABLE LABEL: build the fixed-output INSTALLABLE, whose
# hash is a placeholder, and print the hash Nix reports for it.
collect_fod_hash() {
  local installable=$1 label=$2 log hash
  require_fetch_only "$installable" || return 1
  prepare_fetch_build || return 1
  log=$(mktemp)
  if nix build -L --no-link "$installable" 2>"$log"; then
    cat "$log" >&2
    rm -f "$log"
    echo "Expected the ${label} fetch to fail with the placeholder hash" >&2
    return 1
  fi
  cat "$log" >&2
  hash=$(sed -nE 's/^[[:space:]]*got:[[:space:]]+(sha256-[^[:space:]]+).*/\1/p' "$log" | tail -n1)
  rm -f "$log"
  if [[ ! "$hash" =~ ^sha256-.+ ]]; then
    echo "Unable to collect ${label} hash" >&2
    return 1
  fi
  printf '%s\n' "$hash"
}
