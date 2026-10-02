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
PODS_JSON='{"items": []}'

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

  if [[ "$*" == *"get pods -o json" ]]; then
    printf '%s\n' "$PODS_JSON"
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

ONE_PER_NODE_JSON='{
  "spec": {"replicas": 3, "selector": {"matchLabels": {"app": "edge"}}, "template": {
    "metadata": {"labels": {"app": "edge", "tier": "core"}},
    "spec": {"affinity": {"podAntiAffinity": {
      "requiredDuringSchedulingIgnoredDuringExecution": [{
        "labelSelector": {"matchLabels": {"app": "edge"}},
        "topologyKey": "kubernetes.io/hostname"
      }]
    }}}
  }}
}'
ANTI_AFFINITY='.spec.template.spec.affinity.podAntiAffinity.requiredDuringSchedulingIgnoredDuringExecution[0]'

expect_floor() {
  local expected="$1"
  local reason="$2"
  local floor

  floor=$(deployment_ready_floor default deployment/fixture)
  if [[ "$floor" != "$expected" ]]; then
    printf '%s: floor was %s, expected %s\n' "$reason" "$floor" "$expected" >&2
    exit 1
  fi
}

WORKLOAD_JSON="$ONE_PER_NODE_JSON"
expect_floor 1 'one-per-node Deployment with one remaining node'

WORKLOAD_JSON=$(jq "${ANTI_AFFINITY}.labelSelector.matchLabels.app = \"other\"" <<<"$ONE_PER_NODE_JSON")
expect_floor 3 'anti-affinity against other Pods'

WORKLOAD_JSON=$(jq "${ANTI_AFFINITY}.topologyKey = \"topology.kubernetes.io/zone\"" <<<"$ONE_PER_NODE_JSON")
expect_floor 3 'non-hostname anti-affinity'

WORKLOAD_JSON=$(jq "${ANTI_AFFINITY}.mismatchLabelKeys = [\"tier\"]" <<<"$ONE_PER_NODE_JSON")
expect_floor 3 'anti-affinity narrowed by mismatchLabelKeys'

WORKLOAD_JSON='{"spec": {"replicas": 2, "template": {"spec": {}}}}'
expect_floor 2 'portable Deployment'

# NotIn admits nodes without the label, as the Kubernetes scheduler does, so
# the unlabelled worker-b can still host this workload.
WORKLOAD_JSON='{
  "spec": {"template": {"spec": {"affinity": {"nodeAffinity": {
    "requiredDuringSchedulingIgnoredDuringExecution": {"nodeSelectorTerms": [{
      "matchExpressions": [{
        "key": "browser-worker",
        "operator": "NotIn",
        "values": ["false"]
      }]
    }]}
  }}}}}
}'
if workload_requires_target_node default deployment/fixture; then
  printf 'NotIn affinity rejected a node without the label\n' >&2
  exit 1
fi

# No other eligible node still leaves a floor of one Ready replica.
WORKLOAD_JSON=$(jq '.spec.template.spec.nodeSelector = {"browser-worker": "true"}' <<<"$ONE_PER_NODE_JSON")
expect_floor 1 'one-per-node Deployment with no eligible remaining node'

# A cordoned node still running a Ready replica counts; an empty one does not.
NODES_JSON=$(jq '.items += [{
  "metadata": {"name": "worker-c", "labels": {"kubernetes.io/hostname": "worker-c"}},
  "spec": {"unschedulable": true},
  "status": {"conditions": [{"type": "Ready", "status": "True"}]}
}]' <<<"$NODES_JSON")
WORKLOAD_JSON="$ONE_PER_NODE_JSON"
expect_floor 1 'cordoned node without a replica'
PODS_JSON='{"items": [{
  "metadata": {"labels": {"app": "edge"}},
  "spec": {"nodeName": "worker-c"},
  "status": {"conditions": [{"type": "Ready", "status": "True"}]}
}]}'
expect_floor 2 'cordoned node still running a Ready replica'
PODS_JSON='{"items": [{
  "metadata": {"labels": {"app": "edge"}},
  "spec": {"nodeName": "worker-c"},
  "status": {"phase": "Running", "conditions": [{"type": "Ready", "status": "False"}]}
}]}'
expect_floor 2 'cordoned node running a replica that is not Ready yet'

