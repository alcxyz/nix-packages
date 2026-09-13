#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
gate="$repo_root/scripts/ci/require-package-validation.sh"

GO_TEST_RESULT=success \
  PACKAGE_PREREQUISITES_RESULT=success \
  PACKAGE_BUILDS_RESULT=success \
  "$gate"

for result_name in \
  GO_TEST_RESULT \
  PACKAGE_PREREQUISITES_RESULT \
  PACKAGE_BUILDS_RESULT; do
  for failed_result in failure skipped cancelled missing; do
    status=0
    if [[ "$failed_result" == missing ]]; then
      env GO_TEST_RESULT=success \
        PACKAGE_PREREQUISITES_RESULT=success \
        PACKAGE_BUILDS_RESULT=success \
        env -u "$result_name" "$gate" >/dev/null 2>&1 || status=$?
    else
      env \
        GO_TEST_RESULT=success \
        PACKAGE_PREREQUISITES_RESULT=success \
        PACKAGE_BUILDS_RESULT=success \
        "$result_name=$failed_result" \
        "$gate" >/dev/null 2>&1 || status=$?
    fi
    if [[ "$status" != 1 ]]; then
      echo "Expected ${result_name}=${failed_result} to fail closed, got status ${status}." >&2
      exit 1
    fi
  done
done

echo 'Package validation aggregate regression tests passed.'
