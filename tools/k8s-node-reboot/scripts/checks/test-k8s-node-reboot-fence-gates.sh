#!/usr/bin/env bash
# Operations are imported first, then overridden for lifecycle failure tests.
# shellcheck disable=SC2218
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

# shellcheck disable=SC1091
source "$ROOT/scripts/ops/k8s-node-reboot.sh"

# PATH stubs cannot reach a real cluster or host, including on a failed test.
mkdir "$TMP/bin"
export FENCE_FIXTURES="$TMP"
export PATH="$TMP/bin:$PATH"
printf '#!%s\n' "$(command -v bash)" >"$TMP/bin/kubectl"
cat >>"$TMP/bin/kubectl" <<'STUB'
set -euo pipefail
printf 'kubectl %s\n' "$*" >>"$FENCE_FIXTURES/calls"
case "$*" in
  'annotate node server-a fence.alc.xyz/maintenance-baseline='*)
    [[ "$*" == *'--overwrite --field-manager=k8s-node-reboot --request-timeout=5s' ]] || exit 97
    [[ "$(cat "$FENCE_FIXTURES/baseline-write-exit")" == 0 ]] || exit 1
    jq --arg manager "$(cat "$FENCE_FIXTURES/baseline-manager")" \
      --arg time "$(cat "$FENCE_FIXTURES/baseline-time")" --arg value "${4#*=}" \
      '.metadata.annotations["fence.alc.xyz/maintenance-baseline"] = $value
       | .metadata.managedFields += [{manager:$manager,time:$time}]' \
      "$FENCE_FIXTURES/node.json" >"$FENCE_FIXTURES/changed.json"
    mv "$FENCE_FIXTURES/changed.json" "$FENCE_FIXTURES/node.json"
    ;;
  'annotate node server-a fence.alc.xyz/maintenance-baseline- --request-timeout=5s')
    [[ "$(cat "$FENCE_FIXTURES/baseline-cleanup-exit")" == 0 ]] || exit 1
    jq 'del(.metadata.annotations["fence.alc.xyz/maintenance-baseline"])' \
      "$FENCE_FIXTURES/node.json" >"$FENCE_FIXTURES/changed.json"
    mv "$FENCE_FIXTURES/changed.json" "$FENCE_FIXTURES/node.json"
    ;;
  'get node server-a -o json --show-managed-fields=true --request-timeout=5s')
    [[ "$(cat "$FENCE_FIXTURES/baseline-read-exit")" == 0 ]] || exit 1
    cat "$FENCE_FIXTURES/node.json"
    ;;
  'get node server-a' | 'get node server-a -o json'*) cat "$FENCE_FIXTURES/node.json" ;;
  'get nodes -o json') cat "$FENCE_FIXTURES/nodes.json" ;;
  'wait node/server-a --for=condition=Ready --timeout='*) : ;;
  'uncordon server-a')
    jq '.spec.unschedulable = false' "$FENCE_FIXTURES/node.json" >"$FENCE_FIXTURES/changed.json"
    mv "$FENCE_FIXTURES/changed.json" "$FENCE_FIXTURES/node.json"
    ;;
  *) printf 'unexpected kubectl: %s\n' "$*" >&2; exit 97 ;;
esac
STUB
printf '#!%s\n' "$(command -v bash)" >"$TMP/bin/ssh"
cat >>"$TMP/bin/ssh" <<'STUB'
set -euo pipefail
[[ "$*" == *'BatchMode=yes'* && "$*" == *'server-admin@maintenance-target'* ]] || exit 97
script=$(cat)
printf 'ssh %s\n' "$script" >>"$FENCE_FIXTURES/calls"
case "$script" in
  *'systemctl stop node-self-fence'*) exit "$(cat "$FENCE_FIXTURES/stop-exit")" ;;
  *'node-self-fence-status --json'*)
    cat "$FENCE_FIXTURES/status.json"
    exit "$(cat "$FENCE_FIXTURES/status-exit")"
    ;;
  *) printf 'unexpected ssh script\n' >&2; exit 97 ;;
esac
STUB
chmod +x "$TMP/bin/ssh" "$TMP/bin/kubectl"

export NODE=server-a
export SSH_TARGET=server-admin@maintenance-target
export ACTION=reboot
FENCE_TIMEOUT=0s
export FENCE_STOP_TIMEOUT_SECONDS=0

