#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
gate="$repo_root/scripts/ci/require-package-validation.sh"

GO_TEST_RESULT=success \
  PACKAGE_PREREQUISITES_RESULT=success \
  PACKAGE_BUILDS_RESULT=success \
  PACKAGE_FULL_MATRIX=false \
  PACKAGE_FULL_SHARD_0_RESULT=skipped \
  PACKAGE_FULL_SHARD_1_RESULT=skipped \
  "$gate"

GO_TEST_RESULT=success \
  PACKAGE_PREREQUISITES_RESULT=success \
  PACKAGE_BUILDS_RESULT=success \
  PACKAGE_FULL_MATRIX=true \
  PACKAGE_FULL_SHARD_0_RESULT=success \
  PACKAGE_FULL_SHARD_1_RESULT=success \
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
        PACKAGE_FULL_MATRIX=false \
        PACKAGE_FULL_SHARD_0_RESULT=skipped \
        PACKAGE_FULL_SHARD_1_RESULT=skipped \
        env -u "$result_name" "$gate" >/dev/null 2>&1 || status=$?
    else
      env \
        GO_TEST_RESULT=success \
        PACKAGE_PREREQUISITES_RESULT=success \
        PACKAGE_BUILDS_RESULT=success \
        PACKAGE_FULL_MATRIX=false \
        PACKAGE_FULL_SHARD_0_RESULT=skipped \
        PACKAGE_FULL_SHARD_1_RESULT=skipped \
        "$result_name=$failed_result" \
        "$gate" >/dev/null 2>&1 || status=$?
    fi
    if [[ "$status" != 1 ]]; then
      echo "Expected ${result_name}=${failed_result} to fail closed, got status ${status}." >&2
      exit 1
    fi
  done
done

for full in true false missing; do
  for shard in 0 1; do
    for full_result in success skipped failure cancelled missing; do
      expected_result=skipped
      [[ $full == true ]] && expected_result=success
      status=0
      env GO_TEST_RESULT=success PACKAGE_PREREQUISITES_RESULT=success PACKAGE_BUILDS_RESULT=success \
        PACKAGE_FULL_MATRIX="$full" \
        PACKAGE_FULL_SHARD_0_RESULT="$expected_result" PACKAGE_FULL_SHARD_1_RESULT="$expected_result" \
        "PACKAGE_FULL_SHARD_${shard}_RESULT=$full_result" \
        "$gate" >/dev/null 2>&1 || status=$?
      expected=1
      if [[ $full != missing && $full_result == "$expected_result" ]]; then expected=0; fi
      if [[ $status != "$expected" ]]; then
        echo "Expected full=${full} shard=${shard} result=${full_result} status ${expected}, got ${status}." >&2
        exit 1
      fi
    done
  done
done

echo 'Package validation aggregate regression tests passed.'
