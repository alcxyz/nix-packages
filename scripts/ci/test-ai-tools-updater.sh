#!/usr/bin/env bash
set -euo pipefail
repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/scripts/update-packages"
for package in claude-code codex-cli codex-app-server; do
  mkdir -p "$tmp/pkgs/$package"
  printf 'version = "1.2.3";\n' > "$tmp/pkgs/$package/default.nix"
  cat > "$tmp/scripts/update-packages/update-$package.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
package=${0##*/update-}
package=${package%.sh}
printf '%s\n' "$package" >> calls
if [[ "$package" == "${FAIL_PACKAGE:-}" ]]; then exit 42; fi
printf 'updated=%s\n' "${CHANGED:-false}" >> "$GITHUB_OUTPUT"
SH
done
cd "$tmp"
export GITHUB_OUTPUT="$tmp/output"
bash "$repo/scripts/update-packages/update-ai-tools.sh"
grep -qx 'updated=false' output
grep -qx 'version=Codex 1.2.3, app-server 1.2.3, Claude 1.2.3' output
printf 'claude-code\ncodex-cli\ncodex-app-server\n' > expected
cmp calls expected
: > output
CHANGED=true bash "$repo/scripts/update-packages/update-ai-tools.sh"
grep -qx 'updated=true' output
: > calls
: > output
if FAIL_PACKAGE=codex-cli bash "$repo/scripts/update-packages/update-ai-tools.sh"; then
  echo 'Expected provider failure to abort combined update' >&2
  exit 1
else
  [[ $? == 42 ]]
fi
[[ ! -s output ]]
printf 'claude-code\ncodex-cli\n' > expected
cmp calls expected
echo 'Grouped AI updater tests passed.'