reset_fixtures() {
  cat >"$TMP/node.json" <<'JSON'
{"metadata":{"name":"server-a","annotations":{"fence.alc.xyz/agent-mode":"enforce"},"managedFields":[{"manager":"node-self-fence","time":"2026-01-01T00:03:00Z"}]},"spec":{"unschedulable":true},"status":{"conditions":[{"type":"Ready","status":"True","lastTransitionTime":"2026-01-01T00:02:00Z"}]}}
JSON
  jq -n --slurpfile node "$TMP/node.json" '{"node-role.kubernetes.io/control-plane": "true"} as $server
    | {items: [$node[0] | .metadata.labels = $server, {metadata:{name:"server-b",labels:$server},spec:{}},
      {metadata:{name:"agent-c"},spec:{}}]}' >"$TMP/nodes.json"
  cat >"$TMP/status.json" <<'JSON'
{"node":"server-a","servers":2,"quorum":2,"states":[{"address":"127.0.0.1","self":true,"ok":true,"node":"server-a","mode":"enforce","classification":"healthy","fenced":false,"since_unfence":null,"refence_window":900},{"address":"192.0.2.2","self":false,"ok":true,"node":"server-b","mode":"observe","classification":"healthy","fenced":false,"since_unfence":900,"refence_window":900}]}
JSON
  printf '0\n' >"$TMP/status-exit"
  printf '0\n' >"$TMP/stop-exit"
  printf '0\n' >"$TMP/baseline-write-exit"
  printf '0\n' >"$TMP/baseline-read-exit"
  printf '0\n' >"$TMP/baseline-cleanup-exit"
  printf 'k8s-node-reboot\n' >"$TMP/baseline-manager"
  printf '2026-01-01T00:04:00Z\n' >"$TMP/baseline-time"
  : >"$TMP/calls"
  FENCE_HAS_AGENT=false
  FENCE_PREVIOUS_MODE=""
  FENCE_STOPPED_AT=""
  FENCE_AGENT_STOPPED=false
  FENCE_TIMEOUT=0s
  ACTION=reboot
}

change_json() {
  local file="$1" filter="$2"
  jq "$filter" "$TMP/$file.json" >"$TMP/changed.json"
  mv "$TMP/changed.json" "$TMP/$file.json"
}

expect_failure() {
  local message="$1"
  shift
  local status
  set +e
  (set -e; "$@") >"$TMP/output" 2>&1
  status=$?
  set -e
  if [[ "$status" -eq 0 ]]; then
    printf 'expected failure containing: %s\n' "$message" >&2
    exit 1
  fi
  if ! rg -Fq -- "$message" "$TMP/output"; then
    cat "$TMP/output" >&2
    printf 'missing diagnostic: %s\n' "$message" >&2
    exit 1
  fi
}

reset_fixtures
verify_fence_preflight
[[ "$FENCE_HAS_AGENT" == true && "$FENCE_PREVIOUS_MODE" == enforce ]]
wait_for_returned_fence_agent
rg -Fq -- '--show-managed-fields=true' "$TMP/calls"
# A resumed target may already be cordoned; fence preflight is independent of it.
export RESUME_MAINTENANCE=true
verify_fence_preflight
RESUME_MAINTENANCE=false

reset_fixtures
change_json status '.states[1].fenced = true'
expect_failure 'server-b: fenced' verify_fence_preflight

# A server missing from the agent's peer list is not silently skipped.
reset_fixtures
change_json nodes '.items += [{metadata:{name:"server-c",labels:{"node-role.kubernetes.io/control-plane":"true"}},spec:{}}]'
expect_failure 'server-c: control-plane Node has no verified self-fence state' verify_fence_preflight

# The recheck before the power action keeps the pre-maintenance mode.
reset_fixtures
verify_fence_preflight
change_json status '.states[0].mode = "observe"'
expect_failure 'agent mode changed from enforce to observe' verify_fence_preflight

# Leading zeros would be read as octal by duration_to_seconds.
expect_failure 'unsupported fence duration: 08m' parse_args --fence-timeout 08m server-a

reset_fixtures
change_json status '.states[1] = {address:"192.0.2.2",self:false,ok:false,error:"unauthenticated"}'
printf '1\n' >"$TMP/status-exit"
expect_failure '192.0.2.2: unverified fence state (unauthenticated)' verify_fence_preflight

