package k8snode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// The fake below stands in for kubectl and ssh: it serves mutable cluster
// and host state as JSON, applies the mutations a run makes (cordon, drain,
// patch, annotate, power actions) and records every call. No test reaches a
// real cluster or host.

const timeLayout = "2006-01-02T15:04:05Z"

// call is one recorded command.
type call struct {
	line  string
	stdin string
}

type fakeCluster struct {
	t     *testing.T
	clock time.Time
	calls []call
	// contexts are the --context options kubectl calls carried, in order.
	contexts []string
	// sleeps counts the operation's poll pauses.
	sleeps int
	// audits records each network audit as node, expected IP and SSH target.
	audits []string

	nodes       []map[string]any
	pods        []map[string]any
	controllers map[string]map[string]any // by namespace/kind/name
	longhorn    bool
	volumes     []map[string]any
	replicas    []map[string]any
	settings    map[string]string
	backup      map[string]any
	attachments []map[string]any
	pdbs        []map[string]any
	limitRanges []map[string]any
	cnpg        bool
	clusters    []map[string]any
	// standbys overrides, by replica Pod name, the "state|lag" a primary
	// reports in pg_stat_replication; other running replicas stream with no
	// lag.
	standbys map[string]string

	// status is the self-fence status report; statusText, when set, is
	// served instead.
	status         map[string]any
	statusText     *string
	statusExit     int
	stopExit       int
	agentPublishes bool
	// Baseline annotation failures and the managed field the write leaves;
	// an empty baselineTime is the fake clock's time.
	baselineWriteExit   int
	baselineReadExit    int
	baselineCleanupExit int
	baselineManager     string
	baselineTime        string

	privilegeExit int
	storageExit   int
	auditExit     int
	// flannelDown is the number of Flannel probes that fail before the
	// interface appears.
	flannelDown int
	bootID      string
	rebootExit  int
	// rebootKeepsBoot makes the host come back with the same boot ID.
	rebootKeepsBoot bool
	// hostDown is the number of SSH probes that fail before the host is up.
	hostDown   int
	poweredOff bool
	// sshNeverDrops keeps SSH answering after a power action.
	sshNeverDrops bool
	drainExit     int
	rolloutExit   int

	// respond, when set, answers commands before the defaults.
	respond func(c Command, line string) (Result, bool)
}

func (f *fakeCluster) run(c Command) Result {
	// The context option is recorded and then answered like any other
	// call, so the fixtures match command lines without it. Like kubectl, a
	// flag before a plugin's name fails.
	if len(c.Argv) > 2 && c.Argv[0] == "kubectl" {
		if strings.HasPrefix(c.Argv[1], "--context=") && c.Argv[2] == "cnpg" {
			f.calls = append(f.calls, call{line: strings.Join(c.Argv, " ")})
			return Result{Status: 1, Stderr: "Error: flags cannot be placed before plugin name: " + c.Argv[1] + "\n"}
		}
		at := 1
		if c.Argv[1] == "cnpg" {
			at = 2
		}
		if strings.HasPrefix(c.Argv[at], "--context=") {
			f.contexts = append(f.contexts, c.Argv[at])
			c.Argv = append(slices.Clone(c.Argv[:at]), c.Argv[at+1:]...)
		}
	}
	line := strings.Join(c.Argv, " ")
	f.calls = append(f.calls, call{line: line, stdin: c.Stdin})
	if f.respond != nil {
		if result, ok := f.respond(c, line); ok {
			return result
		}
	}
	switch c.Argv[0] {
	case "kubectl":
		return f.kubectl(c.Argv[1:], line)
	case "ssh":
		return f.ssh(c, line)
	case "command":
		return Result{}
	}
	f.t.Errorf("unexpected command: %s", line)
	return Result{Status: 97}
}

func (f *fakeCluster) unexpected(line string) Result {
	f.t.Errorf("unexpected command: %s", line)
	return Result{Status: 97, Stderr: "unexpected command\n"}
}

