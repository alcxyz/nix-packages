package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"nix-deploy/bootcheck"
)

// nixfleet status against canned ssh, sh and kubectl answers; nothing reaches
// a real host or cluster.

const statusInventory = `{
  "schemaVersion": 1, "operatorHost": "xyz", "homeManagerUser": "alc", "homeOutputPrefix": "alc-",
  "knownHosts": ["xyz", "node", "solo", "mac", "k1", "spare"],
  "homeManagerHosts": ["xyz", "mac"],
  "remoteHosts": ["node", "solo", "k1"],
  "deployAllHosts": ["xyz", "node", "solo", "mac", "k1"],
  "aliases": {"edge": "node"},
  "sshHosts": {"node": "node.invalid", "solo": "solo.invalid", "k1": "k1.invalid"},
  "systemSshUsers": {"solo": "operator"},
  "systemRemoteSudoHosts": ["solo"],
  "systemActivationModes": {}
}`

// systems renders bootcheck.Script output: the booted system uses kernel k1
// and initrd i1, the current and next-boot systems the given ones; "-"
// leaves an entry unresolved.
func systems(current, kernel, initrd string) string {
	lines := []string{"os=Linux"}
	for _, side := range []string{"booted", "current", "profile"} {
		system, k, i := "/nix/store/aaa-nixos-system-booted", "k1", "i1"
		if side != "booted" {
			system, k, i = current, kernel, initrd
		}
		if i != "-" {
			i = "/nix/store/" + i + "-initrd/initrd"
		}
		lines = append(lines, side+"="+system, side+"-kernel=/nix/store/"+k+"-linux/bzImage", side+"-initrd="+i, side+"-kernel-modules=/nix/store/m1-modules", side+"-kernel-params=p:quiet")
	}
	return strings.Join(lines, "\n") + "\n"
}

const nodesJSON = `{"items": [
  {"metadata": {"name": "edge"}, "spec": {"unschedulable": true},
   "status": {"conditions": [{"type": "Ready", "status": "True"}]}},
  {"metadata": {"name": "k1"}, "spec": {},
   "status": {"conditions": [{"type": "Ready", "status": "False"}]}},
  {"metadata": {"name": "other"}, "spec": {}, "status": {}}
]}`

// fakeFleet answers the status commands and records them.
type fakeFleet struct {
	lock    sync.Mutex
	calls   [][]string
	kubectl func() (string, string, int)
}

func (f *fakeFleet) run(timeout time.Duration, argv ...string) (string, string, int) {
	f.lock.Lock()
	f.calls = append(f.calls, argv)
	f.lock.Unlock()
	if argv[0] == "kubectl" {
		return f.kubectl()
	}
	last := argv[len(argv)-1]
	if argv[0] == "sh" {
		// This machine: switched to a new kernel, with runner units.
		return systems("/nix/store/bbb-nixos-system-new", "k2", "i1") +
			"runner=forgejo-actions-runner.service loaded active running Forgejo Actions Runner (xyz)\n" +
			"runner=● gitea-runner-spare.service loaded failed failed gitea-runner-spare\n", "", 0
	}
	if argv[0] != "ssh" || !strings.HasPrefix(last, "sh -c '") || !slices.Contains(argv, "BatchMode=yes") {
		return "", "unexpected command\n", 97
	}
	switch argv[len(argv)-2] {
	case "root@node.invalid":
		return systems("/nix/store/aaa-nixos-system-booted", "k1", "i1"), "", 0
	case "operator@solo.invalid":
		return "", "ssh: connect to host solo.invalid port 22: Connection timed out\n", 255
	case "root@mac":
		return "os=Darwin\n", "", 0
	case "root@k1.invalid":
		return systems("/nix/store/aaa-nixos-system-booted", "k1", "-"), "", 0
	case "root@spare":
		return "", "", exitTimedOut
	}
	return "", "unexpected host\n", 97
}

