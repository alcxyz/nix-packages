#!/usr/bin/env bash
set -euo pipefail

NODE="${HOSTNAME:-}"
EXPECTED_NODE_IP=""
EXPECTED_INTERFACE=""
DISALLOWED_INTERFACES=("wt0")
KUBECTL_BIN="${KUBECTL_BIN:-}"
IP_BIN="${IP_BIN:-ip}"

usage() {
  cat <<'EOF'
Usage: k8s-node-network-audit --expected-node-ip <address> [options]

Fail-closed audit of the local Kubernetes node and Flannel underlay path.

Options:
  --node <name>                    Kubernetes node name. Defaults to hostname.
  --expected-node-ip <address>     Required LAN address for this node.
  --expected-interface <name>      Required Flannel and peer-route interface.
  --disallowed-interface <name>    Reject this interface. Repeatable.
  -h, --help                       Show this help.

Set KUBECTL_BIN or IP_BIN to override command discovery for tests.
EOF
}

die() {
  printf 'network path audit: FAIL: %s\n' "$*" >&2
  exit 1
}

need_command() {
  command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"
}

parse_args() {
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --node)
        [[ $# -ge 2 ]] || die "--node requires a value"
        NODE="$2"
        shift 2
        ;;
      --expected-node-ip)
        [[ $# -ge 2 ]] || die "--expected-node-ip requires a value"
        EXPECTED_NODE_IP="$2"
        shift 2
        ;;
      --expected-interface)
        [[ $# -ge 2 ]] || die "--expected-interface requires a value"
        EXPECTED_INTERFACE="$2"
        shift 2
        ;;
      --disallowed-interface)
        [[ $# -ge 2 ]] || die "--disallowed-interface requires a value"
        DISALLOWED_INTERFACES+=("$2")
        shift 2
        ;;
      -h | --help)
        usage
        exit 0
        ;;
      *)
        die "unknown argument: $1"
        ;;
    esac
  done

  [[ -n "$NODE" ]] || die "node name is empty"
  [[ -n "$EXPECTED_NODE_IP" ]] || die "--expected-node-ip is required"
}

select_kubectl() {
  if [[ -n "$KUBECTL_BIN" ]]; then
    need_command "$KUBECTL_BIN"
    KUBECTL=("$KUBECTL_BIN")
  elif command -v kubectl >/dev/null 2>&1; then
    KUBECTL=(kubectl)
  elif command -v k3s >/dev/null 2>&1; then
    KUBECTL=(k3s kubectl)
  else
    die "neither kubectl nor k3s is available"
  fi
}

interface_is_disallowed() {
  local interface="$1"
  local disallowed

  for disallowed in "${DISALLOWED_INTERFACES[@]}"; do
    [[ "$interface" != "$disallowed" ]] || return 0
  done
  return 1
}

field_after() {
  local field="$1"
  shift
  awk -v field="$field" '{
    for (idx = 1; idx <= NF; idx++) {
      if ($idx == field && idx < NF) {
        print $(idx + 1)
        exit
      }
    }
  }' <<<"$*"
}

main() {
  local internal_ip flannel_ip flannel_link flannel_interface
  local peer_rows peer_count=0 peer_name peer_addresses peer_ip
  local route route_interface route_source

  parse_args "$@"
  select_kubectl
  need_command "$IP_BIN"
  need_command awk
  need_command sort

  internal_ip=$(
    "${KUBECTL[@]}" get node "$NODE" \
      -o 'jsonpath={range .status.addresses[?(@.type=="InternalIP")]}{.address}{"\n"}{end}'
  )
  [[ "$(wc -l <<<"$internal_ip")" -eq 1 ]] ||
    die "${NODE} must advertise exactly one InternalIP"
  [[ "$internal_ip" == "$EXPECTED_NODE_IP" ]] ||
    die "${NODE} InternalIP ${internal_ip:-missing} does not match ${EXPECTED_NODE_IP}"

  flannel_ip=$(
    "${KUBECTL[@]}" get node "$NODE" \
      -o 'jsonpath={.metadata.annotations.flannel\.alpha\.coreos\.com/public-ip}'
  )
  [[ "$flannel_ip" == "$EXPECTED_NODE_IP" ]] ||
    die "${NODE} Flannel public IP ${flannel_ip:-missing} does not match ${EXPECTED_NODE_IP}"

  flannel_link=$("$IP_BIN" -d -o link show flannel.1)
  flannel_ip=$(field_after local "$flannel_link")
  flannel_interface=$(field_after dev "$flannel_link")
  [[ "$flannel_ip" == "$EXPECTED_NODE_IP" ]] ||
    die "Flannel VTEP ${flannel_ip:-missing} does not match ${EXPECTED_NODE_IP}"
  [[ -n "$flannel_interface" ]] || die "Flannel underlay interface is missing"
  ! interface_is_disallowed "$flannel_interface" ||
    die "Flannel selected disallowed interface ${flannel_interface}"
  if [[ -n "$EXPECTED_INTERFACE" ]]; then
    [[ "$flannel_interface" == "$EXPECTED_INTERFACE" ]] ||
      die "Flannel interface ${flannel_interface} does not match ${EXPECTED_INTERFACE}"
  fi

  peer_rows=$(
    "${KUBECTL[@]}" get nodes \
      -o 'jsonpath={range .items[*]}{.metadata.name}{"\t"}{range .status.addresses[?(@.type=="InternalIP")]}{.address}{" "}{end}{"\n"}{end}' |
      sort -u
  )

  while IFS=$'\t' read -r peer_name peer_addresses; do
    [[ -n "$peer_name" && "$peer_name" != "$NODE" ]] || continue
    for peer_ip in $peer_addresses; do
      [[ -n "$peer_ip" ]] || continue
      peer_count=$((peer_count + 1))
      route=$("$IP_BIN" -o route get "$peer_ip")
      route_interface=$(field_after dev "$route")
      route_source=$(field_after src "$route")

      [[ -n "$route_interface" ]] || die "route to ${peer_name}/${peer_ip} has no interface"
      ! interface_is_disallowed "$route_interface" ||
        die "route to ${peer_name}/${peer_ip} selected disallowed interface ${route_interface}"
      [[ "$route_interface" == "$flannel_interface" ]] ||
        die "route to ${peer_name}/${peer_ip} uses ${route_interface}, not Flannel underlay ${flannel_interface}"
      [[ "$route_source" == "$EXPECTED_NODE_IP" ]] ||
        die "route to ${peer_name}/${peer_ip} uses source ${route_source:-missing}, not ${EXPECTED_NODE_IP}"
    done
  done <<<"$peer_rows"

  [[ "$peer_count" -gt 0 ]] || die "no Kubernetes peer InternalIPs were found"
  printf 'network path audit: PASS node=%s internal_ip=%s flannel_interface=%s peers=%s\n' \
    "$NODE" "$EXPECTED_NODE_IP" "$flannel_interface" "$peer_count"
}

main "$@"