func serve(v any) Result {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return Result{Stdout: string(data) + "\n"}
}

func list(items []map[string]any) Result {
	if items == nil {
		items = []map[string]any{}
	}
	return serve(map[string]any{"items": items})
}

func (f *fakeCluster) node(name string) map[string]any {
	for _, n := range f.nodes {
		if get(n, "metadata", "name") == name {
			return n
		}
	}
	return nil
}

// withoutManagedFields copies a Node as kubectl shows it by default.
func withoutManagedFields(n map[string]any) map[string]any {
	c := clone(n).(map[string]any)
	if metadata, ok := c["metadata"].(map[string]any); ok {
		delete(metadata, "managedFields")
	}
	return c
}

func (f *fakeCluster) kubectl(args []string, line string) Result {
	command := strings.Join(args, " ")
	switch {
	case len(args) >= 3 && args[0] == "get" && args[1] == "node":
		n := f.node(args[2])
		if n == nil {
			return Result{Status: 1, Stderr: "node not found\n"}
		}
		if len(args) == 3 {
			return Result{}
		}
		if slices.Contains(args, "--show-managed-fields=true") {
			if f.baselineReadExit != 0 {
				return Result{Status: f.baselineReadExit}
			}
			return serve(n)
		}
		return serve(withoutManagedFields(n))
	case command == "get nodes -o json":
		var items []map[string]any
		for _, n := range f.nodes {
			items = append(items, withoutManagedFields(n))
		}
		return list(items)
	case command == "get nodes -l workload-class=stable -o json":
		var items []map[string]any
		for _, n := range f.nodes {
			if get(n, "metadata", "labels", "workload-class") == "stable" {
				items = append(items, withoutManagedFields(n))
			}
		}
		return list(items)
	case command == "get pods --all-namespaces -o json":
		return list(f.pods)
	case strings.HasPrefix(command, "get pods --all-namespaces --field-selector spec.nodeName="):
		name := strings.TrimSuffix(strings.TrimPrefix(command, "get pods --all-namespaces --field-selector spec.nodeName="), " -o json")
		return list(f.podsWhere(func(p map[string]any) bool { return get(p, "spec", "nodeName") == name }))
	case command == "get pods --all-namespaces --field-selector status.phase=Pending -o json":
		return list(f.podsWhere(func(p map[string]any) bool { return get(p, "status", "phase") == "Pending" }))
	case len(args) == 6 && args[0] == "-n" && args[2] == "get" && args[3] == "limitranges":
		return list(f.limitRanges)
	case len(args) == 6 && args[0] == "-n" && args[2] == "get" && args[3] == "pods":
		return list(f.podsWhere(func(p map[string]any) bool { return get(p, "metadata", "namespace") == args[1] }))
	case len(args) == 7 && args[0] == "-n" && args[2] == "get" && (args[3] == "replicaset" || args[3] == "statefulset" || args[3] == "controllerrevision"):
		return f.controller(args[1], args[3]+"/"+args[4])
	case len(args) == 6 && args[0] == "-n" && args[2] == "get" && strings.Contains(args[3], "/"):
		return f.controller(args[1], args[3])
	case len(args) >= 4 && args[0] == "-n" && args[2] == "rollout" && args[3] == "status":
		return Result{Status: f.rolloutExit}
	case len(args) >= 4 && args[0] == "-n" && args[2] == "wait":
		return Result{}
	case strings.HasPrefix(command, "get clusters.postgresql.cnpg.io --all-namespaces"):
		if !f.cnpg {
			return Result{Status: 1, Stderr: "the server doesn't have a resource type\n"}
		}
		return list(f.clusters)
	case len(args) == 11 && args[0] == "-n" && args[2] == "exec" && args[4] == "-c" && args[5] == "postgres" &&
		strings.Join(args[6:10], " ") == "-- psql -XAt -c" && args[10] == replicationQuery:
		return f.replication(args[1], args[3])
	case len(args) == 6 && args[0] == "cnpg" && args[1] == "promote":
		return f.promote(args[3], args[4], args[5])
	case command == "get namespace longhorn-system":
		if !f.longhorn {
			return Result{Status: 1}
		}
		return Result{}
	case strings.HasPrefix(command, "-n longhorn-system get volumes.longhorn.io"):
		if !f.longhorn {
			return Result{Status: 1}
		}
		return list(f.volumes)
	case command == "-n longhorn-system get replicas.longhorn.io -o json":
		return list(f.replicas)
	case len(args) == 7 && strings.HasPrefix(command, "-n longhorn-system get settings.longhorn.io "):
		value, ok := f.settings[args[4]]
		if !ok {
			return Result{Status: 1}
		}
		return serve(map[string]any{"metadata": map[string]any{"name": args[4]}, "value": value})
	case len(args) == 8 && strings.HasPrefix(command, "-n longhorn-system patch settings.longhorn.io "):
		var patch struct{ Value string }
		if args[5] != "--type=merge" || args[6] != "-p" || json.Unmarshal([]byte(args[7]), &patch) != nil {
			return f.unexpected(line)
		}
		f.settings[args[4]] = patch.Value
		return Result{Stdout: "setting patched\n"}
	case command == "-n longhorn-system get backuptargets.longhorn.io default -o json":
		if f.backup == nil {
			return Result{Status: 1}
		}
		return serve(f.backup)
	case command == "get poddisruptionbudgets.policy --all-namespaces -o json":
		return list(f.pdbs)
	case command == "get volumeattachments.storage.k8s.io -o json":
		return list(f.attachments)
	case len(args) == 2 && args[0] == "cordon":
		set(f.node(args[1]), true, "spec", "unschedulable")
		return Result{Stdout: "node/" + args[1] + " cordoned\n"}
	case len(args) == 2 && args[0] == "uncordon":
		set(f.node(args[1]), false, "spec", "unschedulable")
		return Result{Stdout: "node/" + args[1] + " uncordoned\n"}
	case len(args) >= 2 && args[0] == "drain":
		if f.drainExit != 0 {
			return Result{Status: f.drainExit, Stderr: "error: cannot evict pod\n"}
		}
		f.drain(args[1])
		return Result{Stdout: "node/" + args[1] + " drained\n"}
	case len(args) == 4 && args[0] == "wait" && strings.HasPrefix(args[1], "node/") && args[2] == "--for=condition=Ready":
		return Result{Stdout: args[1] + " condition met\n"}
	case len(args) == 7 && args[0] == "annotate" && strings.HasPrefix(args[3], baselineAnnotation+"="):
		if strings.Join(args[4:], " ") != "--overwrite --field-manager=k8s-node-reboot --request-timeout=5s" {
			return f.unexpected(line)
		}
		if f.baselineWriteExit != 0 {
			return Result{Status: f.baselineWriteExit}
		}
		n := f.node(args[2])
		set(n, strings.TrimPrefix(args[3], baselineAnnotation+"="), "metadata", "annotations", baselineAnnotation)
		baselineTime := f.baselineTime
		if baselineTime == "" {
			baselineTime = f.clock.Format(timeLayout)
		}
		appendTo(n, map[string]any{"manager": f.baselineManager, "time": baselineTime}, "metadata", "managedFields")
		return Result{}
	case command == "annotate node "+argAt(args, 2)+" "+baselineAnnotation+"- --request-timeout=5s":
		if f.baselineCleanupExit != 0 {
			return Result{Status: f.baselineCleanupExit}
		}
		del(f.node(args[2]), "metadata", "annotations", baselineAnnotation)
		return Result{}
	}
	return f.unexpected(line)
}

