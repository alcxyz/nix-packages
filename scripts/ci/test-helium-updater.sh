#!/usr/bin/env bash
set -eEuo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
test_root=$(mktemp -d)
trap 'rm -rf "$test_root"' EXIT
trap 'echo "Helium test failed for ${asset:-setup} (line $LINENO, updater status ${status:-unset})" >&2; [[ ! -f output ]] || cat output >&2' ERR
mkdir -p "$test_root/bin"
cat >"$test_root/bin/curl" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
output=''
while (($#)); do
  case "$1" in
    -o) output=$2; shift 2 ;;
    -H) shift 2 ;;
    -*) shift ;;
    *) url=$1; shift ;;
  esac
done
if [[ "$url" == */releases/latest ]]; then
  printf '{"tag_name":"%s"}\n' "${MOCK_LATEST_VERSION:-99.0.0}"
elif [[ "$url" == *"${MOCK_MISSING_ASSET}" ]]; then
  exit 22
else
  printf 'mock upstream asset\n' >"$output"
fi
MOCK
chmod +x "$test_root/bin/curl"

for asset in x86_64.AppImage arm64-macos.dmg x86_64-macos.dmg arm64.AppImage; do
  case_root="$test_root/$asset"
  mkdir -p "$case_root/pkgs/helium"
  cp "$repo_root/pkgs/helium/default.nix" "$case_root/pkgs/helium/default.nix"
  cd "$case_root"
  status=0
  PATH="$test_root/bin:$PATH" GITHUB_TOKEN='' GITHUB_OUTPUT="$case_root/result" \
    MOCK_MISSING_ASSET="$asset" \
    bash "$repo_root/scripts/update-packages/update-helium.sh" >output 2>&1 || status=$?
  if [[ "$asset" == arm64.AppImage ]]; then
    [[ "$status" == 0 ]]
    grep -q 'updated=true' result
    grep -q 'version = "99.0.0"' pkgs/helium/default.nix
    [[ $(grep -c 'hash = lib.fakeHash;' pkgs/helium/default.nix) == 1 ]]
  else
    [[ "$status" != 0 ]]
    grep -q 'Failed to fetch required Helium asset' output
    cmp "$repo_root/pkgs/helium/default.nix" pkgs/helium/default.nix
    [[ ! -e result ]]
  fi
done

case_root="$test_root/already-current"
mkdir -p "$case_root/pkgs/helium"
cp "$repo_root/pkgs/helium/default.nix" "$case_root/pkgs/helium/default.nix"
cd "$case_root"
current_version=$(sed -n 's/.*version = "\([^"]*\)";.*/\1/p' pkgs/helium/default.nix | head -n1)
PATH="$test_root/bin:$PATH" GITHUB_TOKEN='' GITHUB_OUTPUT="$case_root/result" \
  MOCK_LATEST_VERSION="$current_version" MOCK_MISSING_ASSET='AppImage' \
  bash "$repo_root/scripts/update-packages/update-helium.sh" >output 2>&1
grep -q 'updated=false' result
cmp "$repo_root/pkgs/helium/default.nix" pkgs/helium/default.nix

echo 'Helium updater required/optional asset regression tests passed.'
