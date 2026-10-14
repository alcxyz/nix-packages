#!/usr/bin/env bash
# Manifest expressions use jq variables, not shell interpolation.
# shellcheck disable=SC2016
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
script="$repo_root/modules/home-manager/workspace/workspace-sync.sh"
fixture=$(mktemp -d)
trap 'rm -rf "$fixture"' EXIT
export GIT_CONFIG_NOSYSTEM=1
export GIT_CONFIG_GLOBAL=/dev/null
export GIT_TERMINAL_PROMPT=0

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

git init -q -b main "$fixture/source"
printf 'original\n' >"$fixture/source/tracked"
git -C "$fixture/source" add tracked
git -C "$fixture/source" -c user.name=Fixture -c user.email=fixture@example.invalid commit -qm initial

manifest="$fixture/manifest.json"
root="$fixture/workspace"
make_manifest() {
  jq -n --arg source "$fixture/source" --arg missing "$fixture/nonexistent" \
    "{directories: [\"apps\", \"scratch\"], repositories: $1, links: []}" >"$manifest"
}
sync_workspace() { bash "$script" "$root" "$manifest" "$@" >"$fixture/output" 2>&1; }

# Successful clones and existing repositories have a successful exit status.
make_manifest '[{path:"apps/success",url:$source,branch:"main"}]'
sync_workspace
rg -q '0 existing, 1 cloned, 0 skipped, 0 failed' "$fixture/output"
cmp "$fixture/source/tracked" "$root/apps/success/tracked"
printf 'local edit\n' >"$root/apps/success/tracked"
sync_workspace
rg -q '1 existing, 0 cloned, 0 skipped, 0 failed' "$fixture/output"
[ "$(cat "$root/apps/success/tracked")" = 'local edit' ] || fail 'existing checkout was modified'

# Failed clones are summarized and return failure.
make_manifest '[{path:"apps/failure",url:$missing}]'
if sync_workspace; then fail 'failed clone returned success'; fi
rg -q '0 existing, 0 cloned, 0 skipped, 1 failed' "$fixture/output"
rg -q 'workspace-sync failed repositories:' "$fixture/output"

# A failed named branch must not stop later successful clones.
make_manifest '[{path:"apps/bad-branch",url:$source,branch:"nonexistent"},{path:"apps/later",url:$source}]'
if sync_workspace; then fail 'mixed clones returned success'; fi
rg -q '0 existing, 1 cloned, 0 skipped, 1 failed' "$fixture/output"
cmp "$fixture/source/tracked" "$root/apps/later/tracked"

# Worktrees count as existing, while non-Git paths (including directories
# inside another repository) remain untouched and are counted as skipped.
git -C "$fixture/source" worktree add -q --detach "$root/apps/worktree"
printf 'worktree edit\n' >"$root/apps/worktree/tracked"
mkdir -p "$root/apps/success/ordinary"
printf 'keep\n' >"$root/apps/success/ordinary/file"
printf 'keep file\n' >"$root/apps/file"
make_manifest '[{path:"apps/worktree",url:$missing},{path:"apps/success/ordinary",url:$missing},{path:"apps/file",url:$missing}]'
sync_workspace
rg -q '1 existing, 0 cloned, 2 skipped, 0 failed' "$fixture/output"
[ "$(cat "$root/apps/worktree/tracked")" = 'worktree edit' ] || fail 'worktree was modified'
[ "$(cat "$root/apps/success/ordinary/file")" = keep ] || fail 'non-Git directory was modified'
[ "$(cat "$root/apps/file")" = 'keep file' ] || fail 'existing file was modified'

# Status reports missing entries without attempting clones or reporting failure.
make_manifest '[{path:"apps/worktree",url:$missing},{path:"apps/not-cloned",url:$source}]'
sync_workspace --status
rg -q '1 existing, 1 missing, 0 skipped' "$fixture/output"
[ ! -e "$root/apps/not-cloned" ] || fail 'status cloned a repository'

