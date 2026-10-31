package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeployArguments(t *testing.T) {
	d := &deployer{preflight: true}
	opts, err := d.parseOptions([]string{"--build-only", "--force", "--nixos", "edge", "--json=out.json", "--light"})
	if err != nil || opts.mode != "nixos" || opts.target != "edge" || opts.all {
		t.Fatalf("got %+v, %v", opts, err)
	}
	if !d.buildOnly || !d.force || d.dryRun || d.jsonPath != "out.json" || d.settings.buildProfile != "light" {
		t.Errorf("deployer switches: %+v", d)
	}
	d = &deployer{preflight: true}
	if opts, err := d.parseOptions([]string{"--all", "--build-only", "--no-preflight"}); err != nil || !opts.all || opts.mode != "full" || !d.buildOnly || d.preflight {
		t.Errorf("--all --build-only: %+v, %v, %+v", opts, err, d)
	}
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"--build-only", "--dry-run"}, "--build-only and --dry-run cannot be combined"},
		{[]string{"--dry-run", "--all", "--build-only"}, "--build-only and --dry-run cannot be combined"},
		{[]string{"--all", "--build-only", "--fail-unreachable"}, "--build-only and --fail-unreachable cannot be combined"},
		{[]string{"--json"}, "--json requires a file"},
		{[]string{"--json="}, "--json requires a file"},
		{[]string{"--build"}, "unknown option '--build'"},
	}
	for _, c := range cases {
		if _, err := (&deployer{}).parseOptions(c.args); err == nil || err.Error() != c.want {
			t.Errorf("%v: got %v, want %q", c.args, err, c.want)
		}
	}
	if _, err := (&deployer{}).parseOptions([]string{"--bogus"}); !errors.Is(err, errUnknownOption) {
		t.Errorf("unknown option: %v", err)
	}
	if _, err := (&deployer{}).parseOptions([]string{"node", "-h"}); err != errDeployHelp {
		t.Errorf("help: %v", err)
	}
}

const buildOnlyInventory = `{
  "schemaVersion": 1, "operatorHost": "xyz", "homeManagerUser": "alc", "homeOutputPrefix": "alc-",
  "knownHosts": ["xyz", "node", "solo", "k1", "k2"],
  "homeManagerHosts": ["xyz", "node"],
  "remoteHosts": ["node", "solo", "k1", "k2"],
  "deployAllHosts": ["xyz", "node", "solo", "k1", "k2"],
  "sshHosts": {"node": "node.invalid", "solo": "solo.invalid", "k1": "k1.invalid", "k2": "k2.invalid"},
  "aliases": {"edge": "node"},
  "systemSshUsers": {"solo": "operator"},
  "systemRemoteSudoHosts": ["solo"],
  "systemActivationModes": {},
  "serialSystemHosts": ["k1", "k2"],
  "serialReadyCommand": "ready-check {host}"
}`

// fakeCommand stands in for every command a deploy could run. It logs each
// call; nix evaluates and builds to made-up store paths, a build of a
// derivation named in FAKE_BUILD_FAIL fails, so does the evaluation of an
// attribute named in FAKE_EVAL_FAIL, and ssh never connects, so the plan
// cannot read what a remote host runs.
const fakeCommand = `#!/bin/sh
name=${0##*/}
printf '%s %s\n' "$name" "$*" >> "$FAKE_LOG"
case "$name" in
nix)
  case "$1" in
  eval)
    case "$4" in *"${FAKE_EVAL_FAIL:-never}"*) exit 1 ;; esac
    n=$(printf %s "$4" | tr -c 'a-zA-Z0-9' '-')
    printf '{"drv":"/nix/store/h%s.drv","out":"/nix/store/h%s"}\n' "$n" "$n" ;;
  build|path-info)
    if [ -n "$FAKE_BUILD_FAIL" ]; then
      for arg in "$@"; do
        case "$arg" in *"$FAKE_BUILD_FAIL"*) exit 1 ;; esac
      done
    fi
    if [ "$1" = path-info ]; then d=${2%^out}; echo "${d%.drv}"; fi ;;
  *) exit 1 ;;
  esac ;;
ssh) exit 255 ;;
esac
`

// buildOnlyDeployer returns a build-only deployer whose commands are all
// fakes, and the file the fakes log their calls to.
func buildOnlyDeployer(t *testing.T) (*deployer, string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(dir, "fake")
	if err := os.WriteFile(fake, []byte(fakeCommand), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"nix", "ssh", "sudo", "nixos-rebuild", "darwin-rebuild", "ready-check", "systemctl"} {
		if err := os.Symlink(fake, filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	calls := filepath.Join(dir, "calls")
	t.Setenv("FAKE_LOG", calls)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))

	inv, err := parseInventory([]byte(buildOnlyInventory))
	if err != nil {
		t.Fatal(err)
	}
	d := &deployer{
		inv:       inv,
		settings:  settings{sshCheckTimeout: "1", evalJobs: 4, buildProfile: profileLight, profileSource: "--light", runKeep: 30},
		ui:        newUI(nil),
		self:      "xyz",
		sshArgs:   []string{},
		preflight: true,
		buildOnly: true,
		failed:    loadFailedActivations(),
		jsonPath:  filepath.Join(dir, "report.json"),
	}
	d.record = startRunRecord(nil, dir, false, true, false, 30)
	t.Cleanup(d.releaseRoots)
	return d, calls
}