func argAt(args []string, i int) string {
	if i < len(args) {
		return args[i]
	}
	return ""
}

func (f *fakeCluster) podsWhere(match func(map[string]any) bool) []map[string]any {
	var pods []map[string]any
	for _, p := range f.pods {
		if match(p) {
			pods = append(pods, p)
		}
	}
	return pods
}

func (f *fakeCluster) controller(namespace, ref string) Result {
	c, ok := f.controllers[namespace+"/"+ref]
	if !ok {
		return Result{Status: 1, Stderr: "not found\n"}
	}
	return serve(c)
}

// drain evicts every Pod but DaemonSet Pods and the Longhorn
// instance-manager, and recreates controller-owned ones on another node.
func (f *fakeCluster) drain(name string) {
	var kept []map[string]any
	for _, p := range f.pods {
		if get(p, "spec", "nodeName") != name || ownerKind(p) == "DaemonSet" ||
			get(p, "metadata", "labels", "longhorn.io/component") == "instance-manager" {
			kept = append(kept, p)
			continue
		}
		if ownerKind(p) == "" {
			continue
		}
		moved := clone(p).(map[string]any)
		set(moved, get(moved, "metadata", "name").(string)+"-moved", "metadata", "name")
		set(moved, "server-c", "spec", "nodeName")
		kept = append(kept, moved)
	}
	f.pods = kept
}