# Status on a fresh root is read-only, including declared directories.
fresh_root="$fixture/fresh"
root=$fresh_root
sync_workspace --status
[ ! -e "$fresh_root" ] || fail 'status created the workspace root'
root="$fixture/workspace"

# Links use relative paths, are idempotent, and preserve existing content.
jq '.repositories=[] | .links=[{path:"tools/current",target:"apps/success",profiles:["tools"]}]' "$manifest" >"$fixture/links.json"
mv "$fixture/links.json" "$manifest"
sync_workspace
[ "$(readlink "$root/tools/current")" = ../apps/success ] || fail 'link is not relative'
sync_workspace
rg -q '1 links existing, 0 links created' "$fixture/output"
printf 'local edit again\n' >"$root/apps/success/tracked"
[ "$(cat "$root/apps/success/tracked")" = 'local edit again' ] || fail 'target was modified'

rm "$root/tools/current"
printf 'keep\n' >"$root/tools/current"
sync_workspace
rg -q '1 links skipped' "$fixture/output"
[ "$(cat "$root/tools/current")" = keep ] || fail 'existing link path was replaced'
rm "$root/tools/current"
ln -s wrong "$root/tools/current"
sync_workspace
rg -q '1 links skipped' "$fixture/output"
[ "$(readlink "$root/tools/current")" = wrong ] || fail 'wrong symlink was replaced'

# A missing target stays pending until its canonical checkout appears.
jq '.links=[{path:"tools/pending",target:"apps/pending",profiles:["tools"]}]' "$manifest" >"$fixture/links.json"
mv "$fixture/links.json" "$manifest"
sync_workspace
[ ! -e "$root/tools/pending" ] || fail 'created dangling link'
rg -q '1 links missing' "$fixture/output"

# Reject traversal before any filesystem writes, and reject symlink parents.
root="$fixture/unsafe"
jq '.directories=["../escape"]' "$manifest" >"$fixture/bad.json"
mv "$fixture/bad.json" "$manifest"
if sync_workspace; then fail 'accepted unsafe directory'; fi
[ ! -e "$root" ] || fail 'invalid manifest created workspace'
jq '.directories=[] | .repositories=[{path:"apps/../escape",url:"local"}]' "$manifest" >"$fixture/bad.json"
mv "$fixture/bad.json" "$manifest"
if sync_workspace; then fail 'accepted unsafe repository path'; fi
jq '.repositories=[] | .links=[{path:"tools/link",target:"/etc/passwd",profiles:["tools"]}]' "$manifest" >"$fixture/bad.json"
mv "$fixture/bad.json" "$manifest"
if sync_workspace; then fail 'accepted unsafe link target'; fi
[ ! -e "$root" ] || fail 'unsafe manifest created workspace'

mkdir -p "$fixture/outside" "$root"
ln -s "$fixture/outside" "$root/tools"
jq '.directories=["tools/nested"] | .links=[{path:"tools/link",target:"apps/success",profiles:["tools"]}]' "$manifest" >"$fixture/bad.json"
mv "$fixture/bad.json" "$manifest"
sync_workspace
[ ! -e "$fixture/outside/link" ] || fail 'followed symlink parent'
[ ! -e "$fixture/outside/nested" ] || fail 'created directory outside workspace'

# Directory activation handles a nested root and reports blocked paths.
root="$fixture/nested/child/workspace"
make_manifest '[]'
sync_workspace --directories
[ -d "$root/apps" ] || fail 'nested workspace root was not created'
mkdir -p "$root/blocked"
printf 'keep\n' >"$root/blocked/file"
jq '.directories=["blocked/file/deeper","scratch/available"]' "$manifest" >"$fixture/dirs.json"
mv "$fixture/dirs.json" "$manifest"
if sync_workspace --directories; then fail 'directory activation ignored a conflict'; fi
rg -q '1 directory conflicts or failures' "$fixture/output"
[ -d "$root/scratch/available" ] || fail 'directory activation stopped before later directories'
[ "$(cat "$root/blocked/file")" = keep ] || fail 'blocked path was modified'

echo 'Workspace sync tests passed'
