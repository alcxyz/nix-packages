package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestTriageWarningsSelectsFinishedRuns(t *testing.T) {
	runs := stateRuns(t)
	makeRun(t, runs, "20260102T030400.000000Z-1", true, &runReport{}, nil)
	want := makeRun(t, runs, "20260102T030500.000000Z-2", false, &runReport{}, nil)
	makeRun(t, runs, "20260102T030600.000000Z-3", false, nil, nil)
	held := makeRun(t, runs, "20260102T030700.000000Z-4", false, &runReport{}, nil)
	lock, err := os.OpenFile(filepath.Join(held, runLockName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if got, err := selectRun(runs, "", true); err != nil || got != want {
		t.Fatalf("selectRun = %q, %v; want %q", got, err, want)
	}
	if _, err := writeWarningsContext(held, nil, nil, ""); err == nil || !strings.Contains(err.Error(), "still going on") {
		t.Fatalf("a held run gave %v", err)
	}
}

func TestExtractWarnings(t *testing.T) {
	log := "\x1b[1;35mevaluation warning:\x1b[0m The option `services.old.enable' defined in `/nix/store/" + strings.Repeat("a", 32) + "-source/hosts/a.nix' has been renamed to `services.new.enable'.\n" +
		"evaluation warning: first line\n" +
		"                    second line\n" +
		"\n" +
		"                    after a blank\n" +
		"trace: warning: a trace message\n" +
		"\n" +
		"printed as it is\n" +
		"\n" +
		"{\"drv\":\"/nix/store/x.drv\",\"out\":\"/nix/store/x\"}\n"
	got := extractWarnings(log)
	if len(got) != 3 {
		t.Fatalf("got %d warnings: %+v", len(got), got)
	}
	if got[0].kind != "evaluation warning" || len(got[0].lines) != 1 || strings.Contains(got[0].lines[0], "\x1b") {
		t.Errorf("first warning %+v", got[0])
	}
	if want := "evaluation warning: first line\n                    second line\n\n                    after a blank"; strings.Join(got[1].lines, "\n") != want {
		t.Errorf("multi-line warning %q", got[1].lines)
	}
	if got[2].kind != "trace: warning" || strings.Join(got[2].lines, "\n") != "trace: warning: a trace message\n\nprinted as it is" {
		t.Errorf("trace warning %+v", got[2])
	}

	long := "evaluation warning: long\n" + strings.Repeat("  more\n", 60)
	if w := extractWarnings(long); len(w) != 1 || !w[0].cut || len(w[0].lines) != maxErrorLines {
		t.Errorf("a long warning is not cut: %d lines", len(w[0].lines))
	}

	// A configuration example with braces inside a trace message is kept
	// whole; only the evaluation's own result ends it.
	example := "trace: warning: set it like this:\n{\n  services.x = { enable = true; };\n}\n{\"drv\":\"/nix/store/x.drv\",\"out\":\"/nix/store/x\"}\n"
	if w := extractWarnings(example); len(w) != 1 || strings.Join(w[0].lines, "\n") != "trace: warning: set it like this:\n{\n  services.x = { enable = true; };\n}" {
		t.Errorf("a configuration example is not kept whole: %+v", w)
	}
}

func TestWarningsStopAtNixStatusLines(t *testing.T) {
	for _, line := range []string{
		"building '/nix/store/x.drv'...",
		"copying path '/nix/store/x' from 'https://cache.example.invalid'...",
		"copying '/src/flake' to the store",
		"downloading 'https://example.invalid/x.tar.gz'...",
		"fetching Git repository 'https://example.invalid/x'",
		"fetching the input 'github:example/x'",
		"fetching input 'self' due to self attribute",
		"fetching URL 'https://example.invalid/x': timeout. retrying",
		"fetching x from https://cache.example.invalid",
		"querying info about missing paths",
		"querying x on https://cache.example.invalid",
		"unpacking 'github:example/x' into the Git cache...",
		"these 3 derivations will be built:",
		"these 2 paths will be fetched (1.0 MiB download, 2.0 MiB unpacked):",
		"this derivation will be built:",
		"this path will be fetched (1.0 MiB download, 2.0 MiB unpacked):",
		"waiting for another Nix process to finish fetching input 'github:example/x'.",
		"waiting for lock on '/nix/store/x'",
		"waiting for exclusive access to the Nix store...",
		"waiting for auto-GC to finish",
		"waiting for the big Nix store lock...",
		"waiting for a machine to build '/nix/store/x.drv'",
		"waiting for a free build user ID for '/nix/store/x.drv'",
	} {
		w := extractWarnings("trace: warning: a message\n" + line + "\n  /nix/store/y.drv\n")
		if len(w) != 1 || len(w[0].lines) != 1 {
			t.Errorf("%q was absorbed: %+v", line, w)
		}
	}

	// The same warning between different noise is one warning.
	logs := t.TempDir()
	write := func(name, text string) {
		if err := os.WriteFile(filepath.Join(logs, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("system-alpha-evaluate.log", "trace: warning: old alias\nbuilding '/nix/store/a.drv'...\n")
	write("system-beta-evaluate.log", "trace: warning: old alias\nthese 2 derivations will be built:\n  /nix/store/b.drv\n")
	if got := collectWarnings(logs); len(got) != 1 || len(got[0].targets) != 2 {
		t.Errorf("collectWarnings = %+v", got)
	}
}

func TestOptionPaths(t *testing.T) {
	lines := []string{"The option `services.old.enable' defined in `/nix/store/x-source/a.nix' has been renamed to `services.new.enable`.", "Use `lib.mkForce` there. The options `programs.foo-bar.settings' and `default.nix' are obsolete."}
	got := optionPaths(lines)
	want := []string{"services.old.enable", "services.new.enable", "programs.foo-bar.settings"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("optionPaths = %q, want %q", got, want)
	}
	if got := optionPaths([]string{"evaluation warning: `services.foo.enable' is an obsolete option; remove it."}); len(got) != 1 || got[0] != "services.foo.enable" {
		t.Errorf("an option named before the word is missed: %q", got)
	}
	if got := optionPaths([]string{"trace: warning: pkgs.foo is deprecated, use `lib.hiPrio` instead"}); len(got) != 0 {
		t.Errorf("a function name is an option: %q", got)
	}
}

func TestFileInput(t *testing.T) {
	inputs, err := lockInputs(lockData(t, map[string]lockedInput{
		"mine":    {Type: "git", URL: "https://example.invalid/mine", Rev: "1111", NarHash: hashA},
		"nixpkgs": {Type: "github", Owner: "example", Repo: "pkgs", Rev: "2222", NarHash: hashB},
	}))
	if err != nil {
		t.Fatal(err)
	}
	own := map[string]bool{"mine": true}
	for file, want := range map[string]string{
		lockedStorePath(hashA, "source") + "/mod.nix:3":         "input `mine`, own",
		lockedStorePath(hashB, "source") + "/lib/a.nix":         "input `nixpkgs`, upstream",
		"«github:example/pkgs/2222»/lib/a.nix:1:2":              "input `nixpkgs`, upstream",
		"«git+https://example.invalid/mine?rev=1111»/a.nix:1:2": "input `mine`, own",
		"/nix/store/" + strings.Repeat("c", 32) + "-source":     "not a locked input of the run (the flake itself, a patched or generated source such as applyPatches or import-from-derivation output, or another path)",
	} {
		if got := fileInput(file, inputs, own); got != want {
			t.Errorf("fileInput(%q) = %q, want %q", file, got, want)
		}
	}
}

func TestFileInputSharedRevision(t *testing.T) {
	inputs, err := lockInputs(lockData(t, map[string]lockedInput{
		"fork":     {Type: "github", Owner: "me", Repo: "tool", Rev: "3333", NarHash: hashA},
		"upstream": {Type: "github", Owner: "example", Repo: "tool", Rev: "3333", NarHash: hashB},
	}))
	if err != nil {
		t.Fatal(err)
	}
	own := map[string]bool{"fork": true}
	for file, want := range map[string]string{
		"«github:me/tool/3333»/a.nix:1:2":      "input `fork`, own",
		"«github:example/tool/3333»/a.nix:1:2": "input `upstream`, upstream",
		"«github:other/tool/3333»/a.nix:1:2":   "ambiguous: the revision matches input `fork`, own; input `upstream`, upstream",
	} {
		if got := fileInput(file, inputs, own); got != want {
			t.Errorf("fileInput(%q) = %q, want %q", file, got, want)
		}
	}
}

func TestWarningCommand(t *testing.T) {
	attribute, trace := warningCommand(target{phase: "system", host: "alpha"}, "user-", "other", false)
	if attribute != `.#nixosConfigurations."alpha".config.system.build.toplevel` ||
		trace != `nix eval --no-eval-cache --option abort-on-warn true --show-trace --raw '.#nixosConfigurations."alpha".config.system.build.toplevel'` {
		t.Errorf("NixOS command: %q, %q", attribute, trace)
	}
	attribute, trace = warningCommand(target{phase: "hm", host: "alpha"}, "user-", "other", false)
	if attribute != `.#homeConfigurations."user-alpha".activationPackage` ||
		!strings.HasSuffix(trace, `'.#homeConfigurations."user-alpha".activationPackage'`) {
		t.Errorf("Home Manager command: %q, %q", attribute, trace)
	}
	if attribute, _ := warningCommand(target{phase: "system", host: "mac"}, "", "mac", true); attribute != `.#darwinConfigurations."mac".system` {
		t.Errorf("darwin attribute %q", attribute)
	}
}

func TestTriageWarningsContext(t *testing.T) {
	runs := stateRuns(t)
	renamed := "evaluation warning: The option `services.old.enable' defined in `" + lockedStorePath(hashA, "source") + "/mod.nix' has been renamed to `services.new.enable'.\n"
	dir := makeRun(t, runs, "20260102T030500.000000Z-4", false, &runReport{}, map[string]string{
		"system-beta-evaluate.log":  renamed + "{\"drv\":\"/nix/store/x.drv\",\"out\":\"/nix/store/x\"}\n",
		"hm-alpha-evaluate.log":     "trace: warning: an alias is deprecated\n{\"drv\":\"/nix/store/x.drv\",\"out\":\"/nix/store/x\"}\n",
		"system-alpha-evaluate.log": renamed + "evaluation warning:   The option   `services.old.enable' defined in `" + lockedStorePath(hashA, "source") + "/mod.nix' has been renamed to `services.new.enable'.\n{\"drv\":\"/nix/store/x.drv\",\"out\":\"/nix/store/x\"}\n",
		"system-gamma-activate.log": "evaluation warning: not an evaluation log\n",
	})
	setRun(t, dir, time.Date(2026, 1, 2, 3, 5, 0, 0, time.UTC), "/flake", "", lockData(t, map[string]lockedInput{
		"mine": {Type: "git", URL: "https://example.invalid/mine", Rev: "1111", NarHash: hashA},
	}))
	inv, err := parseInventory(encode(t, validFields()))
	if err != nil {
		t.Fatal(err)
	}
	path, err := writeWarningsContext(dir, map[string]string{"mine": t.TempDir()}, inv, "")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	text := string(data)
	for _, want := range []string{
		"2 distinct warnings",
		"### Warning 1 (evaluation warning)\n\nPrinted by: alpha system, beta system\n",
		"### Warning 2 (trace: warning)\n\nPrinted by: alpha home-manager\n",
		"Names the message calls options (heuristic): `services.old.enable`, `services.new.enable`",
		"hints from the wording",
		"/mod.nix: input `mine`, own",
		"### alpha system\n\n- Attribute: `.#nixosConfigurations.\"alpha\".config.system.build.toplevel`\n- Warnings: 1\n",
		"### alpha home-manager\n\n- Attribute: `.#homeConfigurations.\"alc-alpha\".activationPackage`\n- Warnings: 2\n",
		"## Own inputs",
		"| mine | yes |",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("context.md lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "## Lock diff") || strings.Contains(text, "definitionsWithLocations") || strings.Contains(text, "not an evaluation log") {
		t.Errorf("unexpected content:\n%s", text)
	}
}

func TestTriageWarningsPrompt(t *testing.T) {
	prompt := triagePrompt("/r/context.md", "/s/playbook.md", "", true)
	for _, want := range []string{"warning triage", "/r/context.md", "Warnings section", "limits apply in every mode", "/s/playbook.md", "activation, push, merge or reboot"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt %q lacks %q", prompt, want)
		}
	}
	if strings.Contains(triagePrompt("/c", "/p", "", false), "warning") {
		t.Error("the failure prompt mentions warnings")
	}
}

func TestTriageWarningsInventoryNotes(t *testing.T) {
	runs := stateRuns(t)
	dir := makeRun(t, runs, "20260102T030500.000000Z-4", false, &runReport{}, map[string]string{
		"hm-alpha-evaluate.log": "trace: warning: old alias in /nix/store/" + strings.Repeat("a", 32) + "-source/a.nix\n",
	})
	broken := filepath.Join(t.TempDir(), "inventory.json")
	if err := os.WriteFile(broken, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ env, want string }{
		{"", "No inventory is configured (--config FILE or NIX_DEPLOY_CONFIG), so Home Manager attributes show"},
		{broken, "The inventory could not be read or is invalid, so Home Manager attributes show"},
	} {
		t.Setenv("NIX_DEPLOY_CONFIG", c.env)
		if status := runTriage([]string{"--warnings", "--no-launch"}); status != 0 {
			t.Fatalf("runTriage exit status %d", status)
		}
		data, _ := os.ReadFile(filepath.Join(dir, "context.md"))
		// The run kept no flake.lock, so files cannot be mapped.
		if !strings.Contains(string(data), c.want) || !strings.Contains(string(data), "-source/a.nix: not mapped, since the run has no readable flake.lock") {
			t.Errorf("context.md lacks %q:\n%s", c.want, data)
		}
	}
}

func TestTriageWarningsFlakeCheckout(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	repo := t.TempDir()
	git := func(args ...string) string {
		output, err := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=test", "-c", "user.email=test@example.invalid"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	lock := lockData(t, map[string]lockedInput{})
	if err := os.WriteFile(filepath.Join(repo, "flake.lock"), lock, 0o600); err != nil {
		t.Fatal(err)
	}
	git("init", "-q")
	git("add", "-A")
	git("commit", "-q", "-m", "a")
	head := git("rev-parse", "HEAD")
	clean := false
	info := runInfo{FlakeDir: repo, GitRev: head, GitDirty: &clean, LockSHA256: lockSum(lock)}
	var out strings.Builder
	writeFlakeCheckout(&out, info)
	if want := "- Now: " + head + ", tracked files clean; flake.lock same as the run's\n- Match: yes"; !strings.Contains(out.String(), want) {
		t.Errorf("a matching checkout gave:\n%s", out.String())
	}
	if err := os.WriteFile(filepath.Join(repo, "flake.lock"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	writeFlakeCheckout(&out, info)
	if want := "tracked files differ; flake.lock differs from the run's\n- Match: no; work from a worktree at the run's revision"; !strings.Contains(out.String(), want) {
		t.Errorf("a changed checkout gave:\n%s", out.String())
	}
}

func TestTriageWarningsDarwinWithoutInventory(t *testing.T) {
	saved := triageDarwin
	triageDarwin = true
	defer func() { triageDarwin = saved }()
	runs := stateRuns(t)
	dir := makeRun(t, runs, "20260102T030500.000000Z-4", false, &runReport{}, map[string]string{
		"system-alpha-evaluate.log": "trace: warning: old alias\n",
	})
	path, err := writeWarningsContext(dir, nil, nil, "No inventory is configured")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	for _, want := range []string{
		"No inventory is configured, so Home Manager attributes show `<homeOutputPrefix>` for its prefix, and host aliases are not resolved, so a system target that is this Mac uses `darwinConfigurations`",
		"- Attribute: `.#nixosConfigurations.\"alpha\".config.system.build.toplevel`\n- On the operator's Mac, if this host is that Mac: `.#darwinConfigurations.\"alpha\".system`\n",
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("context.md lacks %q:\n%s", want, data)
		}
	}
	inv, err := parseInventory(encode(t, validFields()))
	if err != nil {
		t.Fatal(err)
	}
	path, _ = writeWarningsContext(dir, nil, inv, "")
	if data, _ := os.ReadFile(path); strings.Contains(string(data), "On the operator's Mac") {
		t.Errorf("an inventory still gave both attributes:\n%s", data)
	}
}

// Flake references as Nix 2.35 prints them in trace frames: forge inputs
// as <type>:<owner>/<repo>/<rev> with a non-default host and the narHash
// in the query, Git and path inputs as their URL with a query.
func TestFileInputFlakeReferenceForms(t *testing.T) {
	rev := strings.Repeat("ab", 20)
	inputs, err := lockInputs(lockData(t, map[string]lockedInput{
		"tool":   {Type: "github", Owner: "example", Repo: "tool", Rev: rev},
		"extra":  {Type: "github", Owner: "example", Repo: "tool-extra", Rev: strings.Repeat("cd", 20)},
		"lab":    {Type: "gitlab", Owner: "group", Repo: "lab", Host: "gitlab.example.invalid", Rev: strings.Repeat("ef", 20)},
		"hut":    {Type: "sourcehut", Owner: "~user", Repo: "hut", Rev: strings.Repeat("12", 20)},
		"git":    {Type: "git", URL: "https://example.invalid/repo.git?ref=refs/heads/main", Rev: strings.Repeat("34", 20)},
		"local":  {Type: "git", URL: "file:///src/local", Rev: strings.Repeat("56", 20)},
		"dir":    {Type: "path", Path: "/src/dir"},
		"sub":    {Type: "gitlab", Owner: "group/subgroup", Repo: "sub", Rev: strings.Repeat("78", 20)},
		"native": {Type: "git", URL: "git://example.invalid/native.git", Rev: strings.Repeat("9a", 20)},
	}))
	if err != nil {
		t.Fatal(err)
	}
	own := map[string]bool{"tool": true}
	for file, want := range map[string]string{
		"«github:example/tool/" + rev + "?narHash=sha256-x%3D»/a.nix:1:2":                                           "input `tool`, own",
		"«github:example/tool-extra/" + rev + "»/a.nix:1:2":                                                         "input `tool`, own (by revision only; the source differs from the lock's)",
		"«gitlab:group/lab/" + strings.Repeat("ef", 20) + "?host=gitlab.example.invalid»/a.nix:1:2":                 "input `lab`, upstream",
		"«gitlab:group/lab/" + strings.Repeat("ef", 20) + "»/a.nix:1:2":                                             "input `lab`, upstream (by revision only; the source differs from the lock's)",
		"«sourcehut:~user/hut/" + strings.Repeat("12", 20) + "»/a.nix:1:2":                                          "input `hut`, upstream",
		"«git+https://example.invalid/repo.git?ref=refs/heads/main&rev=" + strings.Repeat("34", 20) + "»/a.nix:1:2": "input `git`, upstream",
		"«git+file:///src/local?ref=refs/heads/main&rev=" + strings.Repeat("56", 20) + "&shallow=1»/a.nix:1:2":      "input `local`, upstream",
		"«gitlab:group%2Fsubgroup/sub/" + strings.Repeat("78", 20) + "»/a.nix:1:2":                                  "input `sub`, upstream",
		"«git://example.invalid/native.git?ref=main&rev=" + strings.Repeat("9a", 20) + "»/a.nix:1:2":                "input `native`, upstream",
		"«path:/src/dir?lastModified=1&narHash=sha256-x%3D»/a.nix:1:2":                                              "input `dir`, upstream",
	} {
		if got := fileInput(file, inputs, own); got != want {
			t.Errorf("fileInput(%q) = %q, want %q", file, got, want)
		}
	}
	if ref := parseFlakeRef("gitlab:group/lab/" + rev + "?host=gitlab.example.invalid"); ref.typ != "gitlab" || ref.owner != "group" || ref.repo != "lab" || ref.rev != rev || ref.host != "gitlab.example.invalid" {
		t.Errorf("parseFlakeRef = %+v", ref)
	}
}
