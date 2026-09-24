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

case "${PACKAGE_FULL_MATRIX:-}" in
  true)
    for result_name in PACKAGE_FULL_SHARD_0_RESULT PACKAGE_FULL_SHARD_1_RESULT; do
      result="${!result_name-}"
      [[ "$result" == success ]] || {
        echo "${result_name} must be success for a full export plan (got: ${result:-missing})." >&2
        exit 1
      }
    done
    ;;
  false)
    for result_name in PACKAGE_FULL_SHARD_0_RESULT PACKAGE_FULL_SHARD_1_RESULT; do
      result="${!result_name-}"
      [[ "$result" == skipped ]] || {
        echo "${result_name} must be skipped for a selected plan (got: ${result:-missing})." >&2
        exit 1
      }
    done
    ;;
  *)
    echo "PACKAGE_FULL_MATRIX must be true or false (got: ${PACKAGE_FULL_MATRIX:-missing})." >&2
    exit 1
    ;;
esac