func newStatusCheck(t *testing.T, f *fakeFleet) *statusCheck {
	t.Helper()
	inv, err := parseInventory([]byte(statusInventory))
	if err != nil {
		t.Fatal(err)
	}
	if f.kubectl == nil {
		f.kubectl = func() (string, string, int) { return nodesJSON, "", 0 }
	}
	return &statusCheck{inv: inv, self: "xyz", sshArgs: []string{"-o", "ConnectTimeout=30"}, connectTimeout: "5", run: f.run}
}

func hostByName(t *testing.T, report statusReport, host string) hostStatus {
	t.Helper()
	for _, h := range report.Hosts {
		if h.Host == host {
			return h
		}
	}
	t.Fatalf("no %s in %+v", host, report.Hosts)
	return hostStatus{}
}

func TestStatusReadsEachHost(t *testing.T) {
	f := &fakeFleet{}
	s := newStatusCheck(t, f)
	report := s.collect([]string{"xyz", "node", "solo", "mac", "k1", "spare"})

	if got := []string{report.Hosts[0].Host, report.Hosts[5].Host}; !slices.Equal(got, []string{"xyz", "spare"}) {
		t.Errorf("order: %v", got)
	}
	xyz := hostByName(t, report, "xyz")
	if !xyz.Reachable || xyz.RebootNeeded == nil || !*xyz.RebootNeeded || !slices.Equal(xyz.RebootDiffers, []string{"kernel"}) {
		t.Errorf("xyz: %+v", xyz)
	}
	if xyz.Runners == nil || xyz.Runners.Active != 1 || xyz.Runners.Total != 2 || xyz.Runners.Units[1] != (runnerUnit{Unit: "gitea-runner-spare.service", State: "failed"}) {
		t.Errorf("xyz runners: %+v", xyz.Runners)
	}
	if xyz.Node != nil {
		t.Errorf("xyz is no node: %+v", xyz.Node)
	}
	node := hostByName(t, report, "node")
	if node.RebootNeeded == nil || *node.RebootNeeded || node.Runners != nil {
		t.Errorf("node: %+v", node)
	}
	// The node is named by the host's alias.
	if node.Node == nil || *node.Node != (nodeStatus{Name: "edge", Cordoned: true, Ready: true}) {
		t.Errorf("node's node: %+v", node.Node)
	}
	solo := hostByName(t, report, "solo")
	if solo.Reachable || solo.RebootNeeded != nil || !strings.Contains(solo.Error, "Connection timed out") {
		t.Errorf("solo: %+v", solo)
	}
	mac := hostByName(t, report, "mac")
	if !mac.Reachable || mac.OS != "Darwin" || mac.RebootNeeded != nil || mac.Runners != nil {
		t.Errorf("mac: %+v", mac)
	}
	k1 := hostByName(t, report, "k1")
	if !k1.Reachable || k1.RebootNeeded != nil || !strings.Contains(k1.RebootReason, "cannot compare initrd") {
		t.Errorf("k1: %+v", k1)
	}
	if k1.Node == nil || k1.Node.Cordoned || k1.Node.Ready {
		t.Errorf("k1's node: %+v", k1.Node)
	}
	spare := hostByName(t, report, "spare")
	if spare.Reachable || spare.Error != "no answer within 30s" {
		t.Errorf("spare: %+v", spare)
	}
	if !report.Cluster.Reachable || report.Cluster.Nodes != 3 {
		t.Errorf("cluster: %+v", report.Cluster)
	}

	// Only reads: one kubectl get, local sh for this machine, and the boot
	// check over SSH with BatchMode and the status connect timeout first.
	for _, call := range f.calls {
		switch call[0] {
		case "kubectl":
			if !slices.Equal(call, []string{"kubectl", "get", "nodes", "-o", "json", "--request-timeout=10s"}) {
				t.Errorf("kubectl call: %v", call)
			}
		case "sh":
		case "ssh":
			if !slices.Equal(call[1:7], []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "-o", "ConnectionAttempts=1"}) ||
				!strings.Contains(call[len(call)-1], bootcheck.Script) || !strings.Contains(call[len(call)-1], runnerScript) {
				t.Errorf("ssh call: %v", call)
			}
		default:
			t.Errorf("unexpected call: %v", call)
		}
	}
	if len(f.calls) != 7 {
		t.Errorf("%d calls: %v", len(f.calls), f.calls)
	}

	var out bytes.Buffer
	printStatus(&out, report)
	for _, want := range []string{
		"host         ssh          system           reboot   node                  runners  boot\n",
		"xyz          ok           switched         yes      -                     1/2      -\n",
		"node         ok           booted           no       cordoned              -        -\n",
		"solo         unreachable  -                -        -                     -        -\n",
		"mac          ok           darwin           -        -                     -        -\n",
		"k1           ok           booted           unknown  schedulable,notready  -        -\n",
		"xyz: reboot needed: kernel differs between the booted system and the current or next-boot system\n",
		"solo: unreachable: ssh: connect to host solo.invalid port 22: Connection timed out\n",
		"k1: reboot unknown: cannot compare initrd",
		"spare: unreachable: no answer within 30s\n",
		"cluster: 3 nodes\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in:\n%s", want, out.String())
		}
	}
}

