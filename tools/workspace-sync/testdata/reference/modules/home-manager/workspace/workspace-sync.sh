#!/usr/bin/env bash
set -euo pipefail

root=$1
manifest=$2
shift 2
mode=sync

case "${1:-}" in
  --status) mode=status ;;
  --directories) mode=directories ;;
  --help | -h)
    cat <<'USAGE'
workspace-sync [--status | --directories]

Create missing workspace directories, clone missing repositories, and add missing
relative links to existing canonical targets. Existing paths are preserved.
--status reports the layout without changing it; --directories creates only
configured directories (used during Home Manager activation).
USAGE
    exit 0
    ;;
  "") ;;
  *)
    printf 'workspace-sync: unknown argument: %s\n' "$1" >&2
    exit 2
    ;;
esac

# Validate the entire manifest before creating anything. Restrict path components
# so newline-delimited jq output is unambiguous and no path can escape the root.
if ! jq -e '
  def safe_path:
    type == "string" and length > 0 and
    (split("/") | all(.[]; . != "." and . != ".." and test("^[A-Za-z0-9._-]+$")));
  (.directories | type == "array" and all(.[]; safe_path)) and
  (.repositories | type == "array" and all(.[];
    (.path | safe_path) and (.url | type == "string" and length > 0) and
    (.branch == null or (.branch | type == "string")))) and
  (.links | type == "array" and all(.[];
    (.path | safe_path) and (.target | safe_path) and
    (.profiles | type == "array" and all(.[]; type == "string"))))
' "$manifest" >/dev/null; then
  printf 'workspace-sync: invalid manifest\n' >&2
  exit 2
fi

# Reject pre-existing symlinks at the root and every managed parent. A symlink
# there could otherwise redirect mkdir or clone outside the workspace.
prepare_directory() {
  local relative=$1 current=$root component
  local -a parts
  if [ -L "$current" ] || { [ -e "$current" ] && [ ! -d "$current" ]; }; then
    printf 'conflict: %s is not a plain directory\n' "$current" >&2
    return 1
  fi
  if [ ! -d "$current" ]; then
    if [ "$mode" = status ]; then
      return 0
    fi
    if ! mkdir -p -- "$current"; then
      printf 'failed: could not create %s\n' "$current" >&2
      return 1
    fi
  fi
  if [ "$relative" = . ]; then return 0; fi
  IFS=/ read -r -a parts <<<"$relative"
  for component in "${parts[@]}"; do
    current="$current/$component"
    if [ -L "$current" ] || { [ -e "$current" ] && [ ! -d "$current" ]; }; then
      printf 'conflict: %s is not a plain directory\n' "$current" >&2
      return 1
    fi
    if [ ! -d "$current" ]; then
      if [ "$mode" = status ]; then
        return 0
      fi
      if ! mkdir -- "$current"; then
        printf 'failed: could not create %s\n' "$current" >&2
        return 1
      fi
    fi
  done
}

# This is also checked when the manifest has no directories.
if ! prepare_directory .; then exit 1; fi
directory_failures=0
while IFS= read -r dir; do
  if ! prepare_directory "$dir"; then
    directory_failures=$((directory_failures + 1))
  fi
done < <(jq -r '.directories[]' "$manifest")

if [ "$mode" = directories ]; then
  if [ "$directory_failures" -gt 0 ]; then
    printf 'workspace-sync: %d directory conflicts or failures\n' "$directory_failures" >&2
    exit 1
  fi
  exit 0
fi

existing=0 cloned=0 missing=0 skipped=0 failed=0
links_existing=0 links_created=0 links_missing=0 links_skipped=0
failures=()