reset_fixtures
change_json status '.states[1].classification = "ambiguous"'
expect_failure 'server-b: classification ambiguous' verify_fence_preflight

reset_fixtures
change_json status '.states[1].since_unfence = 899'
expect_failure 'server-b: recent unfence (899s < refence window 900s)' verify_fence_preflight

reset_fixtures
change_json nodes '.items[1].metadata.annotations["fence.alc.xyz/disabled"] = "true"'
expect_failure 'server-b: fence.alc.xyz/disabled=true' verify_fence_preflight

reset_fixtures
change_json nodes '.items[1].spec.taints = [{key:"node.kubernetes.io/out-of-service",effect:"NoExecute"}]'
expect_failure 'server-b: node.kubernetes.io/out-of-service taint' verify_fence_preflight

reset_fixtures
printf '127\n' >"$TMP/status-exit"
printf '' >"$TMP/status.json"
expect_failure 'self-fence agent is not deployed' verify_fence_preflight

reset_fixtures
printf '2\n' >"$TMP/status-exit"
expect_failure 'node-self-fence-status failed (exit 2)' verify_fence_preflight

reset_fixtures
printf 'invalid json\n' >"$TMP/status.json"
expect_failure 'invalid or unavailable self-fence status' verify_fence_preflight

reset_fixtures
cat "$TMP/status.json" "$TMP/status.json" >"$TMP/duplicate.json"
mv "$TMP/duplicate.json" "$TMP/status.json"
expect_failure 'invalid or unavailable self-fence status' verify_fence_preflight

reset_fixtures
change_json status 'del(.states[1].since_unfence)'
expect_failure 'server-b: missing or invalid unfence timing' verify_fence_preflight

reset_fixtures
change_json status '.states = []'
expect_failure 'invalid or unavailable self-fence status' verify_fence_preflight

reset_fixtures
change_json status '.states |= map(select(.self))'
expect_failure 'incomplete self-fence server states' verify_fence_preflight

reset_fixtures
change_json node 'del(.metadata.annotations["fence.alc.xyz/agent-mode"])'
verify_fence_preflight >"$TMP/output"
stop_fence_agent
wait_for_returned_fence_agent
[[ "$FENCE_HAS_AGENT" == false ]]
rg -Fq 'skipping all fence gates' "$TMP/output"
[[ "$(wc -l <"$TMP/calls")" -eq 1 ]]
# Any annotation value marks an enabled agent.
change_json node '.metadata.annotations["fence.alc.xyz/agent-mode"] = ""'
verify_fence_preflight

reset_fixtures
verify_fence_preflight
change_json node '.metadata.annotations["fence.alc.xyz/agent-mode"] = "stopped"'
stop_fence_agent
rg -Fq 'systemctl stop node-self-fence' "$TMP/calls"
[[ "$FENCE_AGENT_STOPPED" == true && "$FENCE_STOPPED_AT" == "$(jq -n '"2026-01-01T00:03:00Z" | fromdateiso8601')" ]]

# A peer that fences while the target drains blocks the power action.
reset_fixtures
verify_fence_preflight
change_json status '.states[1].fenced = true | .states[1].classification = "fenced"'
expect_failure 'fenced or missing fenced state' stop_fence_agent
if rg -Fq 'systemctl stop node-self-fence' "$TMP/calls"; then exit 1; fi

reset_fixtures
verify_fence_preflight
expect_failure 'agent did not publish mode=stopped' stop_fence_agent
rg -Fq 'remains cordoned' "$TMP/output"
rg -Fq 'systemctl start node-self-fence' "$TMP/output"
printf '1\n' >"$TMP/stop-exit"
expect_failure 'failed to stop node-self-fence' stop_fence_agent

reset_fixtures
verify_fence_preflight
change_json status '.states[0].classification = "grace"'
expect_failure 'own agent is not healthy' wait_for_returned_fence_agent
rg -Fq 'remains cordoned' "$TMP/output"

reset_fixtures
verify_fence_preflight
change_json status '.states[0].fenced = true'
expect_failure 'fenced=true' wait_for_returned_fence_agent

reset_fixtures
verify_fence_preflight
printf '2\n' >"$TMP/status-exit"
expect_failure 'node-self-fence-status failed (exit 2)' wait_for_returned_fence_agent

