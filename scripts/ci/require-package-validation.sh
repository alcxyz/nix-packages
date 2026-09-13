#!/usr/bin/env bash
set -euo pipefail

for result_name in \
  GO_TEST_RESULT \
  PACKAGE_PREREQUISITES_RESULT \
  PACKAGE_BUILDS_RESULT; do
  result="${!result_name-}"
  if [[ "$result" != success ]]; then
    echo "${result_name} must be success (got: ${result:-missing})." >&2
    exit 1
  fi
done