while IFS= read -r repo; do
  path=$(jq -r '.path' <<<"$repo")
  url=$(jq -r '.url' <<<"$repo")
  branch=$(jq -r '.branch // empty' <<<"$repo")
  target="$root/$path"
  parent=${path%/*}
  if [ "$parent" = "$path" ]; then parent=.; fi

  if ! prepare_directory "$parent"; then
    skipped=$((skipped + 1))
    continue
  fi
  if [ -L "$target" ]; then
    printf 'conflict: %s is a symlink\n' "$target" >&2
    skipped=$((skipped + 1))
    continue
  fi
  if [ -e "$target/.git" ] && git -C "$target" rev-parse --git-dir >/dev/null 2>&1; then
    printf 'exists: %s\n' "$target"
    existing=$((existing + 1))
    continue
  fi
  if [ -e "$target" ]; then
    printf 'conflict: %s exists but is not a git repository\n' "$target" >&2
    skipped=$((skipped + 1))
    continue
  fi
  if [ "$mode" = status ]; then
    printf 'missing: %s -> %s\n' "$target" "$url"
    missing=$((missing + 1))
    continue
  fi
  if [ -n "$branch" ]; then
    if git clone --branch "$branch" "$url" "$target"; then
      cloned=$((cloned + 1))
    else
      printf 'failed: %s -> %s (branch: %s)\n' "$target" "$url" "$branch" >&2
      failures+=("$path -> $url (branch: $branch)")
      failed=$((failed + 1))
    fi
  else
    if git clone "$url" "$target"; then
      cloned=$((cloned + 1))
    else
      printf 'failed: %s -> %s\n' "$target" "$url" >&2
      failures+=("$path -> $url")
      failed=$((failed + 1))
    fi
  fi
done < <(jq -c '.repositories[]' "$manifest")

while IFS= read -r link; do
  path=$(jq -r '.path' <<<"$link")
  canonical=$(jq -r '.target' <<<"$link")
  target="$root/$path"
  parent=${path%/*}
  if [ "$parent" = "$path" ]; then parent=.; fi

  if ! prepare_directory "$parent"; then
    links_skipped=$((links_skipped + 1))
    continue
  fi
  relative_target=$(realpath -m --relative-to="$(dirname "$target")" "$root/$canonical")
  if [ -L "$target" ]; then
    if [ "$(readlink -- "$target")" = "$relative_target" ] && [ -e "$target" ]; then
      printf 'link exists: %s -> %s\n' "$target" "$relative_target"
      links_existing=$((links_existing + 1))
    else
      printf 'conflict: %s is an existing symlink\n' "$target" >&2
      links_skipped=$((links_skipped + 1))
    fi
    continue
  fi
  if [ -e "$target" ]; then
    printf 'conflict: %s already exists\n' "$target" >&2
    links_skipped=$((links_skipped + 1))
    continue
  fi
  if [ ! -e "$root/$canonical" ]; then
    printf 'link target missing: %s -> %s\n' "$target" "$canonical"
    links_missing=$((links_missing + 1))
    continue
  fi
  resolved_root=$(realpath -e -- "$root")
  resolved_target=$(realpath -e -- "$root/$canonical")
  case "$resolved_target" in
    "$resolved_root"/*) ;;
    *)
      printf 'conflict: %s resolves outside workspace\n' "$root/$canonical" >&2
      links_skipped=$((links_skipped + 1))
      continue
      ;;
  esac
  if [ "$mode" = status ]; then
    printf 'link missing: %s -> %s\n' "$target" "$relative_target"
    links_missing=$((links_missing + 1))
  else
    ln -s -- "$relative_target" "$target"
    printf 'link created: %s -> %s\n' "$target" "$relative_target"
    links_created=$((links_created + 1))
  fi
done < <(jq -c '.links[]' "$manifest")

if [ "$mode" = status ]; then
  printf '\nworkspace-sync status: %d existing, %d missing, %d skipped; %d links existing, %d links missing, %d links skipped\n' \
    "$existing" "$missing" "$skipped" "$links_existing" "$links_missing" "$links_skipped"
else
  printf '\nworkspace-sync summary: %d existing, %d cloned, %d skipped, %d failed; %d links existing, %d links created, %d links missing, %d links skipped\n' \
    "$existing" "$cloned" "$skipped" "$failed" "$links_existing" "$links_created" "$links_missing" "$links_skipped"
  if [ "$failed" -gt 0 ]; then
    printf 'workspace-sync failed repositories:\n' >&2
    printf '  - %s\n' "${failures[@]}" >&2
    exit 1
  fi
fi
