#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
AUDIT="$ROOT/scripts/ops/k8s-node-network-audit.sh"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

cat >"$TMP/kubectl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [[ "$1 $2" == "get node" ]]; then
  if [[ "$*" == *flannel* ]]; then
    printf '%s' "$MOCK_FLANNEL_IP"
  else
    printf '%s\n' "$MOCK_NODE_IP"
  fi
elif [[ "$1 $2" == "get nodes" ]]; then
  printf 'nex\t%s \n' "$MOCK_NODE_IP"
  printf 'nux\t%s \n' "$MOCK_PEER_IP"
else
  exit 2
fi
EOF

cat >"$TMP/ip" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [[ "$*" == *"link show flannel.1"* ]]; then
  printf '13: flannel.1: vxlan id 1 local %s dev %s dstport 8472\n' \
    "$MOCK_FLANNEL_IP" "$MOCK_FLANNEL_INTERFACE"
elif [[ "$*" == *"route get"* ]]; then
  printf '%s dev %s src %s uid 0\n' \
    "$MOCK_PEER_IP" "$MOCK_ROUTE_INTERFACE" "$MOCK_ROUTE_SOURCE"
else
  exit 2
fi
EOF

chmod +x "$TMP/kubectl" "$TMP/ip"
for fixture in "$TMP/kubectl" "$TMP/ip"; do
  sed -i "1s|.*|#!$(command -v bash)|" "$fixture"
done

export KUBECTL_BIN="$TMP/kubectl"
export IP_BIN="$TMP/ip"
export MOCK_NODE_IP=192.168.1.16
export MOCK_PEER_IP=192.168.1.15
export MOCK_FLANNEL_IP=192.168.1.16
export MOCK_FLANNEL_INTERFACE=eno1
export MOCK_ROUTE_INTERFACE=eno1
export MOCK_ROUTE_SOURCE=192.168.1.16

run_audit() {
  bash "$AUDIT" \
    --node nex \
    --expected-node-ip 192.168.1.16 \
    --expected-interface eno1 \
    --disallowed-interface wt0
}

expect_failure() {
  local description="$1"
  shift
  if "$@" >/dev/null 2>&1; then
    printf 'expected failure: %s\n' "$description" >&2
    exit 1
  fi
}

run_audit | grep -q 'network path audit: PASS'

MOCK_NODE_IP=100.82.131.35
expect_failure 'unexpected InternalIP' run_audit
MOCK_NODE_IP=192.168.1.16

MOCK_FLANNEL_INTERFACE=wt0
expect_failure 'NetBird Flannel interface' run_audit
MOCK_FLANNEL_INTERFACE=eno1

MOCK_ROUTE_INTERFACE=wt0
expect_failure 'NetBird peer route' run_audit
MOCK_ROUTE_INTERFACE=eno1

MOCK_ROUTE_SOURCE=100.82.131.35
expect_failure 'unexpected peer-route source' run_audit

printf 'k8s node network audit tests: PASS\n'