// The inventory's kubernetesContext picks the cluster that is read.
func TestStatusReadsInventoryContext(t *testing.T) {
	f := &fakeFleet{}
	s := newStatusCheck(t, f)
	s.inv.KubernetesContext = "lab"
	if report := s.collect([]string{"k1"}); !report.Cluster.Reachable {
		t.Fatalf("cluster: %+v", report.Cluster)
	}
	want := []string{"kubectl", "--context=lab", "get", "nodes", "-o", "json", "--request-timeout=10s"}
	if !slices.ContainsFunc(f.calls, func(call []string) bool { return slices.Equal(call, want) }) {
		t.Errorf("no %v in %v", want, f.calls)
	}
}

func TestStatusStagedSystem(t *testing.T) {
	h := hostStatus{Reachable: true, OS: "Linux", BootedSystem: "/nix/store/a", CurrentSystem: "/nix/store/a", NextBootSystem: "/nix/store/b"}
	if got := systemState(h); got != "booted,staged" {
		t.Errorf("got %q", got)
	}
}

// A cluster that cannot be read leaves every node unknown and fails nothing.
func TestStatusWithoutCluster(t *testing.T) {
	cases := map[string]struct {
		answer func() (string, string, int)
		want   string
	}{
		"unreachable": {func() (string, string, int) {
			return "", "E1016 memcache.go\nThe connection to the server was refused\n", 1
		}, "cluster: not read: The connection to the server was refused\n"},
		"missing":   {func() (string, string, int) { return "", "", 127 }, "cluster: not read: kubectl is not available\n"},
		"slow":      {func() (string, string, int) { return "", "", exitTimedOut }, "cluster: not read: kubectl did not answer within 15s\n"},
		"malformed": {func() (string, string, int) { return "not json", "", 0 }, "cluster: not read: kubectl output does not decode"},
	}
	for name, c := range cases {
		f := &fakeFleet{kubectl: c.answer}
		report := newStatusCheck(t, f).collect([]string{"node", "k1"})
		if report.Cluster.Reachable || report.Hosts[0].Node != nil || report.Hosts[1].Node != nil {
			t.Errorf("%s: %+v", name, report)
		}
		var out bytes.Buffer
		printStatus(&out, report)
		if !strings.Contains(out.String(), c.want) {
			t.Errorf("%s: missing %q in:\n%s", name, c.want, out.String())
		}
	}
}

