#!/usr/bin/env bash
set -euo pipefail

nix_build() {
  rm -rf /homeless-shelter
  nix build "$@"
}

for attr in agent-sync-check forge-mirror nix-deploy zfs-auto-unlock devlog wcap; do
  echo "::group::nix build ${attr}"
  nix_build ".#${attr}"
  echo "::endgroup::"
done

if [ -n "${GITHUB_BASE_REF:-}" ]; then
  git fetch origin "${GITHUB_BASE_REF}"
  base="origin/${GITHUB_BASE_REF}"
else
  base="$(git rev-parse HEAD^)"
fi

changed_attrs="$(
  git diff --name-only "${base}"...HEAD \
    | awk -F/ '/^(pkgs|tools)\/[^/]+\// { print $2 }' \
    | sort -u
)"

for attr in $changed_attrs; do
  echo "::group::changed package ${attr}"
  if [ ! -e "pkgs/${attr}" ] && [ ! -e "tools/${attr}" ]; then
    echo "${attr} was deleted in this change; skipping package build."
    echo "::endgroup::"
    continue
  fi

  if nix eval ".#packages.x86_64-linux.${attr}.drvPath" >/dev/null 2>&1; then
    nix_build ".#${attr}"
  else
    echo "${attr} is not buildable on the x86_64-linux runner; validating Darwin package metadata."
    nix eval ".#packages.aarch64-darwin.${attr}.meta.platforms" --json >/dev/null \
      || nix eval ".#packages.x86_64-darwin.${attr}.meta.platforms" --json >/dev/null
  fi
  echo "::endgroup::"
done
