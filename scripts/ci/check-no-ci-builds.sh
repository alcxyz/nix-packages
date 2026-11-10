#!/usr/bin/env bash
# Fail when CI could run a native Nix build (ADR-0003). The shared runners run
# cheap checks only: tests, lint, and Nix evaluation without builds. This scans
# the workflows and every non-test script under scripts/, which covers helpers
# a workflow reaches indirectly, such as files sourced through $(dirname ...).
# Fixed-output fetches are allowed only through
# scripts/update-packages/fod-hash.sh, which refuses anything that would
# compile. The local-only build scripts are exempt from the scan, and any
# other scanned file that names them fails, by file name alone, so a call
# through a variable such as "$script_dir/build-changed-packages.sh" counts too.
# Only the `nix flake check ... --no-build` command itself is allowed, and only
# without a command substitution in it; a build elsewhere on the same line
# still fails. Test scripts, including the tools/
# contracts the workflows run, put mocked Nix commands first on PATH and are
# not scanned.
set -euo pipefail

root=${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}
cd "$root"

builds='nix[[:space:]]+build|nix-build|nixos-rebuild|nixosTest|runNixOSTest|nix[[:space:]]+flake[[:space:]]+check'
no_build_check='nix[[:space:]]+flake[[:space:]]+check[^#;&|$`()]*--no-build'
local_only='(^|[^[:alnum:]_-])(build-changed-packages|verify-t3code-providers)\.sh'

files=(.forgejo/workflows/*.yml)
while IFS= read -r file; do
  files+=("$file")
done < <(rg --files scripts | sort)

status=0
for file in "${files[@]}"; do
  case "$file" in
    scripts/ci/test-* | scripts/update-packages/fod-hash.sh | \
      scripts/ci/check-no-ci-builds.sh | scripts/ci/build-changed-packages.sh | \
      scripts/ci/verify-t3code-providers.sh) continue ;;
  esac
  if hits=$(rg -n '' "$file" | sed -E "s/${no_build_check}//g" | rg -e "$builds"); then
    printf '%s runs a native Nix build on CI:\n%s\n' "$file" "$hits" >&2
    status=1
  fi
  if hits=$(rg -n -e "$local_only" "$file"); then
    printf '%s runs a local-only build script on CI:\n%s\n' "$file" "$hits" >&2
    status=1
  fi
done
((status == 0)) && echo "No CI workflow or script it runs builds natively (${#files[@]} files checked)."
exit "$status"
