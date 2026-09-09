#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
gate="$repo_root/scripts/ci/require-package-validation.sh"

PACKAGE_PREREQUISITES_RESULT=success \
  PACKAGE_BASELINE_RESULT=success \
  PACKAGE_SELECTED_RESULT=success \
  "$gate"

for result_name in \
  PACKAGE_PREREQUISITES_RESULT \
  PACKAGE_BASELINE_RESULT \
  PACKAGE_SELECTED_RESULT; do
  for failed_result in failure skipped cancelled missing; do
    status=0
    if [[ "$failed_result" == missing ]]; then
      env PACKAGE_PREREQUISITES_RESULT=success \
        PACKAGE_BASELINE_RESULT=success \
        PACKAGE_SELECTED_RESULT=success \
        env -u "$result_name" "$gate" >/dev/null 2>&1 || status=$?
    else
      env \
        PACKAGE_PREREQUISITES_RESULT=success \
        PACKAGE_BASELINE_RESULT=success \
        PACKAGE_SELECTED_RESULT=success \
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
