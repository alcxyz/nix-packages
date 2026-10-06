#!/usr/bin/env bash
set -euo pipefail

HOST=""
NODE=""
SSH_TARGET=""
ACTION=""
SKIP_DRAIN=false
FORCE_DRAIN=false
BYPASS_PDB=false
LEAVE_CORDONED=false
RESUME_MAINTENANCE=false
CHECK_ONLY=false
DRAIN_TIMEOUT="10m"
READY_TIMEOUT="10m"
FENCE_TIMEOUT="10m"
FENCE_STOP_TIMEOUT_SECONDS=60
FENCE_HAS_AGENT=false
FENCE_PREVIOUS_MODE=""
FENCE_STATUS_JSON=""
FENCE_STATUS_EXIT=0
FENCE_FAILURE=""
FENCE_AGENT_STOPPED=false
FENCE_STOPPED_AT=""
# Longhorn deliberately waits up to 30 minutes for a returning node so it can
# reuse failed replicas instead of creating replacements. A cluster with
# automatic rebuild admission disabled then needs time for its guarded,
# sequential recovery queue.
SETTLE_TIMEOUT="90m"
SSH_TIMEOUT_SECONDS=900
POLL_SECONDS=5
REQUIRE_LONGHORN_BACKUP_TARGET=false
MIN_FREE_POD_SLOTS=20
MIN_LONGHORN_REPLICAS=3
MIN_SURVIVING_LONGHORN_REPLICAS=2
REMOTE_BOOT_ID=""
WORKLOADS_FILE=""
NETWORK_AUDIT_SCRIPT="${K8S_NODE_NETWORK_AUDIT_SCRIPT:-}"
NODE_SSH_USER="${K8S_NODE_SSH_USER:-root}"
CNPG_SWITCHOVERS=""
LONGHORN_REBUILD_LIMIT_ORIGINAL=""

usage() {
  cat <<'EOF'
Usage: kreboot [options] <host>
       koff [options] <host>
       kon [options] <host>

Kubernetes-aware node power helpers.

Commands:
  kreboot                  Move database primaries, cordon, drain, reboot,
                           wait for Ready, and uncordon.
  koff                     Move database primaries, cordon, drain, power off,
                           and leave cordoned.
  kon                      Wait for host/node to return, uncordon, settle.

Options:
  --node <name>            Kubernetes node name. Defaults to <host>.
  --ssh-target <target>    SSH target. Defaults to root@<host>.
  --skip-drain             Cordon only, then reboot. Use for intentional disruption.
  --force-drain            Pass --force to kubectl drain for unmanaged pods.
  --bypass-pdb             Use pod deletion instead of eviction; ignores PDBs.
  --leave-cordoned         Keep the node cordoned after it returns.
  --resume-maintenance     Reuse an already cordoned, drained, and detached
                           node for another power cycle. Fails closed if the
                           maintenance-state gates do not pass.
  --check-only             Validate preflight or resumed-maintenance gates and
                           report planned database switchovers without changes.
  --drain-timeout <dur>    kubectl drain duration. Default: 10m.
  --ready-timeout <dur>    kubectl wait duration. Default: 10m.
  --fence-timeout <dur>    Agent recovery/heartbeat wait duration. Default: 10m.
  --settle-timeout <dur>   Workload/Longhorn health wait duration. Default: 90m.
  --ssh-timeout <seconds>  SSH return timeout. Default: 900.
  --require-longhorn-backup-target
                           Fail if the Longhorn backup target is unavailable.
  --min-free-pod-slots N   Require N aggregate Pod slots after evacuation on
                           the remaining stable nodes.
                           Default: 20.
  -h, --help               Show this help.

Agent-enabled targets require healthy, verified fence states on every server
before maintenance, with no recent unfence, disabled annotation, or out-of-service
taint. The target agent must publish stopped before reboot/off. Before uncordon,
its healthy agent must return in the original mode (kon accepts either mode)
and publish a heartbeat recorded by the API server after the agent was stopped
or, for kon, after a new maintenance baseline established once the node is Ready.
Fence failures after cordon leave the node cordoned. Targets without an agent
annotation skip these gates. --check-only also checks fence preflight.

Requires kubectl access to the target cluster and SSH sudo rights on the host.
EOF
}

log() {
  printf '==> %s\n' "$*"
}

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

cleanup_workloads_file() {
  if [[ -n "$WORKLOADS_FILE" ]]; then
    rm -f -- "$WORKLOADS_FILE"
  fi
}

restore_longhorn_rebuild_limit() {
  local original="$LONGHORN_REBUILD_LIMIT_ORIGINAL"

  [[ -n "$original" ]] || return 0
  log "restoring Longhorn replica rebuild admission to ${original}"
  if kubectl -n longhorn-system patch settings.longhorn.io \
    concurrent-replica-rebuild-per-node-limit \
    --type=merge \
    -p "{\"value\":\"${original}\"}" >/dev/null; then
    LONGHORN_REBUILD_LIMIT_ORIGINAL=""
    return 0
  fi

  printf 'error: failed to restore Longhorn replica rebuild admission to %s\n' \
    "$original" >&2
  return 1
}

cleanup_runtime_state() {
  local status=$?

  trap - EXIT
  if [[ "$FENCE_AGENT_STOPPED" == true ]]; then
    log "node-self-fence is still stopped on ${NODE}; start it with systemctl start node-self-fence before retrying maintenance"
  fi
  restore_longhorn_rebuild_limit || status=1
  cleanup_workloads_file
  exit "$status"
}

need_command() {
  command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"
}

duration_to_seconds() {
  local duration="$1"

  case "$duration" in
    *s)
      printf '%s\n' "${duration%s}"
      ;;
    *m)
      printf '%s\n' "$((${duration%m} * 60))"
      ;;
    *h)
      printf '%s\n' "$((${duration%h} * 3600))"
      ;;
    *[!0-9]*)
      die "unsupported duration: ${duration}"
      ;;
    *)
      printf '%s\n' "$duration"
      ;;
  esac
}

parse_args() {
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --node)
        [[ $# -ge 2 ]] || die "--node requires a value"
        NODE="$2"
        shift 2
        ;;
      --ssh-target)
        [[ $# -ge 2 ]] || die "--ssh-target requires a value"
        SSH_TARGET="$2"
        shift 2
        ;;
      --skip-drain)
        SKIP_DRAIN=true
        shift
        ;;
      --force-drain)
        FORCE_DRAIN=true
        shift
        ;;
      --bypass-pdb)
        BYPASS_PDB=true
        shift
        ;;
      --leave-cordoned)
        LEAVE_CORDONED=true
        shift
        ;;
      --resume-maintenance)
        RESUME_MAINTENANCE=true
        shift
        ;;
      --check-only)
        CHECK_ONLY=true
        shift
        ;;
      --drain-timeout)
        [[ $# -ge 2 ]] || die "--drain-timeout requires a value"
        DRAIN_TIMEOUT="$2"
        shift 2
        ;;
      --ready-timeout)
        [[ $# -ge 2 ]] || die "--ready-timeout requires a value"
        READY_TIMEOUT="$2"
        shift 2
        ;;
      --fence-timeout)
        [[ $# -ge 2 ]] || die "--fence-timeout requires a value"
        # No leading zeros: bash arithmetic would read them as octal.
        [[ "$2" =~ ^([1-9][0-9]*|0)[smh]?$ ]] || die "unsupported fence duration: $2"
        FENCE_TIMEOUT="$2"
        shift 2
        ;;
      --settle-timeout)
        [[ $# -ge 2 ]] || die "--settle-timeout requires a value"
        SETTLE_TIMEOUT="$2"
        shift 2
        ;;
      --ssh-timeout)
        [[ $# -ge 2 ]] || die "--ssh-timeout requires a value"
        SSH_TIMEOUT_SECONDS="$2"
        shift 2
        ;;
      --require-longhorn-backup-target)
        REQUIRE_LONGHORN_BACKUP_TARGET=true
        shift
        ;;
      --min-free-pod-slots)
        [[ $# -ge 2 ]] || die "--min-free-pod-slots requires a value"
        [[ "$2" =~ ^[1-9][0-9]*$ ]] || die "--min-free-pod-slots must be a positive integer"
        MIN_FREE_POD_SLOTS="$2"
        shift 2
        ;;
      -h | --help)
        usage
        exit 0
        ;;
      --*)
        die "unknown option: $1"
        ;;
      *)
        [[ -z "$HOST" ]] || die "only one host can be supplied"
        HOST="$1"
        shift
        ;;
    esac
  done

  [[ -n "$HOST" ]] || {
    usage >&2
    exit 2
  }

  NODE="${NODE:-$HOST}"
  SSH_TARGET="${SSH_TARGET:-root@$HOST}"

  ACTION="${K8S_NODE_POWER_ACTION:-}"

  if [[ -z "$ACTION" ]]; then
    case "$(basename "$0")" in
      koff)
        ACTION="off"
        ;;
      kon)
        ACTION="on"
        ;;
      kreboot | k8s-node-reboot | k8s-node-reboot.sh)
        ACTION="reboot"
        ;;
      *)
        ACTION="reboot"
        ;;
    esac
  fi

  if [[ "$ACTION" == "off" ]]; then
    LEAVE_CORDONED=true
  fi
}

workload_key_for_pod() {
  local namespace="$1"
  local pod="$2"
  local owner_kind
  local owner_name
  local rs_owner_kind
  local rs_owner_name

  owner_kind=$(
    kubectl -n "$namespace" get pod "$pod" -o jsonpath='{.metadata.ownerReferences[0].kind}' 2>/dev/null || true
  )
  owner_name=$(
    kubectl -n "$namespace" get pod "$pod" -o jsonpath='{.metadata.ownerReferences[0].name}' 2>/dev/null || true
  )

  case "$owner_kind" in
    DaemonSet | Job | Node | "")
      return 0
      ;;
    StatefulSet)
      printf '%s\tstatefulset/%s\n' "$namespace" "$owner_name"
      ;;
    ReplicaSet)
      rs_owner_kind=$(
        kubectl -n "$namespace" get replicaset "$owner_name" -o jsonpath='{.metadata.ownerReferences[0].kind}' 2>/dev/null || true
      )
      rs_owner_name=$(
        kubectl -n "$namespace" get replicaset "$owner_name" -o jsonpath='{.metadata.ownerReferences[0].name}' 2>/dev/null || true
      )

      if [[ "$rs_owner_kind" == "Deployment" && -n "$rs_owner_name" ]]; then
        printf '%s\tdeployment/%s\n' "$namespace" "$rs_owner_name"
      else
        printf '%s\treplicaset/%s\n' "$namespace" "$owner_name"
      fi
      ;;
    *)
      printf '%s\t%s/%s\n' "$namespace" "${owner_kind,,}" "$owner_name"
      ;;
  esac
}