func ownerKind(p map[string]any) string {
	kind, _ := get(p, "metadata", "ownerReferences", 0, "kind").(string)
	return kind
}

// promote moves a CloudNativePG primary to the candidate.
func (f *fakeCluster) promote(namespace, cluster, candidate string) Result {
	for _, c := range f.clusters {
		if get(c, "metadata", "namespace") == namespace && get(c, "metadata", "name") == cluster {
			old := get(c, "status", "currentPrimary")
			set(c, candidate, "status", "currentPrimary")
			for _, p := range f.pods {
				switch get(p, "metadata", "name") {
				case candidate:
					set(p, "primary", "metadata", "labels", "cnpg.io/instanceRole")
				case old:
					set(p, "replica", "metadata", "labels", "cnpg.io/instanceRole")
				}
			}
			return Result{Stdout: "promoted\n"}
		}
	}
	return Result{Status: 1}
}

// replication answers replicationQuery on a primary: every other running
// instance of its cluster, streaming with no lag unless standbys says
// otherwise.
func (f *fakeCluster) replication(namespace, primary string) Result {
	var cluster string
	for _, c := range f.clusters {
		if get(c, "metadata", "namespace") == namespace && get(c, "status", "currentPrimary") == primary {
			cluster, _ = get(c, "metadata", "name").(string)
		}
	}
	if cluster == "" {
		return Result{Status: 1, Stderr: "not a primary\n"}
	}
	var out strings.Builder
	for _, p := range f.pods {
		name, _ := get(p, "metadata", "name").(string)
		if get(p, "metadata", "namespace") != namespace || name == primary ||
			get(p, "metadata", "labels", "cnpg.io/cluster") != cluster || get(p, "status", "phase") != "Running" {
			continue
		}
		state, ok := f.standbys[name]
		if !ok {
			state = "streaming|0"
		}
		if state == "" {
			continue
		}
		fmt.Fprintf(&out, "%s|%s\n", name, state)
	}
	return Result{Stdout: out.String()}
}

// targetNode is the Node the fixtures power-cycle.
func (f *fakeCluster) targetNode() map[string]any {
	return f.nodes[0]
}