reset_fixtures
verify_fence_preflight
change_json status '.states[0].mode = "observe"'
expect_failure 'differs from pre-maintenance mode enforce' wait_for_returned_fence_agent
ACTION=on
change_json node '.metadata.annotations["fence.alc.xyz/agent-mode"] = "observe"'
wait_for_returned_fence_agent >"$TMP/output"
rg -Fq 'recovered in observe mode' "$TMP/output"
ACTION=reboot

# The pre-power-action stop also writes with the agent's field manager; it is
# not a heartbeat.
reset_fixtures
verify_fence_preflight
change_json node '.metadata.annotations["fence.alc.xyz/agent-mode"] = "stopped"'
expect_failure 'no node-self-fence heartbeat after the maintenance baseline' wait_for_returned_fence_agent

reset_fixtures
verify_fence_preflight
# Return gates require only the own entry; an unverified peer is not a blocker.
change_json status '.states[1].ok = false | .states[1].error = "no answer"'
printf '1\n' >"$TMP/status-exit"
wait_for_returned_fence_agent

# Heartbeats use an API-server baseline, independent of the kubelet clock.
for timestamp in '2026-01-01T00:02:00Z' '2026-01-01T00:03:00Z'; do
  reset_fixtures
  verify_fence_preflight
  FENCE_STOPPED_AT=$(jq -n '"2026-01-01T00:03:00Z" | fromdateiso8601')
  change_json node ".metadata.managedFields[0].time = \"$timestamp\""
  expect_failure 'no node-self-fence heartbeat after the maintenance baseline' wait_for_returned_fence_agent
done
reset_fixtures
verify_fence_preflight
FENCE_STOPPED_AT=$(jq -n '"2026-01-01T00:02:30Z" | fromdateiso8601')
change_json node '.status.conditions[0].lastTransitionTime = "2026-01-01T00:09:00Z"'
wait_for_returned_fence_agent
reset_fixtures
verify_fence_preflight
change_json node '.metadata.managedFields = [{manager:"other-manager",time:"2026-01-01T00:04:00Z"}]'
expect_failure 'no node-self-fence heartbeat after the maintenance baseline' wait_for_returned_fence_agent

# Fence preflight must run in both check-only paths before other maintenance gates.
verify_remote_privilege() { :; }
verify_all_node_network_paths() { die 'network gate reached before fence failure'; }
reset_fixtures
change_json status '.states[1].fenced = true'
for RESUME_MAINTENANCE in false true; do
  expect_failure 'server-b: fenced' run_check_only
  expect_failure 'server-b: fenced' prepare_node_for_disruption "$TMP/workloads"
done
if rg -q 'kubectl (cordon|uncordon|drain)|systemctl stop' "$TMP/calls"; then
  printf 'failed preflight attempted a mutation\n' >&2
  exit 1
fi

parse_args --fence-timeout 3m --check-only server-a
[[ "$FENCE_TIMEOUT" == 3m && "$CHECK_ONLY" == true ]]
# Lifecycle failures must stop before power or uncordon. These doubles are
# invoked by the sourced lifecycle functions and never call real clients.
# shellcheck disable=SC2329
prepare_node_for_disruption() { :; }
# shellcheck disable=SC2329,SC2317
stop_fence_agent() { die 'fixture stop gate failed'; }
# shellcheck disable=SC2329
reboot_host() { printf 'power action\n' >>"$TMP/phases"; }
# shellcheck disable=SC2329
poweroff_host() { printf 'power action\n' >>"$TMP/phases"; }
: >"$TMP/phases"
expect_failure 'fixture stop gate failed' run_reboot
expect_failure 'fixture stop gate failed' run_poweroff
[[ ! -s "$TMP/phases" ]]

# shellcheck disable=SC2329
stop_fence_agent() { :; }
# shellcheck disable=SC2329
wait_for_ssh_down() { :; }
# shellcheck disable=SC2329
verify_survivor_node_network_paths() { :; }
# shellcheck disable=SC2329
wait_for_ssh_up() { :; }
# shellcheck disable=SC2329
verify_new_boot() { :; }
# shellcheck disable=SC2329
wait_for_returned_node_network() { :; }
# shellcheck disable=SC2329
wait_for_longhorn_survivability() { :; }
# shellcheck disable=SC2329
wait_for_cloudnativepg_survivability() { :; }
# shellcheck disable=SC2329
verify_node_is_cordoned() { :; }
# shellcheck disable=SC2329
collect_target_pinned_pending_workloads() { :; }
# shellcheck disable=SC2329
wait_for_workloads() { :; }
# shellcheck disable=SC2329
wait_for_longhorn_health() { :; }
# shellcheck disable=SC2329
wait_for_cloudnativepg_health() { :; }
# shellcheck disable=SC2329
wait_for_no_bad_pods() { :; }

