package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const referenceScript = "testdata/reference/modules/home-manager/workspace/workspace-sync.sh"

type parityFixture struct {
	base, root, manifest, bin, git, cwd string
	env                                 []string
}

type parityResult struct {
	status         int
	stdout, stderr string
	state          []string
}

func fixtureCommand(t *testing.T, f *parityFixture, command string, args ...string) {
	t.Helper()
	c := exec.Command(command, args...)
	c.Env = f.env
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("fixture command %s: %v\n%s", command, err, out)
	}
}

func fixtureWrite(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func fixtureShellScript(t *testing.T, path, content string) {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	fixtureWrite(t, path, "#!"+bash+"\n"+strings.TrimPrefix(content, "#!/usr/bin/env bash\n"), 0755)
}

func fixtureLink(t *testing.T, target, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
}

func newParityFixture(t *testing.T) *parityFixture {
	t.Helper()
	base := t.TempDir()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	f := &parityFixture{base: base, root: filepath.Join(base, "root"), manifest: filepath.Join(base, "manifest.json"), bin: filepath.Join(base, "bin"), git: git}
	for _, item := range os.Environ() {
		name, _, _ := strings.Cut(item, "=")
		if name == "HOME" || name == "PATH" || strings.HasPrefix(name, "GIT_") || name == "BASH_ENV" || name == "ENV" {
			continue
		}
		f.env = append(f.env, item)
	}
	f.env = append(f.env, "HOME="+filepath.Join(base, "home"), "PATH="+f.bin+string(os.PathListSeparator)+os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_ALLOW_PROTOCOL=file", "LC_ALL=C")
	fixtureWrite(t, f.manifest, `{"directories":[],"repositories":[],"links":[]}`, 0644)
	return f
}

func (f *parityFixture) manifestJSON(t *testing.T, directories []string, repositories, links []map[string]interface{}) {
	t.Helper()
	if directories == nil {
		directories = []string{}
	}
	if repositories == nil {
		repositories = []map[string]interface{}{}
	}
	if links == nil {
		links = []map[string]interface{}{}
	}
	b, err := json.Marshal(map[string]interface{}{"directories": directories, "repositories": repositories, "links": links})
	if err != nil {
		t.Fatal(err)
	}
	fixtureWrite(t, f.manifest, string(b), 0644)
}

func repositoryEntry(path, url string) map[string]interface{} {
	return map[string]interface{}{"path": path, "url": url}
}
func linkEntry(path, target string) map[string]interface{} {
	return map[string]interface{}{"path": path, "target": target, "profiles": []string{"fixture"}}
}

func (f *parityFixture) source(t *testing.T) string {
	t.Helper()
	source := filepath.Join(f.base, "source with spaces")
	fixtureCommand(t, f, f.git, "init", "-q", "-b", "main", source)
	fixtureWrite(t, filepath.Join(source, "tracked"), "original\n", 0644)
	fixtureCommand(t, f, f.git, "-C", source, "add", "tracked")
	fixtureCommand(t, f, f.git, "-C", source, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "initial")
	return source
}

// Diagnostics owned by Git, jq, Bash and coreutils vary between tool versions.
// Only these exact workspace-sync prefixes belong to the executable contract.
func authoredStderr(stderr string) string {
	var result strings.Builder
	for _, line := range strings.SplitAfter(stderr, "\n") {
		for _, prefix := range []string{"workspace-sync:", "workspace-sync failed repositories:", "conflict:", "failed:", "  - "} {
			if strings.HasPrefix(line, prefix) {
				result.WriteString(line)
				break
			}
		}
	}
	return result.String()
}

func snapshot(t *testing.T, f *parityFixture) []string {
	t.Helper()
	var state []string
	err := filepath.WalkDir(f.base, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(f.base, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if entry.IsDir() && (entry.Name() == ".git" || rel == "bin" || rel == "home") {
			return filepath.SkipDir
		}
		if rel == "manifest.json" {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			state = append(state, rel+" -> "+strings.ReplaceAll(target, f.base, "<fixture>"))
			return nil
		}
		if entry.IsDir() {
			state = append(state, rel+"/")
			return nil
		}
		if entry.Name() == ".git" {
			state = append(state, rel+": git worktree")
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		state = append(state, rel+": "+strings.ReplaceAll(string(content), f.base, "<fixture>"))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(state)
	return state
}

func runParity(t *testing.T, f *parityFixture, binary string, args []string, options ...parityCase) parityResult {
	t.Helper()
	var command *exec.Cmd
	if binary == "" {
		reference, err := filepath.Abs(referenceScript)
		if err != nil {
			t.Fatal(err)
		}
		command = exec.Command("bash", append([]string{reference}, args...)...)
	} else {
		command = exec.Command(binary, args...)
	}
	command.Env = f.env
	command.Dir = f.cwd
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if len(options) > 0 {
		option := options[0]
		if option.stdin != "" {
			command.Stdin = strings.NewReader(option.stdin)
		}
		if option.badStdout {
			fixtureWrite(t, filepath.Join(f.base, "readonly-stdout"), "", 0644)
			file, err := os.Open(filepath.Join(f.base, "readonly-stdout"))
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			command.Stdout = file
		}
	}
	status := 0
	if err := command.Run(); err != nil {
		if e, ok := err.(*exec.ExitError); ok {
			status = e.ExitCode()
		} else {
			t.Fatal(err)
		}
	}
	return parityResult{status: status, stdout: strings.ReplaceAll(stdout.String(), f.base, "<fixture>"), stderr: strings.ReplaceAll(authoredStderr(stderr.String()), f.base, "<fixture>"), state: snapshot(t, f)}
}

func candidateBinary(t *testing.T) string {
	t.Helper()
	if binary := os.Getenv("WORKSPACE_SYNC_TEST_BINARY"); binary != "" {
		absolute, err := filepath.Abs(binary)
		if err != nil {
			t.Fatal(err)
		}
		return absolute
	}
	binary := filepath.Join(t.TempDir(), "workspace-sync")
	c := exec.Command("go", "build", "-o", binary, ".")
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("build candidate: %v\n%s", err, out)
	}
	return binary
}

type parityCase struct {
	name          string
	setup         func(*testing.T, *parityFixture)
	arguments     func(*parityFixture) []string
	repeat        bool
	expectedFatal int
	stdin         string
	badStdout     bool
}

func TestShellParity(t *testing.T) {
	binary := candidateBinary(t)
	defaultArgs := func(f *parityFixture) []string { return []string{f.root, f.manifest} }
	cases := []parityCase{
		{name: "manifest-file-missing", setup: func(t *testing.T, f *parityFixture) {
			if err := os.Remove(f.manifest); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "manifest-is-directory", arguments: func(f *parityFixture) []string { return []string{f.root, f.base} }},
		{name: "missing-all-arguments", arguments: func(f *parityFixture) []string { return nil }},
		{name: "missing-manifest", arguments: func(f *parityFixture) []string { return []string{f.root} }},
		{name: "empty-mode-trailing-ignored", arguments: func(f *parityFixture) []string { return []string{f.root, f.manifest, "", "ignored"} }},
		{name: "status-trailing-ignored", arguments: func(f *parityFixture) []string { return []string{f.root, f.manifest, "--status", "ignored"} }},
		{name: "unknown-mode", arguments: func(f *parityFixture) []string { return []string{f.root, f.manifest, "no-such-mode"} }},
		{name: "help-invalid-manifest", setup: func(t *testing.T, f *parityFixture) { fixtureWrite(t, f.manifest, "invalid", 0644) }, arguments: func(f *parityFixture) []string { return []string{f.root, f.manifest, "--help"} }},
		{name: "short-help", arguments: func(f *parityFixture) []string { return []string{f.root, f.manifest, "-h"} }},
		{name: "fresh-status", setup: func(t *testing.T, f *parityFixture) {
			f.manifestJSON(t, []string{"nested/directory"}, []map[string]interface{}{repositoryEntry("nested/repo", filepath.Join(f.base, "nonexistent"))}, []map[string]interface{}{linkEntry("links/pending", "nested/repo")})
		}, arguments: func(f *parityFixture) []string { return []string{f.root, f.manifest, "--status"} }},
		{name: "ordered-partial-clones-and-links", repeat: true, setup: func(t *testing.T, f *parityFixture) {
			source := f.source(t)
			bad := repositoryEntry("apps/bad", source)
			bad["branch"] = "missing-branch"
			good := repositoryEntry("apps/good", source)
			good["branch"] = "main"
			f.manifestJSON(t, []string{"scratch"}, []map[string]interface{}{bad, good, repositoryEntry("apps/failure", filepath.Join(f.base, "nonexistent"))}, []map[string]interface{}{linkEntry("links/current", "apps/good"), linkEntry("links/pending", "apps/missing")})
		}},
		{name: "dirty-checkout-worktree-ordinary", setup: func(t *testing.T, f *parityFixture) {
			source := f.source(t)
			fixtureCommand(t, f, f.git, "clone", "-q", source, filepath.Join(f.root, "checkout"))
			fixtureWrite(t, filepath.Join(f.root, "checkout/tracked"), "local edit\n", 0644)
			fixtureWrite(t, filepath.Join(f.root, "checkout/ordinary/file"), "keep\n", 0644)
			fixtureCommand(t, f, f.git, "-C", source, "worktree", "add", "-q", "--detach", filepath.Join(f.root, "worktree"))
			fixtureWrite(t, filepath.Join(f.root, "worktree/tracked"), "worktree edit\n", 0644)
			fixtureWrite(t, filepath.Join(f.root, "file"), "keep file\n", 0644)
			f.manifestJSON(t, nil, []map[string]interface{}{repositoryEntry("checkout", source), repositoryEntry("worktree", source), repositoryEntry("checkout/ordinary", source), repositoryEntry("file", source)}, nil)
		}},
		{name: "links-conflicts-escape-and-idempotence", repeat: true, setup: func(t *testing.T, f *parityFixture) {
			fixtureWrite(t, filepath.Join(f.root, "canonical/file"), "keep\n", 0644)
			fixtureWrite(t, filepath.Join(f.base, "outside/file"), "outside\n", 0644)
			fixtureLink(t, filepath.Join(f.base, "outside"), filepath.Join(f.root, "escape"))
			fixtureLink(t, "wrong", filepath.Join(f.root, "links/wrong"))
			fixtureLink(t, "../canonical", filepath.Join(f.root, "links/existing"))
			fixtureWrite(t, filepath.Join(f.root, "links/file"), "keep\n", 0644)
			f.manifestJSON(t, nil, nil, []map[string]interface{}{linkEntry("links/new", "canonical"), linkEntry("links/existing", "canonical"), linkEntry("links/wrong", "canonical"), linkEntry("links/file", "canonical"), linkEntry("links/escape", "escape"), linkEntry("links/missing", "missing")})
		}},
		{name: "root-file", setup: func(t *testing.T, f *parityFixture) { fixtureWrite(t, f.root, "keep\n", 0644) }},
		{name: "root-symlink", setup: func(t *testing.T, f *parityFixture) {
			fixtureWrite(t, filepath.Join(f.base, "outside/file"), "keep\n", 0644)
			fixtureLink(t, filepath.Join(f.base, "outside"), f.root)
		}},
		{name: "root-ancestor-symlink", repeat: true, setup: func(t *testing.T, f *parityFixture) {
			if err := os.MkdirAll(filepath.Join(f.base, "actual"), 0755); err != nil {
				t.Fatal(err)
			}
			fixtureLink(t, filepath.Join(f.base, "actual"), filepath.Join(f.base, "alias"))
			f.root = filepath.Join(f.base, "alias/root")
			fixtureWrite(t, filepath.Join(f.root, "canonical/file"), "keep\n", 0644)
			fixtureLink(t, ".", filepath.Join(f.root, "root-target"))
			f.manifestJSON(t, []string{"directory"}, nil, []map[string]interface{}{linkEntry("links/canonical", "canonical"), linkEntry("links/root", "root-target")})
		}},
		{name: "symlink-parent", setup: func(t *testing.T, f *parityFixture) {
			fixtureWrite(t, filepath.Join(f.base, "outside/file"), "keep\n", 0644)
			fixtureLink(t, filepath.Join(f.base, "outside"), filepath.Join(f.root, "parent"))
			f.manifestJSON(t, []string{"parent/nested", "later"}, []map[string]interface{}{repositoryEntry("parent/repo", "local")}, []map[string]interface{}{linkEntry("parent/link", "later")})
		}},
		{name: "directory-conflict-continues", setup: func(t *testing.T, f *parityFixture) {
			f.root = filepath.Join(f.base, "nested/child/root")
			fixtureWrite(t, filepath.Join(f.root, "blocked/file"), "keep\n", 0644)
			f.manifestJSON(t, []string{"blocked/file/deeper", "later/directory"}, []map[string]interface{}{repositoryEntry("never-cloned", "local")}, []map[string]interface{}{linkEntry("never-linked", "later")})
		}, arguments: func(f *parityFixture) []string { return []string{f.root, f.manifest, "--directories"} }},
		{name: "directory-conflict-sync-success", setup: func(t *testing.T, f *parityFixture) {
			fixtureWrite(t, filepath.Join(f.root, "blocked"), "keep\n", 0644)
			f.manifestJSON(t, []string{"blocked/deeper", "later"}, nil, nil)
		}},
	}
	for _, raw := range []struct{ name, manifest string }{
		{"malformed", "{"}, {"null", "null"}, {"array", "[]"}, {"missing-arrays", "{}"},
		{"null-directories", `{"directories":null,"repositories":[],"links":[]}`},
		{"wrong-directory-type", `{"directories":[1],"repositories":[],"links":[]}`},
		{"unsafe-traversal", `{"directories":["../escape"],"repositories":[],"links":[]}`},
		{"unsafe-absolute", `{"directories":[],"repositories":[{"path":"/escape","url":"local"}],"links":[]}`},
		{"unsafe-empty-component", `{"directories":["a//b"],"repositories":[],"links":[]}`},
		{"numeric-branch", `{"directories":[],"repositories":[{"path":"repo","url":"local","branch":2}],"links":[]}`},
		{"numeric-url", `{"directories":[],"repositories":[{"path":"repo","url":2}],"links":[]}`},
		{"null-url", `{"directories":[],"repositories":[{"path":"repo","url":null}],"links":[]}`},
		{"null-profiles", `{"directories":[],"repositories":[],"links":[{"path":"link","target":"target","profiles":null}]}`},
		{"numeric-profile", `{"directories":[],"repositories":[],"links":[{"path":"link","target":"target","profiles":[1]}]}`},
		{"extra-fields-null-branch", `{"directories":[],"repositories":[{"path":"repo","url":"local","branch":null,"extra":true}],"links":[],"extra":17}`},
		{"multiple-documents", `{"directories":["first"],"repositories":[],"links":[]} ` + "\n" + `{"directories":["second"],"repositories":[],"links":[]}`},
		{"valid-first-invalid-last", `{"directories":[],"repositories":[],"links":[]} ` + "\nnull"},
		{"terminal-newline-path", `{"directories":["folder\n"],"repositories":[],"links":[]}`},
		{"double-terminal-newline-path", `{"directories":["folder\n\n"],"repositories":[],"links":[]}`},
		{"nul-and-newline-url", `{"directories":[],"repositories":[{"path":"repo","url":"lo\u0000cal\n\n","branch":"main\n\n"}],"links":[]}`},
	} {
		manifest := raw.manifest
		cases = append(cases, parityCase{name: "schema-" + raw.name, setup: func(t *testing.T, f *parityFixture) { fixtureWrite(t, f.manifest, manifest, 0644) }, arguments: func(f *parityFixture) []string { return []string{f.root, f.manifest, "--status"} }})
	}
	for _, command := range []string{"realpath", "ln"} {
		fatalCommand := command
		cases = append(cases, parityCase{name: "fatal-" + command, setup: func(t *testing.T, f *parityFixture) {
			fixtureWrite(t, filepath.Join(f.root, "canonical/file"), "keep\n", 0644)
			fixtureShellScript(t, filepath.Join(f.bin, fatalCommand), "exit 37\n")
			f.manifestJSON(t, nil, nil, []map[string]interface{}{linkEntry("links/first", "canonical"), linkEntry("links/later", "canonical")})
		}})
	}
	for _, mode := range []string{"", "--status", "--directories"} {
		selectedMode := mode
		cases = append(cases, parityCase{name: "git-argument-boundaries-" + mode, setup: func(t *testing.T, f *parityFixture) {
			// Record argument boundaries, including empty arguments and embedded
			// newlines. A fake transport avoids interpreting fixture URLs at all.
			fixtureShellScript(t, filepath.Join(f.bin, "git"), "printf '%s\\0' \"$@\" >> \"$HOME/../git-arguments\"\ncase \"$*\" in *failure*) exit 41;; esac\nexit 0\n")
			if err := os.MkdirAll(filepath.Join(f.base, "home"), 0755); err != nil {
				t.Fatal(err)
			}
			first := repositoryEntry("first", "a URL 'quoted' \"double\" $(untouched); failure\n\n")
			first["branch"] = "branch with spaces\n"
			second := repositoryEntry("later", "second\nURL\u0000trimmed\n\n")
			f.manifestJSON(t, []string{"directory"}, []map[string]interface{}{first, second}, nil)
		}, arguments: func(f *parityFixture) []string {
			if selectedMode == "" {
				return []string{f.root, f.manifest}
			}
			return []string{f.root, f.manifest, selectedMode}
		}})
	}
	cases = append(cases, parityCase{name: "fatal-realpath-status", setup: func(t *testing.T, f *parityFixture) {
		fixtureWrite(t, filepath.Join(f.root, "canonical/file"), "keep\n", 0644)
		fixtureShellScript(t, filepath.Join(f.bin, "realpath"), "exit 37\n")
		f.manifestJSON(t, nil, nil, []map[string]interface{}{linkEntry("links/first", "canonical"), linkEntry("links/later", "canonical")})
	}, arguments: func(f *parityFixture) []string { return []string{f.root, f.manifest, "--status"} }})
	for _, stage := range []string{"root", "target"} {
		selectedStage := stage
		cases = append(cases, parityCase{name: "fatal-realpath-resolved-" + stage, setup: func(t *testing.T, f *parityFixture) {
			realpath, err := exec.LookPath("realpath")
			if err != nil {
				t.Fatal(err)
			}
			fixtureWrite(t, filepath.Join(f.root, "canonical/file"), "keep\n", 0644)
			// Selection is based on the -e operand, without quoting fixture paths
			// into executable script text.
			operand := f.root
			if selectedStage == "target" {
				operand = filepath.Join(f.root, "canonical")
			}
			f.env = append(f.env, "FIXTURE_REALPATH="+realpath, "FIXTURE_FAIL_OPERAND="+operand)
			fixtureShellScript(t, filepath.Join(f.bin, "realpath"), "if [[ $1 == -e && $3 == \"$FIXTURE_FAIL_OPERAND\" ]]; then exit 37; fi\nexec \"$FIXTURE_REALPATH\" \"$@\"\n")
			f.manifestJSON(t, nil, nil, []map[string]interface{}{linkEntry("links/first", "canonical"), linkEntry("links/later", "canonical")})
		}})
	}
	for _, failure := range []struct {
		name, script string
		status       int
	}{
		{name: "missing-interpreter", script: "#!/fixture-nonexistent-interpreter\n", status: -1},
		{name: "sigterm", script: "kill -TERM $$\n", status: 143},
	} {
		selectedFailure := failure
		cases = append(cases, parityCase{name: "fatal-realpath-" + failure.name, expectedFatal: failure.status, setup: func(t *testing.T, f *parityFixture) {
			fixtureWrite(t, filepath.Join(f.root, "canonical/file"), "keep\n", 0644)
			if selectedFailure.name == "missing-interpreter" {
				fixtureWrite(t, filepath.Join(f.bin, "realpath"), selectedFailure.script, 0755)
			} else {
				fixtureShellScript(t, filepath.Join(f.bin, "realpath"), selectedFailure.script)
			}
			f.manifestJSON(t, nil, nil, []map[string]interface{}{linkEntry("links/first", "canonical"), linkEntry("links/later", "canonical")})
		}})
	}
	cases = append(cases, parityCase{name: "fatal-realpath-missing-command", expectedFatal: 127, setup: func(t *testing.T, f *parityFixture) {
		fixtureWrite(t, filepath.Join(f.root, "canonical/file"), "keep\n", 0644)
		for _, name := range []string{"jq", "dirname", "mkdir"} {
			path, err := exec.LookPath(name)
			if err != nil {
				t.Fatal(err)
			}
			fixtureLink(t, path, filepath.Join(f.bin, name))
		}
		for i, item := range f.env {
			if strings.HasPrefix(item, "PATH=") {
				f.env[i] = "PATH=" + f.bin
			}
		}
		f.manifestJSON(t, nil, nil, []map[string]interface{}{linkEntry("links/first", "canonical"), linkEntry("links/later", "canonical")})
	}})
	for _, mode := range []string{"", "--status", "--directories"} {
		selectedMode := mode
		cases = append(cases, parityCase{name: "directory-name-too-long-" + mode, setup: func(t *testing.T, f *parityFixture) {
			f.env = append(f.env, "FIXTURE_GIT_LOG="+filepath.Join(f.base, "git-arguments"))
			fixtureShellScript(t, filepath.Join(f.bin, "git"), "echo clone-attempted > \"$FIXTURE_GIT_LOG\"\nmkdir -p -- \"${@: -1}\"\nexit 0\n")
			f.manifestJSON(t, []string{strings.Repeat("a", 256), "later/directory"}, []map[string]interface{}{repositoryEntry("later/repository", "local")}, nil)
		}, arguments: func(f *parityFixture) []string {
			if selectedMode == "" {
				return []string{f.root, f.manifest}
			}
			return []string{f.root, f.manifest, selectedMode}
		}})
	}
	cases = append(cases, parityCase{name: "stdout-write-failure-help", badStdout: true, arguments: func(f *parityFixture) []string { return []string{f.root, f.manifest, "--help"} }})
	for _, mode := range []string{"", "--status"} {
		selectedMode := mode
		cases = append(cases, parityCase{name: "stdout-write-failure-report-" + mode, badStdout: true, setup: func(t *testing.T, f *parityFixture) {
			f.env = append(f.env, "FIXTURE_GIT_LOG="+filepath.Join(f.base, "later-attempted"))
			fixtureShellScript(t, filepath.Join(f.bin, "git"), "case \"$*\" in *later*) echo later > \"$FIXTURE_GIT_LOG\";; esac\nexit 0\n")
			fixtureWrite(t, filepath.Join(f.root, "later/.git"), "fixture", 0644)
			if selectedMode == "" {
				fixtureWrite(t, filepath.Join(f.root, "first/.git"), "fixture", 0644)
			}
			f.manifestJSON(t, nil, []map[string]interface{}{repositoryEntry("first", "local"), repositoryEntry("later", "local")}, nil)
		}, arguments: func(f *parityFixture) []string { return []string{f.root, f.manifest, selectedMode} }})
	}
	cases = append(cases, parityCase{name: "shebangless-git-quoting-partial-failure", setup: func(t *testing.T, f *parityFixture) {
		f.env = append(f.env, "FIXTURE_GIT_LOG="+filepath.Join(f.base, "git-arguments"))
		fixtureWrite(t, filepath.Join(f.bin, "git"), "printf '%s\\0' \"$@\" >> \"$FIXTURE_GIT_LOG\"\nurl=$2\nif [ \"$2\" = --branch ]; then url=$4; fi\ncase \"$url\" in *failure*) exit 41;; esac\nmkdir -p -- \"${@: -1}\"\nexit 0\n", 0755)
		first := repositoryEntry("first", "quoted URL 'single' \"double\" $(untouched); failure")
		first["branch"] = "branch with spaces"
		f.manifestJSON(t, nil, []map[string]interface{}{first, repositoryEntry("later", "URL with spaces")}, nil)
	}})
	cases = append(cases, parityCase{name: "shebangless-git-relative-path", setup: func(t *testing.T, f *parityFixture) {
		bash, err := exec.LookPath("bash")
		if err != nil {
			t.Fatal(err)
		}
		fixtureLink(t, bash, filepath.Join(f.bin, "bash"))
		fixtureWrite(t, filepath.Join(f.bin, "git"), "mkdir -p -- \"${@: -1}\"\n", 0755)
		f.cwd = f.base
		for i, item := range f.env {
			if strings.HasPrefix(item, "PATH="+f.bin) {
				f.env[i] = "PATH=./bin" + strings.TrimPrefix(item, "PATH="+f.bin)
			}
		}
		f.manifestJSON(t, nil, []map[string]interface{}{repositoryEntry("repo", "synthetic URL")}, nil)
	}})
	for _, command := range []string{"realpath", "ln"} {
		selectedCommand := command
		cases = append(cases, parityCase{name: "fatal-shebangless-" + command, setup: func(t *testing.T, f *parityFixture) {
			fixtureWrite(t, filepath.Join(f.root, "canonical/file"), "keep\n", 0644)
			fixtureWrite(t, filepath.Join(f.bin, selectedCommand), "exit 37\n", 0755)
			f.manifestJSON(t, nil, nil, []map[string]interface{}{linkEntry("links/first", "canonical"), linkEntry("links/later", "canonical")})
		}})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var results [2]parityResult
			for i, executable := range []string{"", binary} {
				f := newParityFixture(t)
				if tc.setup != nil {
					tc.setup(t, f)
				}
				args := defaultArgs(f)
				if tc.arguments != nil {
					args = tc.arguments(f)
				}
				results[i] = runParity(t, f, executable, args, tc)
				if tc.name == "shebangless-git-quoting-partial-failure" {
					if results[i].status != 1 || !strings.Contains(results[i].stdout, "0 existing, 1 cloned, 0 skipped, 1 failed") || !directory(filepath.Join(f.root, "later")) {
						t.Errorf("fallback must fail once then clone successfully: %#v", results[i])
					}
				}
				if tc.name == "shebangless-git-relative-path" {
					if results[i].status != 0 || !strings.Contains(results[i].stdout, "0 existing, 1 cloned, 0 skipped, 0 failed") {
						t.Errorf("relative PATH fallback failed: %#v", results[i])
					}
				}
				if strings.HasPrefix(tc.name, "directory-name-too-long-") {
					_, gitErr := os.Stat(filepath.Join(f.base, "git-arguments"))
					if tc.name == "directory-name-too-long-" {
						if gitErr != nil {
							t.Errorf("sync failed to attempt later repository: %v", gitErr)
						}
					} else if !os.IsNotExist(gitErr) {
						t.Errorf("status/directory mode invoked Git: %v", gitErr)
					}
					_, err := os.Stat(filepath.Join(f.root, "later/directory"))
					if tc.name == "directory-name-too-long---status" {
						if !os.IsNotExist(err) {
							t.Errorf("status created a later directory: %v", err)
						}
					} else if err != nil {
						t.Errorf("directory failure prevented later directory creation: %v", err)
					}
				}
				if tc.badStdout {
					if results[i].status != 1 {
						t.Errorf("failed stdout write must exit 1: %#v", results[i])
					}
					if _, err := os.Stat(filepath.Join(f.base, "later-attempted")); !os.IsNotExist(err) {
						t.Errorf("failed report write attempted later entry: %v", err)
					}
				}
				if strings.HasPrefix(tc.name, "git-argument-boundaries-") && tc.name != "git-argument-boundaries-" {
					if _, err := os.Stat(filepath.Join(f.base, "git-arguments")); !os.IsNotExist(err) {
						t.Errorf("read-only or directory-only mode invoked Git: %v", err)
					}
				}
				if tc.repeat {
					first := results[i]
					results[i] = runParity(t, f, executable, args, tc)
					results[i].stdout = first.stdout + "REPEAT\n" + results[i].stdout
					results[i].stderr = first.stderr + "REPEAT\n" + results[i].stderr
					if first.status != results[i].status {
						t.Fatalf("repeat status changed: %d to %d", first.status, results[i].status)
					}
				}
			}
			expected := results[0]
			if tc.expectedFatal == -1 {
				// The approved contract keeps Bash 5.2's 127 for a missing
				// interpreter. Bash 5.3's 126 is the sole accepted exception.
				if expected.status != 126 && expected.status != 127 {
					t.Fatalf("unexpected reference interpreter failure: %d", expected.status)
				}
				expected.status = 127
			}
			if fmt.Sprintf("%#v", expected) != fmt.Sprintf("%#v", results[1]) {
				t.Errorf("shell: %#v\ncandidate: %#v", results[0], results[1])
			}
			if strings.HasPrefix(tc.name, "fatal-") {
				expectedFatal := tc.expectedFatal
				if expectedFatal == -1 {
					expectedFatal = 127
				}
				if expectedFatal == 0 {
					expectedFatal = 37
				}
				for i, result := range results {
					statusMatches := result.status == expectedFatal
					if tc.expectedFatal == -1 && i == 0 {
						statusMatches = result.status == 126 || result.status == 127
					}
					if !statusMatches || strings.Contains(result.stdout, "workspace-sync summary:") || strings.Contains(result.stdout, "workspace-sync status:") || strings.Contains(strings.Join(result.state, "\n"), "links/later") {
						t.Errorf("fatal failure must abort with original status and no summary or later link: %#v", result)
					}
				}
			}
		})
	}
}

func TestPinnedExistingFixtures(t *testing.T) {
	binary := candidateBinary(t)
	fixture, err := os.ReadFile("testdata/reference/scripts/checks/test-workspace-sync.sh")
	if err != nil {
		t.Fatal(err)
	}
	reference, err := os.ReadFile(referenceScript)
	if err != nil {
		t.Fatal(err)
	}
	for _, variant := range []string{"shell", "candidate"} {
		t.Run(variant, func(t *testing.T) {
			f := newParityFixture(t)
			script := filepath.Join(f.base, "reference/scripts/checks/test-workspace-sync.sh")
			fixtureWrite(t, script, string(fixture), 0755)
			implementation := string(reference)
			if variant == "candidate" {
				f.env = append(f.env, "FIXTURE_WORKSPACE_SYNC="+binary)
				implementation = "exec \"$FIXTURE_WORKSPACE_SYNC\" \"$@\"\n"
			}
			fixtureShellScript(t, filepath.Join(f.base, "reference/modules/home-manager/workspace/workspace-sync.sh"), implementation)
			fixtureCommand(t, f, "bash", script)
		})
	}
}