func (f *fakeCluster) ssh(c Command, line string) Result {
	argv := c.Argv[1:]
	last := argv[len(argv)-1]
	// Only the power commands (-T) may omit BatchMode.
	if !slices.Contains(argv, "-T") && !slices.Contains(argv, "BatchMode=yes") {
		return f.unexpected(line)
	}
	switch {
	case last == privilegeCheckCommand:
		return Result{Status: f.privilegeExit}
	case last == remoteRootShellCommand && c.Stdin == fenceStatusScript:
		if f.statusText != nil {
			return Result{Stdout: *f.statusText, Status: f.statusExit}
		}
		result := serve(f.status)
		result.Status = f.statusExit
		return result
	case last == remoteRootShellCommand && c.Stdin == fenceStopScript:
		if f.stopExit == 0 && f.agentPublishes {
			n := f.targetNode()
			set(n, "stopped", "metadata", "annotations", agentModeAnnotation)
			appendTo(n, map[string]any{"manager": agentManager, "time": f.clock.Format(timeLayout)}, "metadata", "managedFields")
		}
		return Result{Status: f.stopExit}
	case last == remoteRootShellCommand && strings.HasPrefix(c.Stdin, "cleanup_sessions="):
		return Result{Status: f.storageExit}
	case strings.Contains(last, "--expected-node-ip"):
		fields := strings.Fields(last)
		auditNode := fields[slices.Index(fields, "--node")+1]
		expectedIP := fields[slices.Index(fields, "--expected-node-ip")+1]
		f.audits = append(f.audits, auditNode+"\t"+expectedIP+"\t"+argv[len(argv)-2])
		if c.Stdin == "" {
			f.t.Errorf("network audit for %s had no helper script on its input", auditNode)
		}
		return Result{Status: f.auditExit}
	case last == "ip link show dev flannel.1 >/dev/null 2>&1":
		if f.flannelDown > 0 {
			f.flannelDown--
			return Result{Status: 1}
		}
		return Result{}
	case last == "true":
		if f.poweredOff || f.hostDown > 0 {
			if f.hostDown > 0 {
				f.hostDown--
			}
			return Result{Status: 255}
		}
		return Result{}
	case last == "/proc/sys/kernel/random/boot_id":
		return Result{Stdout: f.bootID + "\n"}
	case last == rebootCommand:
		if f.rebootExit != 0 {
			return Result{Status: f.rebootExit}
		}
		if !f.sshNeverDrops {
			f.hostDown = 2
		}
		if !f.rebootKeepsBoot {
			f.bootID += "-next"
		}
		if f.agentPublishes {
			// The agent starts with the host and writes a heartbeat.
			n := f.targetNode()
			mode, _ := get(f.status, "states", 0, "mode").(string)
			set(n, mode, "metadata", "annotations", agentModeAnnotation)
			appendTo(n, map[string]any{"manager": agentManager, "time": f.clock.Add(time.Minute).Format(timeLayout)}, "metadata", "managedFields")
		}
		return Result{Status: 255}
	case last == powerOffCommand:
		if f.rebootExit != 0 {
			return Result{Status: f.rebootExit}
		}
		if !f.sshNeverDrops {
			f.poweredOff = true
		}
		return Result{}
	}
	return f.unexpected(line)
}

// called reports whether any recorded call matches the pattern.
func (f *fakeCluster) called(pattern string) bool {
	re := regexp.MustCompile(pattern)
	for _, c := range f.calls {
		if re.MatchString(c.line) || re.MatchString(c.stdin) {
			return true
		}
	}
	return false
}

// mutatingCall is every command that changes cluster or host state.
const mutatingCall = `^kubectl (cordon|uncordon|drain|patch|annotate|cnpg promote|-n \S+ patch)|systemctl (stop|reboot|poweroff)|cleanup_sessions=true`

func (f *fakeCluster) mutations() []string {
	re := regexp.MustCompile(mutatingCall)
	var lines []string
	for _, c := range f.calls {
		if re.MatchString(c.line) || re.MatchString(c.stdin) {
			lines = append(lines, c.line)
		}
	}
	return lines
}

// newOperation runs opts against the fake with a fake clock; the operator's
// output and errors share one buffer.
func newOperation(f *fakeCluster, opts Options) (*Operation, *bytes.Buffer) {
	var output bytes.Buffer
	runner := &Runner{Stdout: &output, Stderr: &output, Fake: f.run}
	o := New(context.Background(), opts, runner)
	o.now = func() time.Time { return f.clock }
	o.sleep = func(ctx context.Context, d time.Duration) error {
		if ctx.Err() != nil {
			return errInterrupted
		}
		f.sleeps++
		f.clock = f.clock.Add(d)
		return nil
	}
	return o, &output
}

