#!/usr/bin/env bash
set -euo pipefail

for result_name in \
  PACKAGE_PREREQUISITES_RESULT \
  PACKAGE_BASELINE_RESULT \
  PACKAGE_SELECTED_RESULT; do
  result="${!result_name-}"
  if [[ "$result" != success ]]; then
    echo "${result_name} must be success (got: ${result:-missing})." >&2
    exit 1
  fi
done
