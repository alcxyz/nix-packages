#!/usr/bin/env bash
# Update providers together so T3 validates one combined candidate.
set -euo pipefail

: "${GITHUB_OUTPUT:?GITHUB_OUTPUT is required}"
output_file=$GITHUB_OUTPUT
step_output=$(mktemp)
trap 'rm -f "$step_output"' EXIT
updated=false
for package in claude-code codex-cli codex-app-server; do
  : > "$step_output"
  GITHUB_OUTPUT="$step_output" bash "scripts/update-packages/update-${package}.sh"
  if grep -qx 'updated=true' "$step_output"; then
    updated=true
  fi
done

version=$(python3 - <<'PY'
import re
from pathlib import Path

versions = []
for package, label in [('codex-cli', 'Codex'), ('codex-app-server', 'app-server'), ('claude-code', 'Claude')]:
    content = Path(f'pkgs/{package}/default.nix').read_text()
    match = re.search(r'\bversion\s*=\s*"([0-9]+\.[0-9]+\.[0-9]+)";', content)
    if not match:
        raise SystemExit(f'Missing stable version in {package}')
    versions.append(f'{label} {match[1]}')
print(', '.join(versions))
PY
)
printf 'updated=%s\nversion=%s\n' "$updated" "$version" >> "$output_file"
