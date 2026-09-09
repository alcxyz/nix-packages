#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

# shellcheck disable=SC1091
source "$ROOT/scripts/ops/k8s-node-reboot.sh"

export NODE=worker-a
export SETTLE_TIMEOUT=1s

NODES_JSON='{
  "items": [
    {
      "metadata": {
        "name": "worker-a",
        "labels": {
          "kubernetes.io/hostname": "worker-a",
          "browser-worker": "true"
        }
      },
      "spec": {},
      "status": {"conditions": [{"type": "Ready", "status": "True"}]}
    },
    {
      "metadata": {
        "name": "worker-b",
        "labels": {"kubernetes.io/hostname": "worker-b"}
      },
      "spec": {},
      "status": {"conditions": [{"type": "Ready", "status": "True"}]}
    }
  ]
}'
WORKLOAD_JSON=""

# Invoked indirectly by workload_requires_target_node from the sourced script.
# shellcheck disable=SC2329
kubectl() {
  if [[ "$*" == "get nodes -o json" ]]; then
    printf '%s\n' "$NODES_JSON"
    return 0
  fi

  if [[ "$*" == *"get deployment/fixture -o json" ]]; then
    printf '%s\n' "$WORKLOAD_JSON"
    return 0
  fi

  printf 'unexpected pinning-test kubectl call: %s\n' "$*" >&2
  return 1
}

WORKLOAD_JSON='{
  "spec": {"template": {"spec": {"nodeSelector": {"browser-worker": "true"}}}}
}'
# The implementation is imported from the sourced operations script. A test
# double with the same name is installed below for the phase-ordering checks.
# shellcheck disable=SC2218
workload_requires_target_node default deployment/fixture

WORKLOAD_JSON='{
  "spec": {"template": {"spec": {}}}
}'
if workload_requires_target_node default deployment/fixture; then
  printf 'portable workload was incorrectly classified as target-pinned\n' >&2
  exit 1
fi

WORKLOAD_JSON='{
  "spec": {"template": {"spec": {"affinity": {"nodeAffinity": {
    "requiredDuringSchedulingIgnoredDuringExecution": {"nodeSelectorTerms": [{
      "matchExpressions": [{
        "key": "browser-worker",
        "operator": "In",
        "values": ["true"]
      }]
    }]}
  }}}}}
}'
# shellcheck disable=SC2218
workload_requires_target_node default deployment/fixture

WORKLOADS="$TMP/workloads"
PREFLIGHT_CALLS="$TMP/preflight-calls"
RETURN_CALLS="$TMP/return-calls"

cat >"$WORKLOADS" <<'EOF'
default	deployment/pinned
default	deployment/portable
EOF

workload_requires_target_node() {
  [[ "$2" == "deployment/pinned" ]]
}

wait_for_controller_ready_floor() {
  printf '%s/%s\n' "$1" "$2" >>"$PREFLIGHT_CALLS"
}

kubectl() {
  if [[ "$*" == *"rollout status"* ]]; then
    printf '%s\n' "$*" >>"$RETURN_CALLS"
    return 0
  fi

  if [[ "$*" == *"get deployment/portable"* ]]; then
    printf '1'
    return 0
  fi

  printf 'unexpected kubectl call: %s\n' "$*" >&2
  return 1
}

wait_for_displaced_workload_survivability "$WORKLOADS"

grep -Fxq 'default/deployment/portable' "$PREFLIGHT_CALLS"
if grep -Fq 'deployment/pinned' "$PREFLIGHT_CALLS"; then
  printf 'target-pinned workload was checked before its node returned\n' >&2
  exit 1
fi

wait_for_workloads "$WORKLOADS"

grep -Fq 'deployment/pinned' "$RETURN_CALLS"
grep -Fq 'deployment/portable' "$RETURN_CALLS"

printf 'k8s node reboot workload phase tests: PASS\n'
