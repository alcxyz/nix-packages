#!/usr/bin/env bash
set -euo pipefail

cache_dir=${NIX_CI_CACHE_DIR:-/tmp/nix-packages-cache}
max_cache_bytes=${NIX_CI_CACHE_MAX_BYTES:-2147483648}
snapshot_file=${NIX_CI_CACHE_SNAPSHOT:-${RUNNER_TEMP:-/tmp}/nix-build-cache-before.txt}
temporary_files=()

cleanup() {
  ((${#temporary_files[@]} == 0)) || rm -f -- "${temporary_files[@]}"
}
trap cleanup EXIT

usage() {
  echo "Usage: $0 prepare|save" >&2
}

fail() {
  echo "nix-build-cache: $*" >&2
  exit 1
}

require_file_command() {
  local variable_name=$1
  local file_path=${!variable_name:-}

  [[ -n "$file_path" ]] || fail "$variable_name is required"
  mkdir -p "$(dirname "$file_path")"
}

normalize_cache_dir() {
  cache_dir=$(realpath -m -- "$cache_dir")
  case "$cache_dir" in
    *[[:space:]?#]*)
      fail "NIX_CI_CACHE_DIR must not contain whitespace, '?' or '#': $cache_dir"
      ;;
  esac
  mkdir -p "$cache_dir"
}

snapshot_store() {
  local snapshot_tmp

  mkdir -p "$(dirname "$snapshot_file")"
  snapshot_tmp=$(mktemp "${snapshot_file}.XXXXXX")
  nix path-info --all | LC_ALL=C sort -u >"$snapshot_tmp"
  mv "$snapshot_tmp" "$snapshot_file"
}

prepare() {
  local cache_uri delimiter

  require_file_command GITHUB_ENV
  normalize_cache_dir
  snapshot_store

  if [[ ! -f "$cache_dir/nix-cache-info" ]]; then
    echo "nix-build-cache: no restored binary cache; continuing cold"
    return 0
  fi

  cache_uri="file://${cache_dir}?trusted=true"
  delimiter="NIX_CI_CACHE_CONFIG_${$}"
  {
    printf 'NIX_CONFIG<<%s\n' "$delimiter"
    [[ -z ${NIX_CONFIG:-} ]] || printf '%s\n' "$NIX_CONFIG"
    printf 'extra-substituters = %s\n' "$cache_uri"
    printf 'fallback = true\n'
    printf '%s\n' "$delimiter"
  } >>"$GITHUB_ENV"

  echo "nix-build-cache: configured restored cache as a lazy substituter"
}

emit_save_outputs() {
  local should_save=$1
  local cache_bytes=$2
  local path_count=$3

  require_file_command GITHUB_OUTPUT
  {
    printf 'save=%s\n' "$should_save"
    printf 'cache-bytes=%s\n' "$cache_bytes"
    printf 'path-count=%s\n' "$path_count"
  } >>"$GITHUB_OUTPUT"
}

save() {
  local metadata_file current_paths added_paths ultimate_paths export_paths
  local cache_uri cache_bytes path_count should_save

  [[ "$max_cache_bytes" =~ ^[1-9][0-9]*$ ]] ||
    fail "NIX_CI_CACHE_MAX_BYTES must be a positive integer"
  [[ -f "$snapshot_file" ]] || fail "store snapshot is missing: $snapshot_file"

  normalize_cache_dir
  metadata_file=$(mktemp "${snapshot_file}.metadata.XXXXXX")
  current_paths=$(mktemp "${snapshot_file}.current.XXXXXX")
  added_paths=$(mktemp "${snapshot_file}.added.XXXXXX")
  ultimate_paths=$(mktemp "${snapshot_file}.ultimate.XXXXXX")
  export_paths=$(mktemp "${snapshot_file}.export.XXXXXX")
  temporary_files=("$metadata_file" "$current_paths" "$added_paths" "$ultimate_paths" "$export_paths")

  nix path-info --json --all >"$metadata_file"
  jq -e 'type == "object" and all(.[]; has("ultimate"))' "$metadata_file" >/dev/null ||
    fail "nix path-info JSON does not contain the ultimate field"

  jq -r 'keys[]' "$metadata_file" | LC_ALL=C sort -u >"$current_paths"
  LC_ALL=C comm -13 "$snapshot_file" "$current_paths" >"$added_paths"
  jq -r 'to_entries[] | select(.value.ultimate == true) | .key' "$metadata_file" |
    LC_ALL=C sort -u >"$ultimate_paths"
  LC_ALL=C comm -12 "$added_paths" "$ultimate_paths" >"$export_paths"

  path_count=$(wc -l <"$export_paths")
  path_count=${path_count//[[:space:]]/}
  if ((path_count > 0)); then
    cache_uri="file://${cache_dir}?compression=zstd&compression-level=1"
    # A binary cache must contain a root's references before it can accept the
    # root. Pass only new local build outputs here; nix copy supplies the
    # closure needed to make those roots valid in the cache.
    xargs -r -n 128 nix copy --to "$cache_uri" <"$export_paths"
  fi

  cache_bytes=$(du -sb -- "$cache_dir" | awk '{print $1}')
  should_save=false
  if ((path_count > 0 && cache_bytes <= max_cache_bytes)); then
    should_save=true
  elif ((cache_bytes > max_cache_bytes)); then
    echo "nix-build-cache: cache is ${cache_bytes} bytes, above ${max_cache_bytes}; skipping upload" >&2
  else
    echo "nix-build-cache: no new locally built store paths; skipping upload"
  fi

  emit_save_outputs "$should_save" "$cache_bytes" "$path_count"
  echo "nix-build-cache: selected ${path_count} paths (${cache_bytes} cache bytes, save=${should_save})"
}

case ${1:-} in
  prepare)
    prepare
    ;;
  save)
    save
    ;;
  *)
    usage
    exit 2
    ;;
esac