collect_displaced_workloads() {
  local pod_list
  local namespace
  local pod

  pod_list=$(mktemp)
  kubectl get pods --all-namespaces \
    --field-selector "spec.nodeName=${NODE}" \
    -o jsonpath='{range .items[*]}{.metadata.namespace}{"\t"}{.metadata.name}{"\n"}{end}' >"$pod_list"

  while IFS=$'\t' read -r namespace pod; do
    [[ -n "$namespace" && -n "$pod" ]] || continue
    workload_key_for_pod "$namespace" "$pod"
  done <"$pod_list" | sort -u

  rm -f "$pod_list"
}

# Node-matching jq definitions shared by the workload placement checks.
# shellcheck disable=SC2016
NODE_MATCH_JQ_DEFS='
      def requirement_matches($value; $requirement):
        ($requirement.operator // "") as $operator
        | ($requirement.values // []) as $values
        | if $operator == "In" then
            $value != null and ($values | index($value) != null)
          elif $operator == "NotIn" then
            $value == null or ($values | index($value) == null)
          elif $operator == "Exists" then
            $value != null
          elif $operator == "DoesNotExist" then
            $value == null
          elif $operator == "Gt" then
            $value != null
            and (($value | tonumber? // null) as $actual
              | (($values[0] // "") | tonumber? // null) as $expected
              | $actual != null and $expected != null and $actual > $expected)
          elif $operator == "Lt" then
            $value != null
            and (($value | tonumber? // null) as $actual
              | (($values[0] // "") | tonumber? // null) as $expected
              | $actual != null and $expected != null and $actual < $expected)
          else
            false
          end;

      def node_selector_matches($node; $selector):
        ($selector // {})
        | to_entries
        | all(. as $entry
          | ($node.metadata.labels[$entry.key] // null) == $entry.value);

      def node_affinity_term_matches($node; $term):
        all(($term.matchExpressions // [])[];
          . as $requirement
          | requirement_matches(
              ($node.metadata.labels[$requirement.key] // null);
              $requirement
            ))
        and all(($term.matchFields // [])[];
          . as $requirement
          | requirement_matches(
              (if $requirement.key == "metadata.name" then
                $node.metadata.name
              else
                null
              end);
              $requirement
            ));

      def required_node_affinity_matches($node; $affinity):
        ($affinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms // []) as $terms
        | ($terms | length) == 0
          or any($terms[]; node_affinity_term_matches($node; .));

      def pod_matches_node($pod; $node):
        node_selector_matches($node; $pod.nodeSelector)
        and required_node_affinity_matches($node; $pod.affinity.nodeAffinity // {});

      def node_is_ready($node):
        any(($node.status.conditions // [])[];
          .type == "Ready" and .status == "True");
'

workload_requires_target_node() {
  local namespace="$1"
  local workload="$2"
  local workload_json
  local nodes_json

  workload_json=$(kubectl -n "$namespace" get "$workload" -o json 2>/dev/null) || return 1
  nodes_json=$(kubectl get nodes -o json 2>/dev/null) || return 1

  jq -en \
    --arg node "$NODE" \
    --argjson workload "$workload_json" \
    --argjson nodes "$nodes_json" "${NODE_MATCH_JQ_DEFS}"'
      ($workload.spec.template.spec // {}) as $pod
      | ($nodes.items // []) as $all_nodes
      | ($all_nodes | map(select(.metadata.name == $node)) | first) as $target
      | $target != null
        and pod_matches_node($pod; $target)
        and ([$all_nodes[]
          | select(.metadata.name != $node)
          | select(.spec.unschedulable != true)
          | select(node_is_ready(.))
          | select(pod_matches_node($pod; .))]
          | length) == 0' >/dev/null
}

# Prints the Ready-replica floor a Deployment can meet while NODE is out. A
# required one-per-node anti-affinity on the pods' own labels caps the floor at
# the other Ready nodes the pods can use: schedulable ones, plus nodes still
# running a live replica. The floor never drops below one. Pods that tolerate
# the cordon (they may land back on NODE) and selector fields this check does
# not model keep the full replica count.
# A strict hostname spread instead replays the scheduler's skew check to find
# how many replicas can still run with NODE out.
deployment_ready_floor() {
  local namespace="$1"
  local workload="$2"
  local snapshot
  local status=0

  # Namespace Pod and Node lists can exceed the per-argument size limit, so
  # hand them to jq as files.
  snapshot=$(mktemp -d)
  if kubectl -n "$namespace" get "$workload" -o json >"${snapshot}/workload.json" 2>/dev/null &&
    kubectl get nodes -o json >"${snapshot}/nodes.json" 2>/dev/null &&
    kubectl -n "$namespace" get pods -o json >"${snapshot}/pods.json" 2>/dev/null; then
    deployment_ready_floor_from_files "$namespace" "$snapshot" || status=$?
  else
    status=1
  fi
  rm -rf "$snapshot"
  return "$status"
}

deployment_ready_floor_from_files() {
  local namespace="$1"
  local snapshot="$2"

  jq -enr \
    --arg node "$NODE" \
    --arg namespace "$namespace" \
    --slurpfile workload_doc "${snapshot}/workload.json" \
    --slurpfile nodes_doc "${snapshot}/nodes.json" \
    --slurpfile pods_doc "${snapshot}/pods.json" "${NODE_MATCH_JQ_DEFS}"'
      def labels_match($labels; $match):
        ($match // {}) | to_entries | all(($labels[.key] // null) == .value);

      def one_per_node($pod; $labels):
        any(($pod.affinity.podAntiAffinity.requiredDuringSchedulingIgnoredDuringExecution // [])[];
          .topologyKey == "kubernetes.io/hostname"
          and .namespaceSelector == null
          and ((.namespaces // []) | length == 0 or index($namespace) != null)
          and (.matchLabelKeys // []) == []
          and (.mismatchLabelKeys // []) == []
          and (.labelSelector.matchExpressions // []) == []
          and ((.labelSelector.matchLabels // {}) | length > 0)
          and labels_match($labels; .labelSelector.matchLabels));

      # The strict hostname spread on the Pod template labels, if any. Other
      # spread forms keep the full replica count.
      def hostname_spread($pod; $labels):
        [($pod.topologySpreadConstraints // [])[] | select(.whenUnsatisfiable == "DoNotSchedule")]
        | if length == 1
            and (.[0]
              | .topologyKey == "kubernetes.io/hostname"
              and ((.maxSkew // 0) >= 1)
              and (.nodeAffinityPolicy // "Honor") == "Honor"
              and .minDomains == null
              and (.matchLabelKeys // []) == []
              and (.labelSelector.matchExpressions // []) == []
              and ((.labelSelector.matchLabels // {}) | length > 0)
              and labels_match($labels; .labelSelector.matchLabels)) then
            .[0]
          else
            null
          end;

      def tolerates_taint($pod; $taint):
        any(($pod.tolerations // [])[];
          (.operator // "Equal") as $operator
          | ((.key // "") == "" and $operator == "Exists"
            or (.key // "") == $taint.key)
          and ($operator == "Exists" or (.value // "") == ($taint.value // ""))
          and ((.effect // "") == "" or .effect == $taint.effect));

      # Taints that block scheduling, other than the cordon itself.
      def node_admits($pod; $candidate):
        all(($candidate.spec.taints // [])[]
          | select(.effect == "NoSchedule" or .effect == "NoExecute")
          | select(.key != "node.kubernetes.io/unschedulable");
          tolerates_taint($pod; .));

      def live_pod:
        .metadata.deletionTimestamp == null
        and .status.phase != "Succeeded" and .status.phase != "Failed"
        and (.spec.nodeName // "") != "";

      def tolerates_cordon($pod):
        any(($pod.tolerations // [])[];
          (.key // "") as $key
          | (.operator // "Equal") as $operator
          | ($key == "node.kubernetes.io/unschedulable" or ($key == "" and $operator == "Exists"))
          and ($operator == "Exists" or ($operator == "Equal" and (.value // "") == ""))
          and ((.effect // "") == "" or .effect == "NoSchedule"));

      $workload_doc[0] as $workload
      | $nodes_doc[0] as $nodes
      | $pods_doc[0] as $pods
      | ($workload.spec.replicas // 1) as $desired
      | ($workload.spec.template.spec // {}) as $pod
      | ($workload.spec.template.metadata.labels // {}) as $labels
      | ($workload.spec.selector // {}) as $selector
      | hostname_spread($pod; $labels) as $spread
      | if $desired > 1
          and (tolerates_cordon($pod) | not)
          and ($selector.matchExpressions // []) == []
          and (($selector.matchLabels // {}) | length > 0)
          and (one_per_node($pod; $labels) or $spread != null) then
          [($pods.items // [])[]
            | select(labels_match(.metadata.labels // {}; $selector.matchLabels))
            | select(live_pod)] as $replicas
          | if one_per_node($pod; $labels) then
              [$replicas[] | .spec.nodeName] as $running_nodes
              | ([($nodes.items // [])[]
                  | select(.metadata.name != $node)
                  | select(node_is_ready(.))
                  | select((.metadata.name as $name | $running_nodes | index($name) != null)
                    or (pod_matches_node($pod; .) and .spec.unschedulable != true
                      and node_admits($pod; .)))]
                  | length) as $eligible
              | [$desired, ([$eligible, 1] | max)] | min
            else
              # Replay the scheduler spread filter: count every live Pod the
              # constraint selects per node domain (DaemonSet Pods included),
              # then place the missing replicas one at a time on schedulable
              # nodes while the skew stays within maxSkew.
              ([$replicas[]
                | select(.spec.nodeName != $node)
                | select((.metadata.ownerReferences // []) | any(.kind == "ReplicaSet"))]
                | length) as $running
              | [($nodes.items // [])[]
                  | select(pod_matches_node($pod; .))
                  | select((.metadata.labels["kubernetes.io/hostname"] // "") != "")
                  | select(($spread.nodeTaintsPolicy // "Ignore") == "Ignore"
                    or (.spec.unschedulable != true and .metadata.name != $node
                      and node_admits($pod; .)))
                  | .metadata.name as $name
                  | {
                      name: $name,
                      hostname: .metadata.labels["kubernetes.io/hostname"],
                      # The drain removes this Deployment from NODE; other
                      # selected Pods there, such as DaemonSet Pods, remain.
                      count: ([($pods.items // [])[]
                        | select(live_pod and .spec.nodeName == $name)
                        | select(labels_match(.metadata.labels // {}; $spread.labelSelector.matchLabels))
                        | select($name != $node
                          or (labels_match(.metadata.labels // {}; $selector.matchLabels)
                            and ((.metadata.ownerReferences // []) | any(.kind == "ReplicaSet"))
                            | not))]
                        | length),
                      open: ($name != $node and .spec.unschedulable != true
                        and node_is_ready(.) and node_admits($pod; .))
                    }] as $domains
              # Shared hostname values would merge domains; keep every replica.
              | if ($domains | length) == 0
                  or ($domains | map(.hostname) | unique | length) != ($domains | length)
                  or $running >= $desired then
                  $desired
                else
                  (reduce range(0; $desired - $running) as $unused (
                    {domains: $domains, placed: 0};
                    ([.domains[].count] | min) as $minimum
                    | ([.domains[] | select(.open and .count + 1 - $minimum <= $spread.maxSkew)]
                      | min_by(.count) // null) as $pick
                    | if $pick == null then
                        .
                      else
                        .domains |= map(if .name == $pick.name then .count += 1 else . end)
                        | .placed += 1
                      end)
                  | .placed) as $placed
                  | [$desired, ([$running + $placed, 1] | max)] | min
                end
            end
        else
          $desired
        end'
}

collect_target_pinned_pending_workloads() {
  local pod_list
  local namespace
  local pod
  local workload_key
  local workload

  pod_list=$(mktemp)
  kubectl get pods --all-namespaces \
    --field-selector 'status.phase=Pending' \
    -o jsonpath='{range .items[*]}{.metadata.namespace}{"\t"}{.metadata.name}{"\n"}{end}' >"$pod_list"

  while IFS=$'\t' read -r namespace pod; do
    [[ -n "$namespace" && -n "$pod" ]] || continue
    workload_key=$(workload_key_for_pod "$namespace" "$pod")
    [[ -n "$workload_key" ]] || continue
    IFS=$'\t' read -r namespace workload <<<"$workload_key"
    if workload_requires_target_node "$namespace" "$workload"; then
      printf '%s\t%s\n' "$namespace" "$workload"
    fi
  done <"$pod_list" | sort -u

  rm -f "$pod_list"
}

wait_for_workloads() {
  local workloads_file="$1"
  local namespace
  local workload
  local selector
  local failed=false

  [[ -s "$workloads_file" ]] || {
    log "no controller-owned workloads to wait for"
    return 0
  }

  log "waiting for displaced workload controllers to settle"

  while IFS=$'\t' read -r namespace workload; do
    [[ -n "$namespace" && -n "$workload" ]] || continue

    case "$workload" in
      deployment/* | statefulset/*)
        log "waiting for ${namespace}/${workload}"
        if ! kubectl -n "$namespace" rollout status "$workload" --timeout="$SETTLE_TIMEOUT"; then
          failed=true
        fi
        ;;
      replicaset/*)
        log "waiting for ${namespace}/${workload} pods to be Ready"
        selector=$(
          kubectl -n "$namespace" get "$workload" -o json | jq -r '
            .spec.selector.matchLabels
            | to_entries
            | map("\(.key)=\(.value)")
            | join(",")'
        )

        if ! kubectl -n "$namespace" wait --for=condition=Ready pod \
          --selector="$selector" \
          --timeout="$SETTLE_TIMEOUT"; then
          failed=true
        fi
        ;;
      *)
        log "cannot generically wait for ${namespace}/${workload}; skipping"
        ;;
    esac
  done <"$workloads_file"

  [[ "$failed" == false ]] || die "one or more displaced workloads did not settle"
}

wait_for_controller_ready_floor() {
  local namespace="$1"
  local workload="$2"
  local minimum_ready="$3"
  local deadline
  local ready

  deadline=$((SECONDS + $(duration_to_seconds "$SETTLE_TIMEOUT")))

  while ((SECONDS < deadline)); do
    ready=$(
      kubectl -n "$namespace" get "$workload" \
        -o jsonpath='{.status.readyReplicas}' 2>/dev/null || true
    )
    ready="${ready:-0}"

    if ((ready >= minimum_ready)); then
      return 0
    fi

    sleep "$POLL_SECONDS"
  done

  die "${namespace}/${workload} has ${ready} Ready replicas; maintenance requires ${minimum_ready}"
}

wait_for_displaced_workload_survivability() {
  local workloads_file="$1"
  local namespace
  local workload
  local desired
  local minimum_ready

  [[ -s "$workloads_file" ]] || {
    log "no controller-owned workloads need maintenance checks"
    return 0
  }

  log "checking displaced workload availability"

  while IFS=$'\t' read -r namespace workload; do
    [[ -n "$namespace" && -n "$workload" ]] || continue

    if workload_requires_target_node "$namespace" "$workload"; then
      log "deferring target-pinned ${namespace}/${workload} until ${NODE} returns"
      continue
    fi

    case "$workload" in
      deployment/*)
        desired=$(
          kubectl -n "$namespace" get "$workload" \
            -o jsonpath='{.spec.replicas}' 2>/dev/null || true
        )
        desired="${desired:-1}"
        minimum_ready=$(deployment_ready_floor "$namespace" "$workload") ||
          minimum_ready="$desired"
        if ((minimum_ready < desired)); then
          log "${namespace}/${workload} cannot place every replica while ${NODE} is out; requiring ${minimum_ready} of ${desired} Ready"
        fi
        wait_for_controller_ready_floor "$namespace" "$workload" "$minimum_ready"
        ;;
      replicaset/*)
        desired=$(
          kubectl -n "$namespace" get "$workload" \
            -o jsonpath='{.spec.replicas}' 2>/dev/null || true
        )
        desired="${desired:-1}"
        wait_for_controller_ready_floor "$namespace" "$workload" "$desired"
        ;;
      statefulset/*)
        desired=$(
          kubectl -n "$namespace" get "$workload" \
            -o jsonpath='{.spec.replicas}' 2>/dev/null || true
        )
        desired="${desired:-1}"
        minimum_ready=1
        if ((desired > 1)); then
          minimum_ready=$((desired - 1))
        fi
        wait_for_controller_ready_floor "$namespace" "$workload" "$minimum_ready"
        ;;
      *)
        log "using a dedicated availability gate for ${namespace}/${workload}"
        ;;
    esac
  done <"$workloads_file"
}

wait_for_no_bad_pods() {
  local deadline
  local settle_seconds
  local bad

  log "checking for unhealthy non-Job pods"
  settle_seconds=$(duration_to_seconds "$SETTLE_TIMEOUT")
  deadline=$((SECONDS + settle_seconds))

  while ((SECONDS < deadline)); do
    bad=$(
      kubectl get pods --all-namespaces -o json | jq -r '
        .items[]
        | select((.metadata.ownerReferences // [] | map(.kind) | index("Job")) | not)
        | select(.status.phase != "Succeeded")
        | select(
            .status.phase == "Failed"
            or (.status.containerStatuses // [] | any(
              .state.waiting.reason as $reason
              | ["CrashLoopBackOff", "ImagePullBackOff", "ErrImagePull", "CreateContainerConfigError", "CreateContainerError"] | index($reason)
            ))
          )
        | "\(.metadata.namespace)/\(.metadata.name): \(.status.phase)"'
    )

    if [[ -z "$bad" ]]; then
      return 0
    fi

    printf '%s\n' "$bad" >&2
    sleep "$POLL_SECONDS"
  done

  die "unhealthy pods remained after waiting"
}

verify_node_is_cordoned() {
  local unschedulable

  unschedulable=$(kubectl get node "$NODE" -o jsonpath='{.spec.unschedulable}')
  [[ "$unschedulable" == "true" ]] ||
    die "${NODE} is not cordoned; --resume-maintenance requires an existing maintenance cordon"
}

verify_node_has_only_maintenance_pods() {
  local bad_pods

  bad_pods=$(
    kubectl get pods --all-namespaces \
      --field-selector "spec.nodeName=${NODE}" \
      -o json | jq -r '
        .items[]
        | select(.status.phase != "Succeeded" and .status.phase != "Failed")
        | select(
            (.metadata.ownerReferences[0].kind // "") != "DaemonSet"
            and ((
              .metadata.namespace == "longhorn-system"
              and .metadata.labels["longhorn.io/component"] == "instance-manager"
            ) | not)
          )
        | "\(.metadata.namespace)/\(.metadata.name)\towner=\(.metadata.ownerReferences[0].kind // "none")\tphase=\(.status.phase)"'
  )

  if [[ -n "$bad_pods" ]]; then
    printf '%s\n' "$bad_pods" >&2
    die "non-maintenance Pods remain on ${NODE}"
  fi
}

wait_for_cloudnativepg_survivability() {
  local deadline
  local bad_clusters=""

  if ! kubectl get clusters.postgresql.cnpg.io --all-namespaces >/dev/null 2>&1; then
    log "CloudNativePG clusters not found; skipping database maintenance gate"
    return 0
  fi

  log "checking CloudNativePG one-node-down availability"
  deadline=$((SECONDS + $(duration_to_seconds "$SETTLE_TIMEOUT")))

  while ((SECONDS < deadline)); do
    bad_clusters=$(
      kubectl get clusters.postgresql.cnpg.io --all-namespaces -o json | jq -r \
        --arg node "$NODE" \
        --slurpfile pods <(kubectl get pods --all-namespaces -o json) '
          ($pods[0].items
            | map({key: (.metadata.namespace + "/" + .metadata.name), value: .})
            | from_entries) as $pod_index
          | .items[]
          | (.spec.instances // 1) as $desired
          | (if $desired > 1 then $desired - 1 else 1 end) as $minimum_ready
          | (.status.readyInstances // 0) as $ready
          | (.status.currentPrimary // "") as $primary
          | ($pod_index[.metadata.namespace + "/" + $primary] // {}) as $primary_pod
          | select(
              $ready < $minimum_ready
              or $primary == ""
              or ($primary_pod.metadata.name // "") != $primary
              or ($primary_pod.spec.nodeName // "") == $node
              or ($primary_pod.status.phase // "") != "Running"
              or ($primary_pod.status.containerStatuses // [] | length) == 0
              or ($primary_pod.status.containerStatuses // [] | any(.ready != true))
            )
          | "\(.metadata.namespace)/\(.metadata.name)\tready=\($ready)/\($desired)\tminimum=\($minimum_ready)\tprimary=\($primary)\tprimaryNode=\($primary_pod.spec.nodeName // "missing")"'
    )

    [[ -n "$bad_clusters" ]] || return 0
    printf '%s\n' "$bad_clusters" >&2
    sleep "$POLL_SECONDS"
  done

  die "CloudNativePG maintenance availability gate did not pass"
}

wait_for_cloudnativepg_health() {
  local deadline
  local bad_clusters=""

  if ! kubectl get clusters.postgresql.cnpg.io --all-namespaces >/dev/null 2>&1; then
    return 0
  fi

  log "waiting for CloudNativePG clusters to become fully healthy"
  deadline=$((SECONDS + $(duration_to_seconds "$SETTLE_TIMEOUT")))

  while ((SECONDS < deadline)); do
    bad_clusters=$(
      kubectl get clusters.postgresql.cnpg.io --all-namespaces -o json | jq -r '
        .items[]
        | (.spec.instances // 1) as $desired
        | (.status.readyInstances // 0) as $ready
        | select($ready != $desired or .status.phase != "Cluster in healthy state")
        | "\(.metadata.namespace)/\(.metadata.name)\tready=\($ready)/\($desired)\tphase=\(.status.phase // "unknown")"'
    )

    [[ -n "$bad_clusters" ]] || return 0
    printf '%s\n' "$bad_clusters" >&2
    sleep "$POLL_SECONDS"
  done

  die "CloudNativePG clusters did not return to full health"
}

load_cloudnativepg_primary_switchovers() {
  CNPG_SWITCHOVERS=""

  if ! kubectl get clusters.postgresql.cnpg.io --all-namespaces >/dev/null 2>&1; then
    return 0
  fi

  CNPG_SWITCHOVERS=$(
    kubectl get clusters.postgresql.cnpg.io --all-namespaces -o json | jq -r \
      --arg node "$NODE" \
      --slurpfile pods <(kubectl get pods --all-namespaces -o json) \
      --slurpfile nodes <(kubectl get nodes -o json) '
        ($pods[0].items
          | map({key: (.metadata.namespace + "/" + .metadata.name), value: .})
          | from_entries) as $pod_index
        | ($nodes[0].items
          | map({key: .metadata.name, value: .})
          | from_entries) as $node_index
        | .items[]
        | . as $cluster
        | (.status.currentPrimary // "") as $primary
        | ($pod_index[.metadata.namespace + "/" + $primary] // {}) as $primary_pod
        | select(($primary_pod.spec.nodeName // "") == $node)
        | ([
            $pods[0].items[]
            | select(.metadata.namespace == $cluster.metadata.namespace)
            | select((.metadata.labels["cnpg.io/cluster"] // "") == $cluster.metadata.name)
            | select((.metadata.labels["cnpg.io/podRole"] // "") == "instance")
            | select((.metadata.labels["cnpg.io/instanceRole"] // "") == "replica")
            | . as $candidate_pod
            | select(.metadata.name != $primary)
            | select((.spec.nodeName // "") != $node)
            | select(.metadata.deletionTimestamp == null)
            | select(.status.phase == "Running")
            | select((.status.containerStatuses // [] | length) > 0)
            | select(.status.containerStatuses | all(.ready == true))
            | select(($cluster.status.instancesStatus.healthy // []) | index($candidate_pod.metadata.name) != null)
            | select(
                ($node_index[.spec.nodeName].spec.unschedulable // false) != true
                and ($node_index[.spec.nodeName].metadata.labels["workload-class"] // "") == "stable"
                and ($node_index[.spec.nodeName].status.conditions // [] | any(.type == "Ready" and .status == "True"))
              )
          ] | sort_by(.metadata.name) | .[0] // {}) as $candidate
        | [
            .metadata.namespace,
            .metadata.name,
            $primary,
            ($candidate.metadata.name // ""),
            ($candidate.spec.nodeName // "")
          ]
        | @tsv'
  )
}

plan_cloudnativepg_primary_switchovers() {
  local namespace
  local cluster
  local primary
  local candidate
  local candidate_node
  local missing_candidate=false

  load_cloudnativepg_primary_switchovers
  [[ -n "$CNPG_SWITCHOVERS" ]] || return 0

  printf 'namespace\tcluster\tcurrent-primary\ttarget-primary\ttarget-node\n'
  while IFS=$'\t' read -r namespace cluster primary candidate candidate_node; do
    printf '%s\t%s\t%s\t%s\t%s\n' \
      "$namespace" "$cluster" "$primary" "${candidate:-unavailable}" "${candidate_node:-unavailable}"
    if [[ -z "$candidate" || -z "$candidate_node" ]]; then
      missing_candidate=true
    fi
  done <<<"$CNPG_SWITCHOVERS"

  [[ "$missing_candidate" == false ]] ||
    die "CloudNativePG primary on ${NODE} has no healthy replica on a surviving stable node"
}

switch_cloudnativepg_primaries_off_node() {
  local namespace
  local cluster
  local primary
  local candidate
  local candidate_node

  log "planning CloudNativePG primary switchovers away from ${NODE}"
  plan_cloudnativepg_primary_switchovers
  [[ -n "$CNPG_SWITCHOVERS" ]] || {
    log "no CloudNativePG primaries need to move"
    return 0
  }

  need_command kubectl-cnpg
  while IFS=$'\t' read -r namespace cluster primary candidate candidate_node; do
    log "switching ${namespace}/${cluster} primary from ${primary} to ${candidate} on ${candidate_node}"
    kubectl cnpg promote -n "$namespace" "$cluster" "$candidate"
  done <<<"$CNPG_SWITCHOVERS"

  wait_for_cloudnativepg_health
  wait_for_cloudnativepg_survivability
}

wait_for_longhorn_health() {
  local deadline
  local settle_seconds
  local bad_volumes
  local bad_count
  local last_report_seconds=-300
  local backup_available
  local backup_reason
  local replica_rebuild_limit
  local replica_reuse_wait
  local under_replicated

  if ! kubectl get namespace longhorn-system >/dev/null 2>&1; then
    log "longhorn-system namespace not found; skipping Longhorn checks"
    return 0
  fi

  if ! kubectl -n longhorn-system get volumes.longhorn.io >/dev/null 2>&1; then
    log "Longhorn volume CRDs not found; skipping Longhorn checks"
    return 0
  fi

  log "checking attached Longhorn replica floor (${MIN_LONGHORN_REPLICAS})"
  under_replicated=$(
    kubectl -n longhorn-system get volumes.longhorn.io -o json | jq -r \
      --argjson minimum "$MIN_LONGHORN_REPLICAS" '
        .items[]
        | select(.status.state == "attached" or .status.state == "attaching")
        | select((.spec.numberOfReplicas // 0) < $minimum)
        | "\(.metadata.name)\tdesired=\(.spec.numberOfReplicas // 0)\t\(.status.state)\t\(.status.kubernetesStatus.namespace // "")/\(.status.kubernetesStatus.pvcName // "")"'
  )
  if [[ -n "$under_replicated" ]]; then
    printf '%s\n' "$under_replicated" >&2
    die "attached Longhorn volumes are below the replica policy floor"
  fi

  log "waiting for attached Longhorn volumes to be healthy"
  settle_seconds=$(duration_to_seconds "$SETTLE_TIMEOUT")
  replica_reuse_wait=$(
    kubectl -n longhorn-system get settings.longhorn.io \
      replica-replenishment-wait-interval \
      -o jsonpath='{.value}' 2>/dev/null || true
  )
  if [[ "$replica_reuse_wait" =~ ^[0-9]+$ ]]; then
    log "Longhorn failed-replica reuse window is ${replica_reuse_wait}s; health timeout is ${settle_seconds}s"
  fi
  replica_rebuild_limit=$(
    kubectl -n longhorn-system get settings.longhorn.io \
      concurrent-replica-rebuild-per-node-limit \
      -o jsonpath='{.value}' 2>/dev/null || true
  )
  if [[ "$replica_rebuild_limit" == 0 ]]; then
    log "Longhorn automatic rebuild admission is disabled; degraded volumes require the guarded sequential recovery procedure"
  fi
  deadline=$((SECONDS + settle_seconds))

  bad_volumes=$(
    kubectl -n longhorn-system get volumes.longhorn.io -o json | jq -r '
      .items[]
      | select(.status.state == "attached" or .status.state == "attaching")
      | select(.status.robustness != "healthy")
      | "\(.metadata.name)\t\(.status.state)\t\(.status.robustness)\t\(.status.kubernetesStatus.namespace // "")/\(.status.kubernetesStatus.pvcName // "")"'
  )

  if [[ -n "$bad_volumes" && "$replica_rebuild_limit" == 0 && "$CHECK_ONLY" == false ]]; then
    LONGHORN_REBUILD_LIMIT_ORIGINAL="$replica_rebuild_limit"
    log "temporarily enabling two guarded Longhorn replica rebuilds per node"
    kubectl -n longhorn-system patch settings.longhorn.io \
      concurrent-replica-rebuild-per-node-limit \
      --type=merge \
      -p '{"value":"2"}' >/dev/null
  fi

  while ((SECONDS < deadline)); do
    bad_volumes=$(
      kubectl -n longhorn-system get volumes.longhorn.io -o json | jq -r '
        .items[]
        | select(.status.state == "attached" or .status.state == "attaching")
        | select(.status.robustness != "healthy")
        | "\(.metadata.name)\t\(.status.state)\t\(.status.robustness)\t\(.status.kubernetesStatus.namespace // "")/\(.status.kubernetesStatus.pvcName // "")"'
    )

    if [[ -z "$bad_volumes" ]]; then
      break
    fi

    bad_count=$(wc -l <<<"$bad_volumes")
    if ((SECONDS - last_report_seconds >= 300)); then
      log "${bad_count} attached Longhorn volume(s) are still reconciling"
      last_report_seconds=$SECONDS
    fi
    sleep "$POLL_SECONDS"
  done

  if [[ -n "$bad_volumes" ]]; then
    printf '%s\n' "$bad_volumes" >&2
    die "Longhorn volumes are not healthy"
  fi

  restore_longhorn_rebuild_limit

  if kubectl -n longhorn-system get backuptargets.longhorn.io default >/dev/null 2>&1; then
    log "checking Longhorn backup target"
    backup_available=$(
      kubectl -n longhorn-system get backuptargets.longhorn.io default -o jsonpath='{.status.available}' 2>/dev/null || true
    )

    if [[ "$backup_available" != "true" ]]; then
      backup_reason=$(
        kubectl -n longhorn-system get backuptargets.longhorn.io default -o json | jq -r '
          (.status.conditions // [])
          | map(select(.type == "Unavailable" and .status == "True"))
          | .[0].reason // "unavailable"'
      )

      if [[ "$REQUIRE_LONGHORN_BACKUP_TARGET" == true ]]; then
        die "Longhorn backup target is not available (${backup_reason})"
      fi

      log "Longhorn backup target is not available (${backup_reason}); continuing"
    fi
  fi
}

check_pdb_eviction_blockers() {
  local ignore_cnpg_primaries="${1:-false}"
  local report

  log "checking PodDisruptionBudgets on ${NODE}"
  report=$(
    kubectl get poddisruptionbudgets.policy --all-namespaces -o json |
      jq -r \
        --arg node "$NODE" \
        --argjson ignore_cnpg_primaries "$ignore_cnpg_primaries" \
        --slurpfile pods <(kubectl get pods --all-namespaces -o json) '
        def selector_matches($labels; $selector):
          (($selector.matchLabels // {}) | to_entries | all(
            . as $entry | ($labels[$entry.key] // null) == $entry.value
          ))
          and
          (($selector.matchExpressions // []) | all(
            . as $expression
            | ($labels[$expression.key] // null) as $value
            | if $expression.operator == "In" then
                $value != null and (($expression.values // []) | index($value) != null)
              elif $expression.operator == "NotIn" then
                $value != null and (($expression.values // []) | index($value) == null)
              elif $expression.operator == "Exists" then
                $labels | has($expression.key)
              elif $expression.operator == "DoesNotExist" then
                ($labels | has($expression.key)) | not
              else
                false
              end
          ));

        .items[] as $pdb
        | select(($pdb.status.disruptionsAllowed // 0) < 1)
        | $pods[0].items[]
        | select(.spec.nodeName == $node)
        | select(.status.phase != "Succeeded" and .status.phase != "Failed")
        | select((.metadata.ownerReferences // [] | map(.kind) | index("DaemonSet")) | not)
        | select((.metadata.labels["longhorn.io/component"] // "") != "instance-manager")
        | select(selector_matches(.metadata.labels // {}; $pdb.spec.selector // {}))
        | select(
            ($ignore_cnpg_primaries
              and (.metadata.labels["cnpg.io/instanceRole"] // "") == "primary")
            | not
          )
        | [
            .metadata.namespace,
            $pdb.metadata.name,
            .metadata.name,
            ((.metadata.labels["cnpg.io/instanceRole"] // "-") | tostring)
          ]
        | @tsv'
  )

  if [[ -n "$report" ]]; then
    printf 'namespace\tpdb\tpod\tinstance-role\n%s\n' "$report" >&2
    die "PodDisruptionBudgets block eviction; restore replicas or switchover primaries before disrupting ${NODE}"
  fi
}

check_stable_node_pod_slots() {
  local report
  local status

  log "checking remaining stable-node aggregate Pod capacity (${MIN_FREE_POD_SLOTS} slots reserved)"
  report=$(
    kubectl get nodes -l workload-class=stable -o json |
      jq -r \
        --arg target "$NODE" \
        --argjson required "$MIN_FREE_POD_SLOTS" \
        --slurpfile pods <(kubectl get pods --all-namespaces -o json) '
          def active_pod:
            .status.phase != "Succeeded" and .status.phase != "Failed";
          def daemonset_pod:
            (.metadata.ownerReferences // [] | map(.kind) | index("DaemonSet")) != null;
          def mirror_pod:
            (.metadata.annotations["kubernetes.io/config.mirror"] // "") != "";

          [
            .items[]
            | select(.metadata.name != $target)
            | select(.spec.unschedulable != true)
            | select(.status.conditions | any(.type == "Ready" and .status == "True"))
          ] as $remaining
          | ($remaining | map(.metadata.name)) as $remaining_names
          | ($remaining | map(.status.allocatable.pods | tonumber) | add // 0) as $allocatable
          | ([
              $pods[0].items[]
              | select(active_pod)
              | select(.spec.nodeName as $node | $remaining_names | index($node) != null)
            ] | length) as $scheduled
          | ([
              $pods[0].items[]
              | select(active_pod)
              | select(.spec.nodeName == $target)
              | select(daemonset_pod | not)
              | select(mirror_pod | not)
            ] | length) as $displaced
          | ($allocatable - $scheduled - $displaced) as $free_after
          | [
              ($remaining_names | join(",")),
              ($scheduled | tostring),
              ($displaced | tostring),
              ($allocatable | tostring),
              ($free_after | tostring),
              (if ($remaining | length) > 0 and $free_after >= $required then "pass" else "insufficient" end)
            ]
          | @tsv'
  )

  [[ -n "$report" ]] || die "no remaining stable nodes found for Pod capacity audit"
  printf 'remaining-nodes\tscheduled\tdisplaced\tallocatable\tfree-after\tstatus\n%s\n' "$report"

  status=$(awk -F '\t' '{ print $6 }' <<<"$report")
  [[ "$status" == "pass" ]] || die "remaining stable nodes do not have enough aggregate Pod slots"
}

wait_for_longhorn_survivability() {
  local deadline
  local bad_volumes=""

  if ! kubectl -n longhorn-system get volumes.longhorn.io >/dev/null 2>&1; then
    return 0
  fi

  log "checking Longhorn one-node-down replica floor (${MIN_SURVIVING_LONGHORN_REPLICAS})"
  deadline=$((SECONDS + $(duration_to_seconds "$SETTLE_TIMEOUT")))

  while ((SECONDS < deadline)); do
    bad_volumes=$(
      kubectl -n longhorn-system get volumes.longhorn.io -o json | jq -r \
        --arg node "$NODE" \
        --argjson minimum "$MIN_SURVIVING_LONGHORN_REPLICAS" \
        --slurpfile replicas <(kubectl -n longhorn-system get replicas.longhorn.io -o json) '
          ($replicas[0].items
            | group_by(.spec.volumeName)
            | map({key: .[0].spec.volumeName, value: .})
            | from_entries) as $replica_index
          | .items[]
          | select(.status.state == "attached" or .status.state == "attaching")
          | . as $volume
          | ([
              ($replica_index[.metadata.name] // [])[]
              | select(
                  .spec.nodeID != $node
                  and .status.currentState == "running"
                  and (.spec.failedAt // "") == ""
                )
              | .spec.nodeID
            ] | unique) as $surviving_nodes
          | select(
              (.spec.numberOfReplicas // 0) < 3
              or .status.state != "attached"
              or .status.robustness == "faulted"
              or (.status.currentNodeID // "") == $node
              or (.spec.nodeID // "") == $node
              or ($surviving_nodes | length) < $minimum
            )
          | "\(.metadata.name)\t\(.status.kubernetesStatus.namespace // "")/\(.status.kubernetesStatus.pvcName // "")\tstate=\(.status.state)\trobustness=\(.status.robustness)\tattachment=\(.status.currentNodeID // "")\tsurvivingNodes=\($surviving_nodes | join(","))"'
    )

    [[ -n "$bad_volumes" ]] || return 0
    printf '%s\n' "$bad_volumes" >&2
    sleep "$POLL_SECONDS"
  done

  die "Longhorn one-node-down availability gate did not pass"
}

verify_remote_privilege() {
  log "checking privileged SSH access on ${SSH_TARGET}"
  if ! ssh -o BatchMode=yes "$SSH_TARGET" '
    if [[ $(id -u) -eq 0 ]]; then
      exit 0
    fi
    exec sudo -n true
  '; then
    die "${SSH_TARGET} does not provide noninteractive root access; no cluster state was changed"
  fi
}

remote_root_shell() {
  ssh -o BatchMode=yes "$@" "$SSH_TARGET" '
    if [[ $(id -u) -eq 0 ]]; then
      exec bash -s
    fi
    exec sudo -n bash -s
  '
}

# The annotation identifies agent-enabled targets even while the agent is stopped.
initialize_fence_gate() {
  local node_json

  node_json=$(kubectl get node "$NODE" -o json) || die "could not read ${NODE} fence annotations"
  jq -e 'type == "object" and (.metadata | type == "object")
    and ((.metadata.annotations // {}) | type == "object")' \
    >/dev/null 2>&1 <<<"$node_json" || die "${NODE}: invalid Node fence metadata"
  if jq -e '.metadata.annotations // {} | has("fence.alc.xyz/agent-mode")' \
    >/dev/null <<<"$node_json"; then
    FENCE_HAS_AGENT=true
  else
    FENCE_HAS_AGENT=false
    log "${NODE} has no self-fence agent annotation; skipping all fence gates"
  fi
}

read_fence_status() {
  FENCE_STATUS_EXIT=0
  FENCE_STATUS_JSON=$(remote_root_shell -o ConnectTimeout=5 \
    -o ServerAliveInterval=5 -o ServerAliveCountMax=1 <<'EOF'
set -euo pipefail
if ! command -v node-self-fence-status >/dev/null 2>&1; then
  exit 127
fi
exec timeout --kill-after=5s 15s node-self-fence-status --json
EOF
  ) || FENCE_STATUS_EXIT=$?

  FENCE_FAILURE=""
  if [[ "$FENCE_STATUS_EXIT" -eq 127 ]]; then
    FENCE_FAILURE="${NODE}: self-fence agent is not deployed (node-self-fence-status is missing on ${SSH_TARGET})"
    return 1
  fi
  if ! jq -e -s --arg node "$NODE" '
    length == 1 and (.[0] | type == "object" and .node == $node
    and (.states | type == "array" and length > 0)
    and ([.states[] | select(.self == true)] | length == 1)
    and ([.states[] | select(.self == true)][0].node == $node))
  ' >/dev/null 2>&1 <<<"$FENCE_STATUS_JSON"; then
    FENCE_FAILURE="${NODE}: invalid or unavailable self-fence status (exit ${FENCE_STATUS_EXIT})"
    return 1
  fi
}

# Each diagnostic names the server, falling back to its address for unverified replies.
fence_state_failures() {
  jq -r '
    .states[]
    | (.node // .address // "unknown server") as $server
    | if .ok != true then "\($server): unverified fence state (\(.error // "missing ok"))"
      elif .fenced != false then "\($server): fenced or missing fenced state"
      elif .classification != "healthy" then "\($server): classification \(.classification // "missing")"
      elif (.mode != "enforce" and .mode != "observe") then "\($server): agent mode \(.mode // "missing")"
      elif (has("since_unfence") | not) or (.refence_window | type) != "number"
        or .refence_window < 0 then "\($server): missing or invalid unfence timing"
      elif .since_unfence != null then
        if (.since_unfence | type) != "number" then "\($server): invalid since_unfence"
        elif .since_unfence < .refence_window then
          "\($server): recent unfence (\(.since_unfence)s < refence window \(.refence_window)s)"
        else empty end
      else empty end
  ' <<<"$FENCE_STATUS_JSON"
}

verify_fence_preflight() {
  local nodes_json failures mode

  initialize_fence_gate
  [[ "$FENCE_HAS_AGENT" == true ]] || return 0
  log "checking self-fence preflight on ${NODE} and its peers"

  nodes_json=$(kubectl get nodes -o json) || die "${NODE}: could not check cluster fence annotations/taints"
  failures=$(jq -r '
    .items[]
    | .metadata.name as $node
    | (if .metadata.annotations["fence.alc.xyz/disabled"] == "true" then
        "\($node): fence.alc.xyz/disabled=true" else empty end),
      (if any(.spec.taints[]?; .key == "node.kubernetes.io/out-of-service") then
        "\($node): node.kubernetes.io/out-of-service taint" else empty end)
  ' <<<"$nodes_json") || die "${NODE}: invalid cluster fence metadata"
  [[ -z "$failures" ]] || die "self-fence preflight failed: ${failures}"

  read_fence_status || die "$FENCE_FAILURE"
  failures=$(fence_state_failures) || die "${NODE}: invalid self-fence states"
  [[ -z "$failures" ]] || die "self-fence preflight failed: ${failures}"
  [[ "$FENCE_STATUS_EXIT" -eq 0 ]] || die "${NODE}: node-self-fence-status failed (exit ${FENCE_STATUS_EXIT})"
  jq -e '.servers == (.states | length)' >/dev/null <<<"$FENCE_STATUS_JSON" ||
    die "${NODE}: incomplete self-fence server states"
  # The agent's peer list could miss a server: every control-plane Node must
  # have reported a verified state.
  failures=$(jq -r --argjson status "$FENCE_STATUS_JSON" '
    [$status.states[].node] as $reported
    | .items[]
    | select(.metadata.labels["node-role.kubernetes.io/control-plane"] != null)
    | .metadata.name
    | select(. as $server | $reported | index($server) | not)
    | "\(.): control-plane Node has no verified self-fence state"
  ' <<<"$nodes_json") || die "${NODE}: invalid cluster server list"
  [[ -z "$failures" ]] || die "self-fence preflight failed: ${failures}"

  mode=$(jq -r '.states[] | select(.self == true) | .mode' <<<"$FENCE_STATUS_JSON")
  if [[ -z "$FENCE_PREVIOUS_MODE" ]]; then
    FENCE_PREVIOUS_MODE="$mode"
  elif [[ "$mode" != "$FENCE_PREVIOUS_MODE" ]]; then
    die "${NODE}: agent mode changed from ${FENCE_PREVIOUS_MODE} to ${mode} during maintenance"
  fi
}

stop_fence_agent() {
  [[ "$FENCE_HAS_AGENT" == true ]] || return 0
  local deadline=$((SECONDS + FENCE_STOP_TIMEOUT_SECONDS))
  local node_json
  local recovery="${NODE} remains cordoned; inspect node-self-fence and restore it with systemctl start node-self-fence before retrying maintenance"

  # The drain can take many minutes; peers must still be healthy when the
  # target leaves, or the remaining servers may lose their quorum.
  verify_fence_preflight
  log "stopping self-fence agent on ${NODE} before the power action"
  if ! remote_root_shell -o ConnectTimeout=5 \
    -o ServerAliveInterval=5 -o ServerAliveCountMax=1 <<'EOF'
set -euo pipefail
exec timeout --kill-after=5s 60s systemctl stop node-self-fence
EOF
  then
    die "${NODE}: failed to stop node-self-fence; ${recovery}"
  fi

  while true; do
    if node_json=$(kubectl get node "$NODE" -o json --request-timeout=5s) &&
      jq -e '.metadata.annotations["fence.alc.xyz/agent-mode"] == "stopped"' \
        >/dev/null <<<"$node_json"; then
      FENCE_AGENT_STOPPED=true
      # API-server time of the stop write; a returned heartbeat must be later.
      FENCE_STOPPED_AT=$(kubectl get node "$NODE" -o json --show-managed-fields=true --request-timeout=5s |
        jq -r '[.metadata.managedFields[]? | select(.manager == "node-self-fence") | .time
          | fromdateiso8601] | max // empty') || FENCE_STOPPED_AT=""
      return 0
    fi
    ((SECONDS < deadline)) || die "${NODE}: agent did not publish mode=stopped within ${FENCE_STOP_TIMEOUT_SECONDS}s; ${recovery}"
    sleep "$POLL_SECONDS"
  done
}

wait_for_returned_fence_agent() {
  [[ "$FENCE_HAS_AGENT" == true ]] || return 0
  local deadline=$((SECONDS + $(duration_to_seconds "$FENCE_TIMEOUT")))
  local own_state mode node_json

  log "waiting for ${NODE} healthy self-fence agent and a fresh heartbeat"
  while true; do
    if read_fence_status; then
      if ((FENCE_STATUS_EXIT > 1)); then
        FENCE_FAILURE="${NODE}: node-self-fence-status failed (exit ${FENCE_STATUS_EXIT})"
      else
        own_state=$(jq -c '.states[] | select(.self == true)' <<<"$FENCE_STATUS_JSON")
        mode=$(jq -r '.mode // "missing"' <<<"$own_state")
        if ! jq -e '.ok == true and .classification == "healthy" and .fenced == false
          and (.mode == "enforce" or .mode == "observe")' >/dev/null <<<"$own_state"; then
          FENCE_FAILURE="${NODE}: own agent is not healthy and unfenced (mode=${mode}, $(jq -r '"ok=\(.ok), classification=\(.classification // "missing"), fenced=\(.fenced), error=\(.error // "none")"' <<<"$own_state"))"
        elif [[ "$ACTION" != "on" && "$mode" != "$FENCE_PREVIOUS_MODE" ]]; then
          FENCE_FAILURE="${NODE}: agent mode ${mode} differs from pre-maintenance mode ${FENCE_PREVIOUS_MODE}"
        elif node_json=$(kubectl get node "$NODE" -o json --show-managed-fields=true --request-timeout=5s) &&
          jq -e --arg mode "$mode" --arg baseline "$FENCE_STOPPED_AT" '
            # Compare against the stop write or kon maintenance baseline, both
            # recorded by the API server. The Ready transition carries the
            # kubelet clock and is not compared.
            [.metadata.managedFields[]? | select(.manager == "node-self-fence")
              | .time | fromdateiso8601] as $heartbeat
            | .metadata.annotations["fence.alc.xyz/agent-mode"] == $mode
              and ($heartbeat | length) > 0
              and ($baseline == "" or ($heartbeat | max) > ($baseline | tonumber))
          ' >/dev/null 2>&1 <<<"$node_json"; then
          log "${NODE} self-fence agent recovered in ${mode} mode with a fresh heartbeat"
          return 0
        else
          FENCE_FAILURE="${NODE}: no node-self-fence heartbeat after the maintenance baseline"
        fi
      fi
    fi
    ((SECONDS < deadline)) || die "${FENCE_FAILURE}; fence recovery timed out after ${FENCE_TIMEOUT}; ${NODE} remains cordoned"
    sleep "$POLL_SECONDS"
  done
}

wait_for_node_storage_detach() {
  local deadline
  local timeout_seconds
  local attached_volumes=""
  local volume_attachments=""

  if kubectl -n longhorn-system get volumes.longhorn.io >/dev/null 2>&1; then
    log "waiting for Longhorn volumes to detach from ${NODE}"
    timeout_seconds=$(duration_to_seconds "$DRAIN_TIMEOUT")
    deadline=$((SECONDS + timeout_seconds))

    while ((SECONDS < deadline)); do
      attached_volumes=$(
        kubectl -n longhorn-system get volumes.longhorn.io -o json | jq -r --arg node "$NODE" '
          .items[]
          | select(.status.currentNodeID == $node or .spec.nodeID == $node)
          | select(.status.state != "detached")
          | "\(.metadata.name)\t\(.status.state)\t\(.status.kubernetesStatus.namespace // "")/\(.status.kubernetesStatus.pvcName // "")"'
      )

      [[ -n "$attached_volumes" ]] || break
      printf '%s\n' "$attached_volumes" >&2
      sleep "$POLL_SECONDS"
    done

    [[ -z "$attached_volumes" ]] || die "Longhorn volumes remain attached to ${NODE}"
  fi

  log "waiting for Longhorn VolumeAttachments to leave ${NODE}"
  timeout_seconds=$(duration_to_seconds "$DRAIN_TIMEOUT")
  deadline=$((SECONDS + timeout_seconds))

  while ((SECONDS < deadline)); do
    volume_attachments=$(
      kubectl get volumeattachments.storage.k8s.io -o json | jq -r --arg node "$NODE" '
        .items[]
        | select(.spec.nodeName == $node and .spec.attacher == "driver.longhorn.io")
        | "\(.metadata.name)\t\(.spec.source.persistentVolumeName // "")\tattached=\(.status.attached // false)"'
    )

    [[ -n "$volume_attachments" ]] || break
    printf '%s\n' "$volume_attachments" >&2
    sleep "$POLL_SECONDS"
  done

  [[ -z "$volume_attachments" ]] || die "Longhorn VolumeAttachments remain on ${NODE}"

  log "checking ${SSH_TARGET} for residual Longhorn mounts and iSCSI sessions"
  if ! {
    printf 'cleanup_sessions=%q\n' "$([[ "$CHECK_ONLY" == false ]] && printf true || printf false)"
    cat <<'EOF'
set -euo pipefail
shopt -s nullglob

command -v findmnt >/dev/null
command -v iscsiadm >/dev/null
command -v lsof >/dev/null
command -v lsblk >/dev/null
command -v sed >/dev/null
command -v udevadm >/dev/null

mounts=$(
  findmnt -rn -o TARGET \
    | grep -E '^/var/lib/kubelet/plugins/kubernetes.io/csi/driver\.longhorn\.io/.+/globalmount$' \
    || true
)

if [[ -n "$mounts" ]]; then
  printf 'Residual Longhorn mounts:\n%s\n' "$mounts" >&2
  exit 1
fi

mapfile -t sessions < <(
  iscsiadm -m session 2>/dev/null \
    | awk '$4 ~ /^iqn\.2019-10\.io\.longhorn:/ { print $2, $3, $4 }'
)

if ((${#sessions[@]} == 0)); then
  exit 0
fi

printf 'Found %d detached Longhorn iSCSI session(s); validating host use\n' \
  "${#sessions[@]}" >&2

for session in "${sessions[@]}"; do
  read -r sid portal target <<<"$session"
  sid=${sid#[}
  sid=${sid%]}

  [[ "$sid" =~ ^[0-9]+$ ]] || {
    printf 'Unsafe iSCSI session ID: %s\n' "$sid" >&2
    exit 1
  }
  [[ "$portal" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+:3260,1$ ]] || {
    printf 'Unsafe Longhorn iSCSI portal: %s\n' "$portal" >&2
    exit 1
  }
  [[ "$target" =~ ^iqn\.2019-10\.io\.longhorn:pvc-[0-9a-f-]+$ ]] || {
    printf 'Unsafe Longhorn iSCSI target: %s\n' "$target" >&2
    exit 1
  }

  session_path="/sys/class/iscsi_session/session${sid}"
  [[ -r "$session_path/targetname" ]] || {
    printf 'Missing sysfs state for Longhorn iSCSI session %s\n' "$sid" >&2
    exit 1
  }
  [[ "$(<"$session_path/targetname")" == "$target" ]] || {
    printf 'iSCSI session %s target changed while validating\n' "$sid" >&2
    exit 1
  }

  block_paths=("$session_path"/device/target*/*/block/*)
  for block_path in "${block_paths[@]}"; do
    device=${block_path##*/}
    [[ "$device" =~ ^[A-Za-z0-9._-]+$ && -b "/dev/$device" ]] || {
      printf 'Unsafe block device for iSCSI session %s: %s\n' "$sid" "$device" >&2
      exit 1
    }

    while IFS= read -r related_device; do
      [[ "$related_device" =~ ^[A-Za-z0-9._-]+$ ]] || exit 1

      if lsblk -nr -o MOUNTPOINTS "/dev/$related_device" | grep -q '[^[:space:]]'; then
        printf 'Longhorn iSCSI device /dev/%s is still mounted\n' "$related_device" >&2
        exit 1
      fi

      if [[ -d "/sys/class/block/$related_device/holders" ]] \
        && find "/sys/class/block/$related_device/holders" -mindepth 1 -maxdepth 1 -print -quit \
          | grep -q .; then
        printf 'Longhorn iSCSI device /dev/%s still has block holders\n' "$related_device" >&2
        exit 1
      fi

      if lsof -t -- "/dev/$related_device" 2>/dev/null | grep -q .; then
        printf 'Longhorn iSCSI device /dev/%s is still open\n' "$related_device" >&2
        exit 1
      fi
    done < <(lsblk -nr -o NAME "/dev/$device")
  done
done

if [[ "$cleanup_sessions" != true ]]; then
  printf 'Detached Longhorn iSCSI sessions are safe to reconcile during maintenance\n' >&2
  exit 0
fi

printf 'Reconciling %d detached Longhorn iSCSI session(s)\n' "${#sessions[@]}" >&2
for session in "${sessions[@]}"; do
  read -r sid portal target <<<"$session"
  sid=${sid#[}
  sid=${sid%]}
  portal_address=${portal%,1}
  record_portal=${portal/:3260,1/,3260,1}
  record="/etc/iscsi/nodes/$target/$record_portal/default"

  # Open-iSCSI 2.1.12 rejects records created by older releases when they
  # contain this removed key. Migrate only the exact validated record before
  # asking iscsiadm to log out and remove it.
  if [[ -f "$record" ]]; then
    sed -i '/^node\.session\.conn_reopen_log_freq[[:space:]]*=/d' "$record"
  fi

  iscsiadm -m node -T "$target" -p "$portal_address" --logout
  iscsiadm -m node -T "$target" -p "$portal_address" --op=delete
done

udevadm settle

remaining=$(iscsiadm -m session 2>/dev/null | grep -F 'iqn.2019-10.io.longhorn:' || true)
if [[ -n "$remaining" ]]; then
  printf 'Longhorn iSCSI sessions remain after reconciliation:\n%s\n' "$remaining" >&2
  exit 1
fi
EOF
  } | remote_root_shell; then
    die "residual Longhorn storage is still active on ${SSH_TARGET}"
  fi
}

warn_for_longhorn_health() {
  local bad_volumes

  if ! kubectl get namespace longhorn-system >/dev/null 2>&1; then
    return 0
  fi

  if ! kubectl -n longhorn-system get volumes.longhorn.io >/dev/null 2>&1; then
    return 0
  fi

  bad_volumes=$(
    kubectl -n longhorn-system get volumes.longhorn.io -o json | jq -r '
      .items[]
      | select(.status.state == "attached" or .status.state == "attaching")
      | select(.status.robustness != "healthy")
      | "\(.metadata.name)\t\(.status.state)\t\(.status.robustness)\t\(.status.kubernetesStatus.namespace // "")/\(.status.kubernetesStatus.pvcName // "")"'
  )

  if [[ -n "$bad_volumes" ]]; then
    log "attached Longhorn volumes are not healthy after drain; continuing for poweroff"
    printf '%s\n' "$bad_volumes" >&2
  fi
}

settle_cluster() {
  local workloads_file="$1"

  wait_for_workloads "$workloads_file"
  if [[ "$ACTION" == "off" ]]; then
    warn_for_longhorn_health
  else
    wait_for_longhorn_health
  fi
  wait_for_cloudnativepg_health
  wait_for_no_bad_pods
}

wait_for_returned_node_network() {
  local deadline=$((SECONDS + $(duration_to_seconds "$READY_TIMEOUT")))

  log "waiting for ${NODE} Flannel interface before the network audit"
  until ssh -o BatchMode=yes -o ConnectTimeout=5 "$SSH_TARGET" \
    'ip link show dev flannel.1 >/dev/null 2>&1'; do
    ((SECONDS < deadline)) ||
      die "${NODE} Flannel interface did not appear within ${READY_TIMEOUT}; node remains cordoned"
    sleep "$POLL_SECONDS"
  done

  verify_returned_node_network
}

verify_returned_node_network() {
  local internal_ip
  local flannel_ip

  internal_ip=$(
    kubectl get node "$NODE" \
      -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}'
  )
  flannel_ip=$(
    kubectl get node "$NODE" \
      -o jsonpath='{.metadata.annotations.flannel\.alpha\.coreos\.com/public-ip}'
  )

  [[ -n "$internal_ip" ]] || die "${NODE} has no Kubernetes InternalIP"
  [[ "$flannel_ip" == "$internal_ip" ]] ||
    die "${NODE} Flannel public IP (${flannel_ip:-missing}) does not match InternalIP (${internal_ip})"

  verify_all_node_network_paths
}

verify_network_path_on_node() {
  local audit_node="$1"
  local expected_ip="$2"
  local audit_ssh_target="$3"
  local remote_command

  [[ -n "$expected_ip" ]] || die "${audit_node} has no Kubernetes InternalIP"
  [[ "$audit_node" =~ ^[A-Za-z0-9._-]+$ ]] || die "unsafe Kubernetes node name: ${audit_node}"
  [[ "$expected_ip" =~ ^[0-9A-Fa-f:.]+$ ]] || die "unsafe Kubernetes node address: ${expected_ip}"
  [[ -n "$NETWORK_AUDIT_SCRIPT" && -r "$NETWORK_AUDIT_SCRIPT" ]] ||
    die "Kubernetes node network audit helper is unavailable"

  remote_command="bash -s -- --node ${audit_node} --expected-node-ip ${expected_ip} --disallowed-interface wt0"
  log "checking ${audit_node} Kubernetes peer routes and Flannel underlay"
  if ! ssh -o BatchMode=yes "$audit_ssh_target" "
    if [[ \$(id -u) -eq 0 ]]; then
      exec ${remote_command}
    fi
    exec sudo -n ${remote_command}
  " <"$NETWORK_AUDIT_SCRIPT"; then
    die "${audit_node} Kubernetes network path audit failed"
  fi
}

verify_remote_node_network_path() {
  local expected_ip="${1:-}"

  if [[ -z "$expected_ip" ]]; then
    expected_ip=$(
      kubectl get node "$NODE" \
        -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}'
    )
  fi

  verify_network_path_on_node "$NODE" "$expected_ip" "$SSH_TARGET"
}

verify_all_node_network_paths() {
  local excluded_node="${1:-}"
  local rows audit_node expected_ip ready audit_ssh_target

  rows=$(
    kubectl get nodes -o json | jq -r '
      .items[]
      | [
          .metadata.name,
          ([.status.addresses[]? | select(.type == "InternalIP") | .address]
            | if length == 1 then .[0] else "" end),
          ([.status.conditions[]? | select(.type == "Ready") | .status]
            | if length == 1 then .[0] else "" end)
        ]
      | @tsv'
  )

  while IFS=$'\t' read -r audit_node expected_ip ready; do
    [[ -n "$audit_node" && "$audit_node" != "$excluded_node" ]] || continue
    [[ "$ready" == "True" ]] || die "survivor node ${audit_node} is not Ready"
    if [[ "$audit_node" == "$NODE" ]]; then
      audit_ssh_target="$SSH_TARGET"
    else
      audit_ssh_target="${NODE_SSH_USER}@${audit_node}"
    fi
    verify_network_path_on_node "$audit_node" "$expected_ip" "$audit_ssh_target"
  done <<<"$rows"
}

verify_survivor_node_network_paths() {
  log "checking survivor-node networking after ${NODE} left service"
  verify_all_node_network_paths "$NODE"
}

verify_resumed_maintenance_state() {
  log "validating resumed maintenance state for ${NODE}"
  verify_node_is_cordoned
  verify_node_has_only_maintenance_pods
  wait_for_node_storage_detach
  wait_for_longhorn_survivability
  wait_for_cloudnativepg_survivability
  wait_for_no_bad_pods
}

wait_for_ssh_down() {
  log "waiting for SSH on ${SSH_TARGET} to drop"

  for _ in {1..30}; do
    if ! ssh -o BatchMode=yes -o ConnectTimeout=2 "$SSH_TARGET" true >/dev/null 2>&1; then
      return 0
    fi
    sleep "$POLL_SECONDS"
  done

  die "SSH did not drop; the power action was not verified and ${NODE} remains cordoned"
}

wait_for_ssh_up() {
  local elapsed=0

  log "waiting for SSH on ${SSH_TARGET} to return"

  while ((elapsed < SSH_TIMEOUT_SECONDS)); do
    if ssh -o BatchMode=yes -o ConnectTimeout=5 "$SSH_TARGET" true >/dev/null 2>&1; then
      return 0
    fi

    sleep "$POLL_SECONDS"
    elapsed=$((elapsed + POLL_SECONDS))
  done

  die "SSH did not return within ${SSH_TIMEOUT_SECONDS}s"
}

reboot_host() {
  local status

  REMOTE_BOOT_ID=$(ssh -o BatchMode=yes "$SSH_TARGET" cat /proc/sys/kernel/random/boot_id)
  [[ -n "$REMOTE_BOOT_ID" ]] || die "could not read the current boot ID from ${SSH_TARGET}"

  log "rebooting ${SSH_TARGET}"

  set +e
  ssh -T "$SSH_TARGET" '
    if [[ $(id -u) -eq 0 ]]; then
      exec systemctl reboot --no-block
    fi
    exec sudo -n systemctl reboot --no-block
  '
  status=$?
  set -e

  case "$status" in
    0 | 255)
      ;;
    *)
      die "remote reboot command failed before reboot started; ${NODE} remains cordoned"
      ;;
  esac
}

verify_new_boot() {
  local current_boot_id

  current_boot_id=$(ssh -o BatchMode=yes "$SSH_TARGET" cat /proc/sys/kernel/random/boot_id)
  [[ -n "$current_boot_id" ]] || die "could not read the boot ID after ${SSH_TARGET} returned"
  [[ "$current_boot_id" != "$REMOTE_BOOT_ID" ]] ||
    die "${SSH_TARGET} returned without rebooting; ${NODE} remains cordoned"
}

poweroff_host() {
  local status

  log "powering off ${SSH_TARGET}"

  set +e
  ssh -T "$SSH_TARGET" '
    if [[ $(id -u) -eq 0 ]]; then
      exec systemctl poweroff
    fi
    exec sudo -n systemctl poweroff
  '
  status=$?
  set -e

  case "$status" in
    0 | 255)
      ;;
    *)
      die "remote poweroff command failed before shutdown started"
      ;;
  esac
}

prepare_node_for_disruption() {
  local workloads_file
  local unschedulable
  workloads_file="$1"

  log "checking Kubernetes node ${NODE}"
  kubectl get node "$NODE" >/dev/null
  verify_remote_privilege
  verify_fence_preflight
  verify_all_node_network_paths

  if [[ "$RESUME_MAINTENANCE" == true ]]; then
    verify_resumed_maintenance_state
    collect_target_pinned_pending_workloads >"$workloads_file"
    return 0
  fi

  unschedulable=$(kubectl get node "$NODE" -o jsonpath='{.spec.unschedulable}')
  [[ "$unschedulable" != "true" ]] ||
    die "${NODE} is already cordoned; use --resume-maintenance for another power cycle"

  log "running preflight cluster health checks"
  wait_for_longhorn_health
  wait_for_cloudnativepg_health
  wait_for_no_bad_pods
  log "planning CloudNativePG primary switchovers away from ${NODE}"
  plan_cloudnativepg_primary_switchovers
  check_pdb_eviction_blockers true
  check_stable_node_pod_slots
  switch_cloudnativepg_primaries_off_node
  check_pdb_eviction_blockers

  collect_displaced_workloads >"$workloads_file"

  log "cordoning ${NODE}"
  kubectl cordon "$NODE"

  if [[ "$SKIP_DRAIN" == true ]]; then
    log "skipping drain by request"
  else
    local -a drain_args=(
      drain "$NODE"
      --ignore-daemonsets
      --delete-emptydir-data
      '--pod-selector=longhorn.io/component!=instance-manager'
      --timeout="$DRAIN_TIMEOUT"
    )

    if [[ "$FORCE_DRAIN" == true ]]; then
      drain_args+=(--force)
    fi

    if [[ "$BYPASS_PDB" == true ]]; then
      drain_args+=(--disable-eviction)
    fi

    log "draining ${NODE}"
    if ! kubectl "${drain_args[@]}"; then
      die "drain failed; node disruption was not started and ${NODE} remains cordoned"
    fi

    wait_for_displaced_workload_survivability "$workloads_file"
    wait_for_no_bad_pods
  fi

  wait_for_node_storage_detach
  wait_for_longhorn_survivability
  wait_for_cloudnativepg_survivability
}

run_reboot() {
  WORKLOADS_FILE=$(mktemp)
  trap cleanup_runtime_state EXIT

  prepare_node_for_disruption "$WORKLOADS_FILE"
  stop_fence_agent
  reboot_host
  wait_for_ssh_down
  FENCE_AGENT_STOPPED=false
  verify_survivor_node_network_paths
  wait_for_ssh_up
  verify_new_boot

  log "waiting for ${NODE} to report Ready"
  kubectl wait "node/${NODE}" --for=condition=Ready --timeout="$READY_TIMEOUT"
  wait_for_returned_node_network
  wait_for_longhorn_survivability
  wait_for_cloudnativepg_survivability

  if [[ "$LEAVE_CORDONED" == true ]]; then
    log "leaving ${NODE} cordoned"
  else
    wait_for_returned_fence_agent
    log "uncordoning ${NODE}"
    kubectl uncordon "$NODE"
    settle_cluster "$WORKLOADS_FILE"
  fi

  log "done"
}

run_poweroff() {
  WORKLOADS_FILE=$(mktemp)
  trap cleanup_runtime_state EXIT

  prepare_node_for_disruption "$WORKLOADS_FILE"
  stop_fence_agent
  poweroff_host
  wait_for_ssh_down
  FENCE_AGENT_STOPPED=false
  verify_survivor_node_network_paths

  log "${NODE} is powered off and remains cordoned"
}

run_poweron_finalize() {
  WORKLOADS_FILE=$(mktemp)
  trap cleanup_runtime_state EXIT

  log "checking Kubernetes node ${NODE}"
  kubectl get node "$NODE" >/dev/null

  wait_for_ssh_up
  initialize_fence_gate

  log "waiting for ${NODE} to report Ready"
  kubectl wait "node/${NODE}" --for=condition=Ready --timeout="$READY_TIMEOUT"
  verify_node_is_cordoned
  wait_for_returned_node_network
  wait_for_longhorn_survivability
  wait_for_cloudnativepg_survivability
  collect_target_pinned_pending_workloads >"$WORKLOADS_FILE"

  if [[ "$FENCE_HAS_AGENT" == true ]]; then
    local baseline_value
    baseline_value="$(date +%s%N)-$$-${RANDOM}"
    # kon has no stop timestamp from a previous process. A unique annotation
    # write establishes an API-clock baseline even after an unplanned outage.
    log "establishing self-fence heartbeat baseline for ${NODE}"
    kubectl annotate node "$NODE" "fence.alc.xyz/maintenance-baseline=${baseline_value}" \
      --overwrite --field-manager=k8s-node-reboot --request-timeout=5s >/dev/null ||
      die "${NODE}: could not establish self-fence heartbeat baseline (annotation write failed); ${NODE} remains cordoned"
    FENCE_STOPPED_AT=$(kubectl get node "$NODE" -o json --show-managed-fields=true --request-timeout=5s |
      jq -r '[.metadata.managedFields[]? | select(.manager == "k8s-node-reboot") | .time
        | fromdateiso8601] | max // empty') || FENCE_STOPPED_AT=""
    kubectl annotate node "$NODE" fence.alc.xyz/maintenance-baseline- --request-timeout=5s >/dev/null ||
      die "${NODE}: could not establish self-fence heartbeat baseline (annotation cleanup failed); ${NODE} remains cordoned"
    [[ -n "$FENCE_STOPPED_AT" ]] ||
      die "${NODE}: could not establish self-fence heartbeat baseline (API-server timestamp unavailable); ${NODE} remains cordoned"
  fi

  wait_for_returned_fence_agent
  log "uncordoning ${NODE}"
  kubectl uncordon "$NODE"

  wait_for_workloads "$WORKLOADS_FILE"
  wait_for_longhorn_health
  wait_for_cloudnativepg_health
  wait_for_no_bad_pods

  log "done"
}

run_check_only() {
  local unschedulable

  log "checking Kubernetes node ${NODE} without changing cluster or host state"
  kubectl get node "$NODE" >/dev/null
  verify_remote_privilege
  verify_fence_preflight
  verify_all_node_network_paths

  if [[ "$RESUME_MAINTENANCE" == true ]]; then
    verify_resumed_maintenance_state
  else
    unschedulable=$(kubectl get node "$NODE" -o jsonpath='{.spec.unschedulable}')
    [[ "$unschedulable" != "true" ]] ||
      die "${NODE} is already cordoned; use --resume-maintenance for maintenance-state checks"
    wait_for_longhorn_health
    wait_for_cloudnativepg_health
    wait_for_no_bad_pods
    log "planning CloudNativePG primary switchovers away from ${NODE}"
    plan_cloudnativepg_primary_switchovers
    check_pdb_eviction_blockers true
    check_stable_node_pod_slots
  fi

  log "checks passed; no changes were made"
}

main() {
  parse_args "$@"
  need_command kubectl
  need_command ssh
  need_command jq
  need_command awk
  [[ "$NODE_SSH_USER" =~ ^[A-Za-z0-9._-]+$ ]] || die "unsafe Kubernetes node SSH user: ${NODE_SSH_USER}"

  if [[ "$RESUME_MAINTENANCE" == true && "$ACTION" == "on" ]]; then
    die "--resume-maintenance is for reboot/off cycles; use kon to finalize maintenance"
  fi

  if [[ "$RESUME_MAINTENANCE" == true && "$SKIP_DRAIN" == true ]]; then
    die "--resume-maintenance and --skip-drain cannot be combined"
  fi

  if [[ "$CHECK_ONLY" == true ]]; then
    run_check_only
    return 0
  fi

  case "$ACTION" in
    reboot)
      run_reboot
      ;;
    off)
      run_poweroff
      ;;
    on)
      run_poweron_finalize
      ;;
    *)
      die "unsupported action: ${ACTION}"
      ;;
  esac
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