func TestStatusJSONShape(t *testing.T) {
	report := newStatusCheck(t, &fakeFleet{}).collect([]string{"xyz", "solo", "mac"})
	path := filepath.Join(t.TempDir(), "status.json")
	if err := writeStatusJSON(path, report); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	if document["version"] != version {
		t.Errorf("version: %v", document["version"])
	}
	cluster := document["cluster"].(map[string]any)
	if cluster["reachable"] != true || cluster["nodes"] != 3.0 {
		t.Errorf("cluster: %v", cluster)
	}
	hosts := document["hosts"].([]any)
	keys := []string{"bootCheck", "bootedSystem", "currentSystem", "host", "nextBootSystem", "node", "os", "reachable", "rebootDiffers", "rebootNeeded", "rebootReason", "runners"}
	xyz := hosts[0].(map[string]any)
	if got := sortedKeys(xyz); !slices.Equal(got, keys) {
		t.Errorf("xyz keys: %v", got)
	}
	if xyz["rebootNeeded"] != true || xyz["currentSystem"] != "/nix/store/bbb-nixos-system-new" || xyz["node"] != nil {
		t.Errorf("xyz: %v", xyz)
	}
	runners := xyz["runners"].(map[string]any)
	if runners["active"] != 1.0 || runners["total"] != 2.0 || len(runners["units"].([]any)) != 2 {
		t.Errorf("runners: %v", runners)
	}
	// An unreachable host carries its error; unknown values are null or
	// empty, never a guess.
	solo := hosts[1].(map[string]any)
	withError := append([]string{"error"}, keys...)
	slices.Sort(withError)
	if got := sortedKeys(solo); !slices.Equal(got, withError) {
		t.Errorf("solo keys: %v", got)
	}
	if solo["reachable"] != false || solo["rebootNeeded"] != nil || len(solo["rebootDiffers"].([]any)) != 0 {
		t.Errorf("solo: %v", solo)
	}
	mac := hosts[2].(map[string]any)
	if mac["os"] != "Darwin" || mac["rebootNeeded"] != nil || mac["rebootReason"] != "Darwin host, not NixOS" {
		t.Errorf("mac: %v", mac)
	}
}

func sortedKeys(m map[string]any) []string {
	var keys []string
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func TestStatusArguments(t *testing.T) {
	t.Setenv("NIX_DEPLOY_CONFIG", "/env.json")
	opts, err := parseStatusArgs([]string{"node", "--json", "out.json", "edge", "--config=/x.json"})
	if err != nil || opts.configFile != "/x.json" || opts.jsonPath != "out.json" || !slices.Equal(opts.hosts, []string{"node", "edge"}) {
		t.Fatalf("got %+v, %v", opts, err)
	}
	for _, args := range [][]string{{"--all", "node"}, {"--bogus"}, {"--json"}, {"--config="}, {"--json="}} {
		if _, err := parseStatusArgs(args); err == nil || err == errStatusHelp {
			t.Errorf("%v: accepted", args)
		}
	}
	if _, err := parseStatusArgs([]string{"--help"}); err != errStatusHelp {
		t.Errorf("help: %v", err)
	}

	inv, err := parseInventory([]byte(statusInventory))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		self string
		opts statusOptions
		want []string
	}{
		{"xyz", statusOptions{}, []string{"xyz"}},
		{"xyz", statusOptions{all: true}, []string{"xyz", "node", "solo", "mac", "k1"}},
		// Away from the operator host, --all selects what deploy --all does.
		{"node", statusOptions{all: true}, []string{"node", "solo", "k1"}},
		{"xyz", statusOptions{hosts: []string{"edge", "node", "mac"}}, []string{"node", "mac"}},
	}
	for _, c := range cases {
		got, err := selectHosts(inv, c.self, c.opts)
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("%s %+v: got %v, %v", c.self, c.opts, got, err)
		}
	}
	if _, err := selectHosts(inv, "xyz", statusOptions{hosts: []string{"nope"}}); err == nil || !strings.Contains(err.Error(), "unknown host 'nope'") {
		t.Errorf("unknown host: %v", err)
	}
}

func TestParseRunners(t *testing.T) {
	if parseRunners("os=Linux\n") != nil {
		t.Error("no runner lines must report none")
	}
	r := parseRunners("runner=forgejo-podman-runner.service loaded inactive dead canary\nrunner=short\n")
	if r == nil || r.Total != 1 || r.Active != 0 || r.Units[0].State != "inactive" {
		t.Errorf("got %+v", r)
	}
}

