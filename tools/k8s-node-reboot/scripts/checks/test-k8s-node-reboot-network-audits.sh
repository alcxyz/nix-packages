#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

# shellcheck disable=SC1091
source "$ROOT/scripts/ops/k8s-node-reboot.sh"

NODE=nex
SSH_TARGET=admin@nex-maintenance
NODE_SSH_USER=root
CALLS="$TMP/calls"

kubectl() {
  if [[ "$*" == "get nodes -o json" ]]; then
    cat <<'EOF'
{
  "items": [
    {"metadata":{"name":"nex"},"status":{"addresses":[{"type":"InternalIP","address":"192.0.2.16"}],"conditions":[{"type":"Ready","status":"True"}]}},
    {"metadata":{"name":"nux"},"status":{"addresses":[{"type":"InternalIP","address":"192.0.2.15"}],"conditions":[{"type":"Ready","status":"True"}]}},
    {"metadata":{"name":"xev"},"status":{"addresses":[{"type":"InternalIP","address":"192.0.2.13"}],"conditions":[{"type":"Ready","status":"True"}]}}
  ]
}
EOF
    return 0
  fi
  printf 'unexpected kubectl call: %s\n' "$*" >&2
  return 1
}

verify_network_path_on_node() {
  printf '%s\t%s\t%s\n' "$1" "$2" "$3" >>"$CALLS"
}

verify_all_node_network_paths
grep -Fxq $'nex\t192.0.2.16\tadmin@nex-maintenance' "$CALLS"
grep -Fxq $'nux\t192.0.2.15\troot@nux' "$CALLS"
grep -Fxq $'xev\t192.0.2.13\troot@xev' "$CALLS"
[[ "$(wc -l <"$CALLS")" -eq 3 ]]

: >"$CALLS"
verify_survivor_node_network_paths
if grep -Fq $'nex\t' "$CALLS"; then
  printf 'target node was included in survivor audits\n' >&2
  exit 1
fi
grep -Fxq $'nux\t192.0.2.15\troot@nux' "$CALLS"
grep -Fxq $'xev\t192.0.2.13\troot@xev' "$CALLS"
[[ "$(wc -l <"$CALLS")" -eq 2 ]]

# Node Ready can precede Flannel startup. Wait for the interface, then retain
# the strict audit; neither timeout nor audit failure may advance recovery.
attempts=0
audits=0
sleeps=0
READY_TIMEOUT=30s
ssh() {
  [[ "$*" == *"ip link show dev flannel.1"* ]] || return 97
  attempts=$((attempts + 1))
  ((attempts >= 3))
}
sleep() { sleeps=$((sleeps + 1)); }
verify_returned_node_network() { audits=$((audits + 1)); }
wait_for_returned_node_network
[[ "$attempts" -eq 3 && "$sleeps" -eq 2 && "$audits" -eq 1 ]]

ssh() { return 1; }
READY_TIMEOUT=0s
if (wait_for_returned_node_network) >"$TMP/timeout" 2>&1; then
  printf 'missing Flannel interface unexpectedly passed\n' >&2
  exit 1
fi
grep -Fq 'node remains cordoned' "$TMP/timeout"

ssh() { return 0; }
verify_returned_node_network() { die 'fixture strict network audit failed'; }
if (wait_for_returned_node_network) >"$TMP/audit-failure" 2>&1; then
  printf 'strict network audit failure unexpectedly passed\n' >&2
  exit 1
fi
grep -Fq 'fixture strict network audit failed' "$TMP/audit-failure"

printf 'k8s node reboot network audit fanout and readiness tests: PASS\n'