// options parses a command line with a network audit helper in place.
func options(t *testing.T, action string, args ...string) Options {
	t.Helper()
	script := filepath.Join(t.TempDir(), "k8s-node-network-audit")
	if err := os.WriteFile(script, []byte("#!/usr/bin/env bash\necho fixture audit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("K8S_NODE_NETWORK_AUDIT_SCRIPT", script)
	t.Setenv("K8S_NODE_SSH_USER", "")
	opts, err := ParseArgs(action, args)
	if err != nil {
		t.Fatalf("ParseArgs(%s %v): %v", action, args, err)
	}
	return opts
}

// expectFailure requires err to fail with a message containing want.
func expectFailure(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected failure containing %q", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("missing diagnostic %q in %q", want, err.Error())
	}
}

// expectRunFailure requires a full run to fail and its output to contain
// every wanted line fragment.
func expectRunFailure(t *testing.T, o *Operation, output *bytes.Buffer, want ...string) {
	t.Helper()
	if status := o.Run(); status == 0 {
		t.Fatalf("run unexpectedly passed:\n%s", output)
	}
	for _, w := range want {
		if !strings.Contains(output.String(), w) {
			t.Fatalf("missing %q in output:\n%s", w, output)
		}
	}
}

// The helpers below edit decoded JSON documents in place. A path mixes
// object keys (string) and array indices (int).

func decode(t *testing.T, text string) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		t.Fatalf("fixture JSON: %v", err)
	}
	return v
}

func clone(v any) any {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	var c any
	if err := json.Unmarshal(data, &c); err != nil {
		panic(err)
	}
	return c
}

func get(doc any, path ...any) any {
	for _, key := range path {
		switch k := key.(type) {
		case string:
			m, ok := doc.(map[string]any)
			if !ok {
				return nil
			}
			doc = m[k]
		case int:
			a, ok := doc.([]any)
			if !ok || k >= len(a) {
				return nil
			}
			doc = a[k]
		}
	}
	return doc
}

// set stores value at path, creating missing objects on the way.
func set(doc any, value any, path ...any) {
	parent := doc
	for _, key := range path[:len(path)-1] {
		switch k := key.(type) {
		case string:
			m := parent.(map[string]any)
			if m[k] == nil {
				m[k] = map[string]any{}
			}
			parent = m[k]
		case int:
			parent = parent.([]any)[k]
		}
	}
	switch k := path[len(path)-1].(type) {
	case string:
		parent.(map[string]any)[k] = value
	case int:
		parent.([]any)[k] = value
	}
}

func del(doc any, path ...any) {
	if m, ok := get(doc, path[:len(path)-1]...).(map[string]any); ok {
		delete(m, path[len(path)-1].(string))
	}
}

func appendTo(doc any, value any, path ...any) {
	items, _ := get(doc, path...).([]any)
	set(doc, append(items, value), path...)
}

func mustContain(t *testing.T, output, want string) {
	t.Helper()
	if !strings.Contains(output, want) {
		t.Fatalf("missing %q in:\n%s", want, output)
	}
}

func mustNotCall(t *testing.T, f *fakeCluster, pattern string) {
	t.Helper()
	if f.called(pattern) {
		t.Fatalf("unexpected call matching %q:\n%s", pattern, f.callLog())
	}
}

func mustCall(t *testing.T, f *fakeCluster, pattern string) {
	t.Helper()
	if !f.called(pattern) {
		t.Fatalf("no call matching %q:\n%s", pattern, f.callLog())
	}
}

func (f *fakeCluster) callLog() string {
	var b strings.Builder
	for _, c := range f.calls {
		fmt.Fprintln(&b, c.line)
	}
	return b.String()
}
