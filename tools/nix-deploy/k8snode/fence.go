package k8snode

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The self-fence gates of ADR-0082. A target whose Node carries the
// fence.alc.xyz/agent-mode annotation runs node-self-fence; the agent's
// local status command reports its own and its authenticated peers' states
// as JSON over the existing SSH connection.

const (
	agentModeAnnotation = "fence.alc.xyz/agent-mode"
	baselineAnnotation  = "fence.alc.xyz/maintenance-baseline"
	// agentManager is the field manager of the agent's Node writes; their
	// API-server times are its heartbeats.
	agentManager = "node-self-fence"
	// baselineManager writes the power-on baseline annotation.
	baselineManager = "k8s-node-reboot"
)

const fenceStatusScript = `set -euo pipefail
if ! command -v node-self-fence-status >/dev/null 2>&1; then
  exit 127
fi
exec timeout --kill-after=5s 15s node-self-fence-status --json
`

const fenceStopScript = `set -euo pipefail
exec timeout --kill-after=5s 60s systemctl stop node-self-fence
`

// fenceSSHOptions keep a status read or agent stop from hanging on an
// unreachable host.
var fenceSSHOptions = []string{"-o", "ConnectTimeout=5", "-o", "ServerAliveInterval=5", "-o", "ServerAliveCountMax=1"}

type fenceState struct {
	hasAgent bool
	// previousMode is the target agent's mode before maintenance.
	previousMode string
	// status is the last valid status report, decoded without a schema so
	// the checks can tell missing, null and mistyped fields apart.
	status     map[string]any
	statusExit int
	failure    string
	// agentStopped is set between a confirmed agent stop and the verified
	// power action.
	agentStopped bool
	// baseline is the API-server time, in Unix seconds, that a returned
	// agent's heartbeat must follow: the stop write, or the power-on
	// baseline annotation.
	baseline    int64
	hasBaseline bool
}