// changesToHosts returns the logged calls that could change a host or
// reach one other than to read its generation.
func changesToHosts(t *testing.T, calls string) []string {
	t.Helper()
	data, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	var changes []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		name, args, _ := strings.Cut(line, " ")
		switch {
		case name == "nix" && (strings.HasPrefix(args, "eval ") || strings.HasPrefix(args, "build ") || strings.HasPrefix(args, "path-info ")):
		case name == "ssh" && strings.Contains(args, "readlink"):
		default:
			changes = append(changes, line)
		}
	}
	return changes
}

func readReport(t *testing.T, path string) map[string]hostReport {
	t.Helper()
	var report runReport
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("report %s: %v", data, err)
	}
	hosts := map[string]hostReport{}
	for _, h := range report.Hosts {
		hosts[h.Host] = h
	}
	return hosts
}

func TestBuildOnlyStopsBeforeActivation(t *testing.T) {
	d, calls := buildOnlyDeployer(t)
	if status := d.deployHost("node", "full"); status != 0 {
		t.Fatalf("status %d", status)
	}
	if changes := changesToHosts(t, calls); len(changes) != 0 {
		t.Errorf("calls beyond evaluation and build: %q", changes)
	}
	node := readReport(t, d.jsonPath)["node"]
	if node.System.Status != "built" || node.HomeManager.Status != "built" {
		t.Errorf("node: %+v", node)
	}
	var info runInfo
	data, _ := os.ReadFile(filepath.Join(d.record.dir, "run.json"))
	if err := json.Unmarshal(data, &info); err != nil || !info.BuildOnly || info.DryRun || info.Failed == nil || *info.Failed {
		t.Errorf("run.json %s", data)
	}
}

func TestBuildOnlyFleet(t *testing.T) {
	d, calls := buildOnlyDeployer(t)
	// No sudo prompt, preflight, switch, serial check or ready gate.
	if status := d.deployAll("full"); status != 0 {
		t.Fatalf("status %d", status)
	}
	if changes := changesToHosts(t, calls); len(changes) != 0 {
		t.Errorf("calls beyond evaluation and build: %q", changes)
	}
	hosts := readReport(t, d.jsonPath)
	for _, host := range []string{"xyz", "node", "solo", "k1", "k2"} {
		if hosts[host].System.Status != "built" {
			t.Errorf("%s: %+v", host, hosts[host])
		}
	}
	if hosts["node"].HomeManager.Status != "built" || hosts["solo"].HomeManager.Status != "-" {
		t.Errorf("home-manager: %+v %+v", hosts["node"], hosts["solo"])
	}
}

func TestBuildOnlyFailedBuild(t *testing.T) {
	d, calls := buildOnlyDeployer(t)
	t.Setenv("FAKE_BUILD_FAIL", "k1")
	if status := d.deployAll("nixos"); status != 1 {
		t.Fatalf("status %d", status)
	}
	hosts := readReport(t, d.jsonPath)
	if hosts["k1"].System.Status != "failed" || hosts["k2"].System.Status != "built" {
		t.Errorf("k1 %+v, k2 %+v", hosts["k1"], hosts["k2"])
	}
	// The failed build's log lookups are this machine's; nothing reaches a host.
	for _, call := range changesToHosts(t, calls) {
		if !strings.HasPrefix(call, "nix ") {
			t.Errorf("call beyond evaluation and build: %q", call)
		}
	}
}

// One host's failed phase neither stops its other phase from building nor
// hides that phase's result.
func TestBuildOnlyHostReportsEachPhase(t *testing.T) {
	cases := []struct {
		variable, value string
		system, home    string
	}{
		{"FAKE_EVAL_FAIL", "nixosConfigurations", "failed", "built"},
		{"FAKE_BUILD_FAIL", "homeConfigurations", "built", "failed"},
	}
	for _, c := range cases {
		t.Run(c.variable, func(t *testing.T) {
			d, calls := buildOnlyDeployer(t)
			t.Setenv(c.variable, c.value)
			if status := d.deployHost("node", "full"); status != 1 {
				t.Fatalf("status %d", status)
			}
			node := readReport(t, d.jsonPath)["node"]
			if node.System.Status != c.system || node.HomeManager.Status != c.home {
				t.Errorf("node: %+v", node)
			}
			for _, call := range changesToHosts(t, calls) {
				if !strings.HasPrefix(call, "nix ") {
					t.Errorf("call beyond evaluation and build: %q", call)
				}
			}
		})
	}
}