assert_still_cordoned() {
  jq -e '.spec.unschedulable == true' >/dev/null "$TMP/node.json"
  if rg -Fq 'kubectl uncordon' "$TMP/calls"; then
    printf 'fence failure attempted to uncordon\n' >&2
    exit 1
  fi
  rg -Fq 'remains cordoned' "$TMP/output"
}

# A new kon process must require a heartbeat after its API-clock baseline even
# when an outage left mode=enforce behind. Equal timestamps are stale too.
for timestamp in '2026-01-01T00:03:00Z' '2026-01-01T00:04:00Z'; do
  reset_fixtures
  ACTION=on
  change_json node ".metadata.managedFields[0].time = \"$timestamp\""
  expect_failure 'no node-self-fence heartbeat after the maintenance baseline' run_poweron_finalize
  assert_still_cordoned
  rg -Fq 'kubectl annotate node server-a fence.alc.xyz/maintenance-baseline=' "$TMP/calls"
  rg -Fq 'kubectl annotate node server-a fence.alc.xyz/maintenance-baseline-' "$TMP/calls"
  jq -e '.metadata.annotations | has("fence.alc.xyz/maintenance-baseline") | not' >/dev/null "$TMP/node.json"
done

reset_fixtures
ACTION=on
change_json node '.metadata.managedFields[0].time = "2026-01-01T00:05:00Z"
  | .status.conditions[0].lastTransitionTime = "2026-01-01T00:09:00Z"'
(run_poweron_finalize
 [[ "$FENCE_STOPPED_AT" == "$(jq -n '"2026-01-01T00:04:00Z" | fromdateiso8601')" ]])
rg -Fq 'kubectl uncordon server-a' "$TMP/calls"
jq -e '.spec.unschedulable == false
  and (.metadata.annotations | has("fence.alc.xyz/maintenance-baseline") | not)' >/dev/null "$TMP/node.json"

# Baseline write/read/cleanup errors and missing or invalid manager timestamps
# fail closed even if the agent's old heartbeat otherwise looks healthy.
for failure in write read cleanup missing-manager invalid-time; do
  reset_fixtures
  ACTION=on
  case "$failure" in
    write) printf '1\n' >"$TMP/baseline-write-exit" ;;
    read) printf '1\n' >"$TMP/baseline-read-exit" ;;
    cleanup) printf '1\n' >"$TMP/baseline-cleanup-exit" ;;
    missing-manager) printf 'other-manager\n' >"$TMP/baseline-manager" ;;
    invalid-time) printf 'invalid\n' >"$TMP/baseline-time" ;;
  esac
  expect_failure 'could not establish self-fence heartbeat baseline' run_poweron_finalize
  assert_still_cordoned
  if [[ "$failure" == read || "$failure" == missing-manager || "$failure" == invalid-time ]]; then
    rg -Fq 'kubectl annotate node server-a fence.alc.xyz/maintenance-baseline-' "$TMP/calls"
  fi
done

# Agentless targets skip the baseline write and recovery gate.
reset_fixtures
ACTION=on
change_json node 'del(.metadata.annotations["fence.alc.xyz/agent-mode"])'
(run_poweron_finalize)
rg -Fq 'kubectl uncordon server-a' "$TMP/calls"
if rg -Fq 'kubectl annotate' "$TMP/calls"; then exit 1; fi

# Keep the lifecycle failure checks using the same kubectl PATH stub.
reset_fixtures
printf '2026-01-01T00:02:30Z\n' >"$TMP/baseline-time"
# shellcheck disable=SC2329
wait_for_returned_fence_agent() { die 'fixture recovery gate failed'; }
export LEAVE_CORDONED=false
expect_failure 'fixture recovery gate failed' run_reboot
expect_failure 'fixture recovery gate failed' run_poweron_finalize
if rg -Fq 'uncordon' "$TMP/calls"; then
  printf 'recovery failure attempted to uncordon\n' >&2
  exit 1
fi

printf 'k8s node reboot self-fence gate tests: PASS\n'