PODS_JSON='{"items": []}'
WORKLOAD_JSON=$(jq '.spec.template.spec.tolerations = [{"key": "node.kubernetes.io/unschedulable", "operator": "Exists", "effect": "NoSchedule"}]' <<<"$ONE_PER_NODE_JSON")
expect_floor 3 'Pods that tolerate the cordon may return to the target node'

WORKLOAD_JSON=$(jq '.spec.template.spec.tolerations = [{"key": "node.kubernetes.io/unschedulable", "operator": "Equal", "value": "true", "effect": "NoSchedule"}]' <<<"$ONE_PER_NODE_JSON")
expect_floor 1 'a toleration whose value differs from the cordon taint'

# A Ready replica still counts on a node whose labels no longer match.
WORKLOAD_JSON=$(jq '.spec.template.spec.nodeSelector = {"browser-worker": "true"}' <<<"$ONE_PER_NODE_JSON")
PODS_JSON='{"items": [{
  "metadata": {"labels": {"app": "edge"}},
  "spec": {"nodeName": "worker-b"},
  "status": {"conditions": [{"type": "Ready", "status": "True"}]}
}, {
  "metadata": {"labels": {"app": "edge"}},
  "spec": {"nodeName": "worker-c"},
  "status": {"conditions": [{"type": "Ready", "status": "True"}]}
}]}'
expect_floor 2 'Ready replicas on nodes that no longer match the selector'
PODS_JSON='{"items": []}'

# Namespace Pod lists larger than one argv string must still be evaluated.
PODS_JSON=$(jq -n '{"items": [range(0; 400) | {
  "metadata": {"name": "filler-\(.)", "labels": {"app": "filler"}, "annotations": {"note": ("x" * 512)}},
  "spec": {"nodeName": "worker-b"},
  "status": {"conditions": [{"type": "Ready", "status": "True"}]}
}]}')
WORKLOAD_JSON="$ONE_PER_NODE_JSON"
expect_floor 1 'large namespace Pod list'
PODS_JSON='{"items": []}'

WORKLOADS="$TMP/workloads"
PREFLIGHT_CALLS="$TMP/preflight-calls"
RETURN_CALLS="$TMP/return-calls"

cat >"$WORKLOADS" <<'EOF'
default	deployment/pinned
default	deployment/portable
default	deployment/edge
default	deployment/unknown
EOF

workload_requires_target_node() {
  [[ "$2" == "deployment/pinned" ]]
}

deployment_ready_floor() {
  case "$2" in
    deployment/portable) printf '1' ;;
    deployment/edge) printf '2' ;;
    *) return 1 ;;
  esac
}

wait_for_controller_ready_floor() {
  printf '%s/%s %s\n' "$1" "$2" "$3" >>"$PREFLIGHT_CALLS"
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

  if [[ "$*" == *"get deployment/edge"* || "$*" == *"get deployment/unknown"* ]]; then
    printf '3'
    return 0
  fi

  printf 'unexpected kubectl call: %s\n' "$*" >&2
  return 1
}

wait_for_displaced_workload_survivability "$WORKLOADS"

grep -Fxq 'default/deployment/portable 1' "$PREFLIGHT_CALLS"
# A one-per-node floor is passed through; a failed floor lookup keeps every replica.
grep -Fxq 'default/deployment/edge 2' "$PREFLIGHT_CALLS"
grep -Fxq 'default/deployment/unknown 3' "$PREFLIGHT_CALLS"
if grep -Fq 'deployment/pinned' "$PREFLIGHT_CALLS"; then
  printf 'target-pinned workload was checked before its node returned\n' >&2
  exit 1
fi

wait_for_workloads "$WORKLOADS"

grep -Fq 'deployment/pinned' "$RETURN_CALLS"
grep -Fq 'deployment/portable' "$RETURN_CALLS"

printf 'k8s node reboot workload phase tests: PASS\n'
