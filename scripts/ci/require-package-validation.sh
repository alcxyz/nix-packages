#!/usr/bin/env bash
# Aggregate gate behind the required "Package build validation" context. CI
# runs cheap checks only (ADR-0003), so it requires the Go tests and the
# evaluation and script checks.
set -euo pipefail

for result_name in GO_TEST_RESULT PACKAGE_PREREQUISITES_RESULT; do
  result="${!result_name-}"
  if [[ "$result" != success ]]; then
    echo "${result_name} must be success (got: ${result:-missing})." >&2
    exit 1
  fi
done