// initializeFenceGate reads whether the target runs an agent. The
// annotation identifies agent-enabled targets even while the agent is
// stopped.
func (o *Operation) initializeFenceGate() error {
	node := o.opts.Node
	r := o.kubectl(Command{Argv: []string{"get", "node", node, "-o", "json"}})
	if r.Status != 0 {
		return fmt.Errorf("could not read %s fence annotations", node)
	}
	var doc struct {
		Metadata *struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal([]byte(r.Stdout), &doc); err != nil || doc.Metadata == nil {
		return fmt.Errorf("%s: invalid Node fence metadata", node)
	}
	_, o.fence.hasAgent = doc.Metadata.Annotations[agentModeAnnotation]
	if !o.fence.hasAgent {
		o.log("%s has no self-fence agent annotation; skipping all fence gates", node)
	}
	return nil
}

// readFenceStatus runs the agent's status command on the target. It fails,
// setting fence.failure, unless the reply is one JSON object for the node
// with exactly one own state.
func (o *Operation) readFenceStatus() bool {
	node := o.opts.Node
	r := o.remoteRootShell(fenceStatusScript, false, fenceSSHOptions...)
	o.fence.statusExit = r.Status
	o.fence.status = nil
	o.fence.failure = ""
	if r.Status == 127 {
		o.fence.failure = fmt.Sprintf("%s: self-fence agent is not deployed (node-self-fence-status is missing on %s)", node, o.opts.SSHTarget)
		return false
	}
	status, ok := parseFenceStatus(r.Stdout, node)
	if !ok {
		o.fence.failure = fmt.Sprintf("%s: invalid or unavailable self-fence status (exit %d)", node, r.Status)
		return false
	}
	o.fence.status = status
	return true
}

// parseFenceStatus accepts exactly one JSON object naming the node, with a
// non-empty states list holding exactly one own state for the node.
func parseFenceStatus(text, node string) (map[string]any, bool) {
	decoder := json.NewDecoder(strings.NewReader(text))
	var document any
	if err := decoder.Decode(&document); err != nil {
		return nil, false
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, false
	}
	status, ok := document.(map[string]any)
	if !ok || status["node"] != node {
		return nil, false
	}
	states, ok := status["states"].([]any)
	if !ok || len(states) == 0 {
		return nil, false
	}
	selfStates := 0
	var self map[string]any
	for _, entry := range states {
		state, ok := asObject(entry)
		if !ok {
			return nil, false
		}
		if state["self"] == true {
			selfStates++
			self = state
		}
	}
	if selfStates != 1 || self["node"] != node {
		return nil, false
	}
	return status, true
}

// asObject reads a JSON object, treating null as an empty one as jq does
// for field access.
func asObject(value any) (map[string]any, bool) {
	if value == nil {
		return map[string]any{}, true
	}
	object, ok := value.(map[string]any)
	return object, ok
}

// orElse is jq's `//`: the fallback replaces null and false.
func orElse(value, fallback any) any {
	if value == nil || value == false {
		return fallback
	}
	return value
}

// jqText renders a JSON value as jq string interpolation does.
func jqText(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case nil:
		return "null"
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	data, _ := json.Marshal(value)
	return string(data)
}

// fenceStates returns the status report's states.
func (o *Operation) fenceStates() []map[string]any {
	var states []map[string]any
	entries, _ := o.fence.status["states"].([]any)
	for _, entry := range entries {
		state, _ := asObject(entry)
		states = append(states, state)
	}
	return states
}

// ownFenceState returns the target's own state from the status report.
func (o *Operation) ownFenceState() map[string]any {
	for _, state := range o.fenceStates() {
		if state["self"] == true {
			return state
		}
	}
	return map[string]any{}
}

// fenceStateFailures names every server whose state blocks maintenance:
// unverified, fenced, not healthy, in an unknown mode, or recently
// unfenced. Unverified replies are named by their address.
func fenceStateFailures(states []map[string]any) []string {
	var failures []string
	for _, state := range states {
		server := jqText(orElse(orElse(state["node"], state["address"]), "unknown server"))
		window, windowIsNumber := state["refence_window"].(float64)
		sinceUnfence, hasSinceUnfence := state["since_unfence"]
		switch {
		case state["ok"] != true:
			failures = append(failures, fmt.Sprintf("%s: unverified fence state (%s)", server, jqText(orElse(state["error"], "missing ok"))))
		case state["fenced"] != false:
			failures = append(failures, fmt.Sprintf("%s: fenced or missing fenced state", server))
		case state["classification"] != "healthy":
			failures = append(failures, fmt.Sprintf("%s: classification %s", server, jqText(orElse(state["classification"], "missing"))))
		case state["mode"] != "enforce" && state["mode"] != "observe":
			failures = append(failures, fmt.Sprintf("%s: agent mode %s", server, jqText(orElse(state["mode"], "missing"))))
		case !hasSinceUnfence || !windowIsNumber || window < 0:
			failures = append(failures, fmt.Sprintf("%s: missing or invalid unfence timing", server))
		case sinceUnfence != nil:
			since, ok := sinceUnfence.(float64)
			if !ok {
				failures = append(failures, fmt.Sprintf("%s: invalid since_unfence", server))
			} else if since < window {
				failures = append(failures, fmt.Sprintf("%s: recent unfence (%ss < refence window %ss)", server, jqText(since), jqText(window)))
			}
		}
	}
	return failures
}

// verifyFencePreflight requires, for an agent-enabled target, a healthy,
// verified and unfenced state on every server, no recent unfence, and no
// disabled annotation or out-of-service taint on any node. It also records
// the target agent's mode and fails if a later recheck finds another.
func (o *Operation) verifyFencePreflight() error {
	node := o.opts.Node
	if err := o.initializeFenceGate(); err != nil {
		return err
	}
	if !o.fence.hasAgent {
		return nil
	}
	o.log("checking self-fence preflight on %s and its peers", node)

	nodes, err := o.listNodes()
	if err != nil {
		return fmt.Errorf("%s: could not check cluster fence annotations/taints", node)
	}
	var failures []string
	for _, n := range nodes {
		if n.Metadata.Annotations["fence.alc.xyz/disabled"] == "true" {
			failures = append(failures, n.Metadata.Name+": fence.alc.xyz/disabled=true")
		}
		for _, t := range n.Spec.Taints {
			if t.Key == "node.kubernetes.io/out-of-service" {
				failures = append(failures, n.Metadata.Name+": node.kubernetes.io/out-of-service taint")
				break
			}
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("self-fence preflight failed: %s", strings.Join(failures, "\n"))
	}

	if !o.readFenceStatus() {
		return fmt.Errorf("%s", o.fence.failure)
	}
	if failures := fenceStateFailures(o.fenceStates()); len(failures) > 0 {
		return fmt.Errorf("self-fence preflight failed: %s", strings.Join(failures, "\n"))
	}
	if o.fence.statusExit != 0 {
		return fmt.Errorf("%s: node-self-fence-status failed (exit %d)", node, o.fence.statusExit)
	}
	if servers, ok := o.fence.status["servers"].(float64); !ok || servers != float64(len(o.fenceStates())) {
		return fmt.Errorf("%s: incomplete self-fence server states", node)
	}
	// The agent's peer list could miss a server: every control-plane Node
	// must have reported a verified state.
	var reported []string
	for _, state := range o.fenceStates() {
		if name, ok := state["node"].(string); ok {
			reported = append(reported, name)
		}
	}
	for _, n := range nodes {
		if _, ok := n.Metadata.Labels["node-role.kubernetes.io/control-plane"]; ok && !slices.Contains(reported, n.Metadata.Name) {
			failures = append(failures, n.Metadata.Name+": control-plane Node has no verified self-fence state")
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("self-fence preflight failed: %s", strings.Join(failures, "\n"))
	}

	mode := jqText(o.ownFenceState()["mode"])
	if o.fence.previousMode == "" {
		o.fence.previousMode = mode
	} else if mode != o.fence.previousMode {
		return fmt.Errorf("%s: agent mode changed from %s to %s during maintenance", node, o.fence.previousMode, mode)
	}
	return nil
}

// managedFieldsNode is a Node read with --show-managed-fields.
type managedFieldsNode struct {
	Metadata struct {
		Annotations   map[string]string `json:"annotations"`
		ManagedFields []struct {
			Manager string `json:"manager"`
			Time    any    `json:"time"`
		} `json:"managedFields"`
	} `json:"metadata"`
}

// readManagedFields reads the target Node with its managed fields.
func (o *Operation) readManagedFields() (managedFieldsNode, bool) {
	var n managedFieldsNode
	err := o.getJSON(&n, "get", "node", o.opts.Node, "-o", "json", "--show-managed-fields=true", "--request-timeout=5s")
	return n, err == nil
}

// managerTimes returns the API-server times, in Unix seconds, of the
// manager's writes to the Node. ok is false when any of them is not a
// timestamp in whole seconds, as jq's fromdateiso8601 requires.
func (n managedFieldsNode) managerTimes(manager string) (times []int64, ok bool) {
	for _, field := range n.Metadata.ManagedFields {
		if field.Manager != manager {
			continue
		}
		value, isString := field.Time.(string)
		if !isString {
			return nil, false
		}
		parsed, err := time.Parse("2006-01-02T15:04:05Z", value)
		if err != nil {
			return nil, false
		}
		times = append(times, parsed.Unix())
	}
	return times, true
}

// managerTime returns the latest write time of the manager on the Node.
func (o *Operation) managerTime(manager string) (int64, bool) {
	n, ok := o.readManagedFields()
	if !ok {
		return 0, false
	}
	times, ok := n.managerTimes(manager)
	if !ok || len(times) == 0 {
		return 0, false
	}
	return slices.Max(times), true
}

// stopFenceAgent rechecks the preflight, stops the target's agent and waits
// for its Node to show mode=stopped, so the fence controller does not count
// the power action as a failure. Failures leave the node cordoned.
func (o *Operation) stopFenceAgent() error {
	if !o.fence.hasAgent {
		return nil
	}
	target := o.opts.Node
	deadline := o.now().Add(o.fenceStopTimeout)
	recovery := target + " remains cordoned; inspect node-self-fence and restore it with systemctl start node-self-fence before retrying maintenance"

	// The drain can take many minutes; peers must still be healthy when the
	// target leaves, or the remaining servers may lose their quorum.
	if err := o.verifyFencePreflight(); err != nil {
		return err
	}
	o.log("stopping self-fence agent on %s before the power action", target)
	if r := o.remoteRootShell(fenceStopScript, true, fenceSSHOptions...); r.Status != 0 {
		return fmt.Errorf("%s: failed to stop node-self-fence; %s", target, recovery)
	}

	for {
		var current node
		if err := o.getJSON(&current, "get", "node", target, "-o", "json", "--request-timeout=5s"); err == nil &&
			current.Metadata.Annotations[agentModeAnnotation] == "stopped" {
			o.fence.agentStopped = true
			// The API-server time of the stop write; a returned heartbeat
			// must be later.
			o.fence.baseline, o.fence.hasBaseline = o.managerTime(agentManager)
			return nil
		}
		if o.expired(deadline) {
			return fmt.Errorf("%s: agent did not publish mode=stopped within %ds; %s", target, int(o.fenceStopTimeout.Seconds()), recovery)
		}
		if err := o.pause(); err != nil {
			return err
		}
	}
}

// waitForReturnedFenceAgent waits, before uncordon, for the target's own
// agent to report healthy and unfenced in its pre-maintenance mode (either
// mode for on) with a heartbeat the API server recorded after the baseline.
// Peer states do not block the return. Failures leave the node cordoned.
func (o *Operation) waitForReturnedFenceAgent() error {
	if !o.fence.hasAgent {
		return nil
	}
	node := o.opts.Node
	deadline := o.now().Add(duration(o.opts.FenceTimeout))
	o.log("waiting for %s healthy self-fence agent and a fresh heartbeat", node)
	for {
		if o.readFenceStatus() {
			if o.fence.statusExit > 1 {
				o.fence.failure = fmt.Sprintf("%s: node-self-fence-status failed (exit %d)", node, o.fence.statusExit)
			} else if o.ownAgentReturned() {
				return nil
			}
		}
		if o.expired(deadline) {
			return fmt.Errorf("%s; fence recovery timed out after %s; %s remains cordoned", o.fence.failure, o.opts.FenceTimeout, node)
		}
		if err := o.pause(); err != nil {
			return err
		}
	}
}

// ownAgentReturned checks the own state of a status report, setting
// fence.failure when the agent has not returned.
func (o *Operation) ownAgentReturned() bool {
	node := o.opts.Node
	own := o.ownFenceState()
	mode := jqText(orElse(own["mode"], "missing"))
	if own["ok"] != true || own["classification"] != "healthy" || own["fenced"] != false ||
		(own["mode"] != "enforce" && own["mode"] != "observe") {
		o.fence.failure = fmt.Sprintf("%s: own agent is not healthy and unfenced (mode=%s, ok=%s, classification=%s, fenced=%s, error=%s)",
			node, mode, jqText(own["ok"]), jqText(orElse(own["classification"], "missing")), jqText(own["fenced"]), jqText(orElse(own["error"], "none")))
		return false
	}
	if o.opts.Action != "on" && mode != o.fence.previousMode {
		o.fence.failure = fmt.Sprintf("%s: agent mode %s differs from pre-maintenance mode %s", node, mode, o.fence.previousMode)
		return false
	}
	// Compare against the stop write or the power-on baseline, both
	// recorded by the API server. The Ready transition carries the kubelet
	// clock and is not compared.
	if n, ok := o.readManagedFields(); ok && n.Metadata.Annotations[agentModeAnnotation] == mode {
		heartbeats, ok := n.managerTimes(agentManager)
		if ok && len(heartbeats) > 0 && (!o.fence.hasBaseline || slices.Max(heartbeats) > o.fence.baseline) {
			o.log("%s self-fence agent recovered in %s mode with a fresh heartbeat", node, mode)
			return true
		}
	}
	o.fence.failure = fmt.Sprintf("%s: no node-self-fence heartbeat after the maintenance baseline", node)
	return false
}