// With the inventory's bootCheck, each host's result for its current boot
// is read and shown; without it the script does not look.
func TestStatusReadsBootCheck(t *testing.T) {
	f := &fakeFleet{}
	s := newStatusCheck(t, f)
	report := s.collect([]string{"node"})
	if report.Hosts[0].BootCheck != nil || strings.Contains(f.calls[len(f.calls)-1][len(f.calls[len(f.calls)-1])-1], "nixfleet-boot-check") {
		t.Errorf("boot check read without bootCheck: %+v", report.Hosts[0].BootCheck)
	}

	s.inv.BootCheck = true
	cases := []struct {
		output string
		want   *bootCheckStatus
		row    string
		note   string
	}{
		{"", nil, "-", ""},
		{"bootcheck.status=pending\n", &bootCheckStatus{Status: "pending", Reasons: []string{}}, "pending", ""},
		{"bootcheck.status=ok\nbootcheck.time=1700000000\n", &bootCheckStatus{Status: "ok", Time: 1700000000, Reasons: []string{}}, "ok", ""},
		{"bootcheck.status=failed\nbootcheck.time=1\nbootcheck.reason=unit a.service failed\nbootcheck.reason=booted x but the system profile is y\n",
			&bootCheckStatus{Status: "failed", Time: 1, Reasons: []string{"unit a.service failed", "booted x but the system profile is y"}}, "failed",
			"node: boot check failed: unit a.service failed; booted x but the system profile is y\n"},
		{"bootcheck.present=1\n", &bootCheckStatus{Status: "failed", Reasons: []string{"the result has no status"}}, "failed",
			"node: boot check failed: the result has no status\n"},
		{"bootcheck.status=weird\n", &bootCheckStatus{Status: "failed", Reasons: []string{`the result has status "weird"`}}, "failed",
			`node: boot check failed: the result has status "weird"` + "\n"},
	}
	for _, c := range cases {
		check := &statusCheck{inv: s.inv, self: "xyz", connectTimeout: "5", run: func(timeout time.Duration, argv ...string) (string, string, int) {
			if argv[0] == "kubectl" {
				return nodesJSON, "", 0
			}
			if !strings.Contains(argv[len(argv)-1], "/run/nixfleet-boot-check/result") {
				t.Errorf("no boot check read in %v", argv)
			}
			return systems("/nix/store/aaa-nixos-system-booted", "k1", "i1") + c.output, "", 0
		}}
		report := check.collect([]string{"node"})
		got := report.Hosts[0].BootCheck
		if (got == nil) != (c.want == nil) || got != nil && (got.Status != c.want.Status || got.Time != c.want.Time || !slices.Equal(got.Reasons, c.want.Reasons)) {
			t.Errorf("%q: got %+v, want %+v", c.output, got, c.want)
		}
		var out bytes.Buffer
		printStatus(&out, report)
		if !strings.Contains(out.String(), "cordoned              -        "+c.row+"\n") || !strings.Contains(out.String(), c.note) {
			t.Errorf("%q: row %q or note %q missing in:\n%s", c.output, c.row, c.note, out.String())
		}
	}
}

// The boot check script reads an unterminated last line, tells an empty
// result from a missing check, and fails a result it cannot read.
func TestBootCheckScript(t *testing.T) {
	dir := t.TempDir()
	result, unit := filepath.Join(dir, "result"), filepath.Join(dir, "unit")
	run := func() string {
		t.Helper()
		out, err := exec.Command("sh", "-c", bootCheckScriptFor(result, unit)).Output()
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	if out := run(); out != "" {
		t.Errorf("no unit, no result: %q", out)
	}
	if err := os.WriteFile(unit, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if out := run(); out != "bootcheck.status=pending\n" {
		t.Errorf("unit, no result: %q", out)
	}
	if err := os.WriteFile(result, []byte("status=failed\nreason=unit a.service failed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := parseBootCheck(run()); got == nil || got.Status != "failed" || !slices.Equal(got.Reasons, []string{"unit a.service failed"}) {
		t.Errorf("unterminated last line: %+v", got)
	}
	if err := os.WriteFile(result, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := parseBootCheck(run()); got == nil || got.Status != "failed" {
		t.Errorf("empty result: %+v", got)
	}
	if os.Geteuid() != 0 {
		if err := os.Chmod(result, 0); err != nil {
			t.Fatal(err)
		}
		if got := parseBootCheck(run()); got == nil || got.Status != "failed" || len(got.Reasons) != 1 || !strings.HasPrefix(got.Reasons[0], "the result cannot be read as ") {
			t.Errorf("unreadable result: %+v", got)
		}
	}
}
