package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"nix-deploy/bootcheck"
)

// unitFailurePattern matches the lines of an activation log that name a
// unit that failed: systemd's "Failed to start", switch-to-configuration's
// "the following units failed" and systemctl's "Job for ... failed".
var unitFailurePattern = regexp.MustCompile(`Failed to start |the following units failed|^\s*Job for \S+ failed`)

// maxFindingLines caps the unit failure lines and the test names the
// context lists.
const maxFindingLines = 20

// generatePattern matches go generate's report of a directive that failed,
// such as `cmd/stash/main.go:1: running "go": exit status 1`. It prints only
// the exit status; the generator's own message, if it printed one, is
// elsewhere in the log, and some generators (gqlgen, for one) print the cause
// only with a verbose flag.
var generatePattern = regexp.MustCompile(`^\S+\.go:[0-9]+: running "[^"]+": exit status [0-9]+$`)

// generateFailures returns the distinct go generate failure lines of a log,
// in order, at most maxFindingLines.
func generateFailures(text string) []string {
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimRight(line, "\r"))
		if generatePattern.MatchString(line) && !contains(lines, line) && len(lines) < maxFindingLines {
			lines = append(lines, line)
		}
	}
	return lines
}

// unitFailures returns the distinct lines of a log that name a failed
// unit, in order, at most maxFindingLines, and whether there were more.
func unitFailures(text string) ([]string, bool) {
	var lines []string
	seen := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimRight(line, "\r"))
		if line == "" || seen[line] || !unitFailurePattern.MatchString(line) {
			continue
		}
		if len(lines) == maxFindingLines {
			return lines, true
		}
		seen[line] = true
		lines = append(lines, line)
	}
	return lines, false
}

// testFailures are the Go test failures of a log: the failing tests in
// order (subtests named with a slash) and their messages grouped by what
// they share.
type testFailures struct {
	tests  []string
	groups []messageGroup
}

// messageGroup is one message shared by failing tests: key is the part
// they share, example one whole line that carried it.
type messageGroup struct {
	key     string
	example string
	tests   []string
}

var (
	goFailPattern    = regexp.MustCompile(`^\s*--- FAIL: (\S+)`)
	goMessagePattern = regexp.MustCompile(`^\s+[\w./-]+\.go:[0-9]+: (.*)$`)
)

// goTestFailures reads the "--- FAIL:" blocks of go test output. A failing
// test's messages are its indented "<file>.go:<line>: <message>" lines and
// the indented lines with "error: " in them; each message is grouped by its
// last ": "-separated part, which is usually the underlying cause, so tests
// that fail for one reason group together even when their wrapping
// differs. A last part shorter than minCauseLength (such as "EOF") is too
// generic to stand for a cause, so such a message is grouped whole, and the
// context says so.
// minCauseLength is the shortest last ": "-separated part that groups test
// messages by itself.
const minCauseLength = 12

func goTestFailures(text string) testFailures {
	var result testFailures
	index := map[string]int{}
	current := ""
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		if m := goFailPattern.FindStringSubmatch(line); m != nil {
			current = m[1]
			result.tests = append(result.tests, current)
			continue
		}
		if current == "" {
			continue
		}
		if line == "" || (line[0] != ' ' && line[0] != '\t') || strings.HasPrefix(strings.TrimSpace(line), "=== ") {
			current = ""
			continue
		}
		message := ""
		if m := goMessagePattern.FindStringSubmatch(line); m != nil {
			message = m[1]
		} else if i := strings.LastIndex(line, "error: "); i >= 0 {
			message = line[i+len("error: "):]
		} else {
			continue
		}
		key := strings.TrimSpace(message)
		if i := strings.LastIndex(key, ": "); i >= 0 && len(strings.TrimSpace(key[i+2:])) >= minCauseLength {
			key = strings.TrimSpace(key[i+2:])
		}
		if key == "" {
			continue
		}
		i, ok := index[key]
		if !ok {
			i = len(result.groups)
			index[key] = i
			result.groups = append(result.groups, messageGroup{key: key, example: strings.TrimSpace(line)})
		}
		if !contains(result.groups[i].tests, current) {
			result.groups[i].tests = append(result.groups[i].tests, current)
		}
	}
	sort.SliceStable(result.groups, func(a, b int) bool {
		return len(result.groups[a].tests) > len(result.groups[b].tests)
	})
	return result
}

// maxMessageGroups caps the grouped test messages the context lists.
const maxMessageGroups = 8

func writeTestFailures(out *strings.Builder, f testFailures) {
	subtests := 0
	for _, test := range f.tests {
		if strings.Contains(test, "/") {
			subtests++
		}
	}
	fmt.Fprintf(out, "\nGo test failures: %d \"--- FAIL:\" lines (%d of them subtests).", len(f.tests), subtests)
	if len(f.groups) > 0 {
		fmt.Fprintf(out, " Messages shared by the most failing tests, grouped by their last \": \"-separated part when it has at least %d characters, and whole otherwise (so short causes such as `EOF` behind different wrapping are not merged):\n\n", minCauseLength)
		for i, g := range f.groups {
			if i == maxMessageGroups {
				fmt.Fprintf(out, "- and %d more messages\n", len(f.groups)-maxMessageGroups)
				break
			}
			fmt.Fprintf(out, "- %d test(s): %s (for example %s in `%s`)\n", len(g.tests), g.key, "`"+strings.ReplaceAll(g.example, "`", "'")+"`", g.tests[0])
		}
	} else {
		out.WriteString("\n")
	}
	names := f.tests
	more := ""
	if len(names) > maxFindingLines {
		more = fmt.Sprintf(", and %d more", len(names)-maxFindingLines)
		names = names[:maxFindingLines]
	}
	fmt.Fprintf(out, "\nFailing tests: %s%s.\n", strings.Join(names, ", "), more)
}

// drvFacts is what a derivation file tells without Nix: whether it is
// fixed-output, the system it builds for and the store paths it names.
type drvFacts struct {
	path    string
	present bool
	fixed   bool
	system  string
	pname   string
	paths   []string
}

var (
	// The first output of the ATerm: ("out","<path>","<hash algo>","<hash>");
	// a fixed-output derivation has a hash.
	drvFirstOutput = regexp.MustCompile(`^Derive\(\[\("[^"]*","[^"]*","[^"]*","([^"]*)"\)`)
	drvSystem      = regexp.MustCompile(`\("system","([^"]*)"\)`)
	// pname as an environment entry, or inside the escaped JSON of
	// structured attributes.
	drvPname           = regexp.MustCompile(`\("pname","([^"\\]*)"\)`)
	drvStructuredPname = regexp.MustCompile(`\\"pname\\":\\"([^"\\]*)\\"`)
	storePathRef       = regexp.MustCompile(`/nix/store/[0-9a-z]{32}-[^"/\\\s]+`)
)

// drvPlatform returns the platform field of a derivation in ATerm form,
// Derive(outputs,inputDrvs,inputSrcs,"platform",builder,args,env): the
// fourth top-level field. Unlike the "system" environment entry, it is there
// also when the derivation uses structured attributes, whose environment
// holds the settings as escaped JSON instead. It returns "" when the text is
// not in that form.
func drvPlatform(text string) string {
	rest, ok := strings.CutPrefix(text, "Derive(")
	if !ok {
		return ""
	}
	field, depth, start := 0, 0, 0
	inString, escaped := false, false
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		switch {
		case inString && escaped:
			escaped = false
		case inString && c == '\\':
			escaped = true
		case inString && c == '"':
			inString = false
		case inString:
		case c == '"':
			inString = true
		case c == '[' || c == '(':
			depth++
		case c == ']' || c == ')':
			depth--
			if depth < 0 {
				return ""
			}
		case c == ',' && depth == 0:
			if field == 3 {
				value := rest[start:i]
				if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' && !strings.Contains(value, "\\") {
					return value[1 : len(value)-1]
				}
				return ""
			}
			field++
			start = i + 1
		}
	}
	return ""
}

// readDrv reads a derivation file from the store. A derivation the store
// no longer has (collected since the run) is not present.
func readDrv(path string) drvFacts {
	f := drvFacts{path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		return f
	}
	f.present = true
	text := string(data)
	if m := drvFirstOutput.FindStringSubmatch(text); m != nil && m[1] != "" {
		f.fixed = true
	}
	f.system = drvPlatform(text)
	if m := drvPname.FindStringSubmatch(text); m != nil {
		f.pname = m[1]
	} else if m := drvStructuredPname.FindStringSubmatch(text); m != nil {
		f.pname = m[1]
	}
	if f.system == "" {
		if m := drvSystem.FindStringSubmatch(text); m != nil {
			f.system = m[1]
		}
	}
	seen := map[string]bool{}
	for _, ref := range storePathRef.FindAllString(text, -1) {
		if !seen[ref] {
			seen[ref] = true
			f.paths = append(f.paths, ref)
		}
	}
	return f
}

// maxAttempted caps the attempted derivations the context lists and the
// fixed-output ones a failed build rebuilds.
const maxAttempted = 20

// attemptedDerivation is a derivation the failed build attempted, as
// build.log names it, with the run's mark of it as fixed-output.
type attemptedDerivation struct {
	drv   string
	fixed bool
}

// attemptedFromBuildLog reads the attempted derivations from build.log:
// the "attempted" lines since 0.33.0, or the derivations whose logs it
// lists in older records.
func attemptedFromBuildLog(text string) []attemptedDerivation {
	var attempted, logged []attemptedDerivation
	for _, line := range strings.Split(text, "\n") {
		switch {
		case strings.HasPrefix(line, attemptedFixedPrefix):
			attempted = append(attempted, attemptedDerivation{drv: strings.TrimPrefix(line, attemptedFixedPrefix), fixed: true})
		case strings.HasPrefix(line, attemptedPrefix):
			attempted = append(attempted, attemptedDerivation{drv: strings.TrimPrefix(line, attemptedPrefix)})
		case strings.HasPrefix(line, "log of "):
			if drv, _, ok := strings.Cut(strings.TrimPrefix(line, "log of "), ": "); ok {
				logged = append(logged, attemptedDerivation{drv: drv})
			}
		}
	}
	if attempted != nil {
		return attempted
	}
	return logged
}

// derivationName drops the directory, the store hash and .drv from a
// derivation path.
func derivationName(drv string) string {
	name := strings.TrimSuffix(filepath.Base(drv), ".drv")
	if hash, rest, ok := strings.Cut(name, "-"); ok && len(hash) == 32 {
		return rest
	}
	return name
}

// isNixfleet tells whether a derivation builds nixfleet (or its nix-deploy
// alias): its name is the package name and a version, such as
// nixfleet-0.32.0 or nixfleet-0.32.0-go-modules, unlike helpers such as
// nixfleet-runner-status.
func isNixfleet(drv string) bool {
	name := derivationName(drv)
	for _, prefix := range []string{"nixfleet-", "nix-deploy-"} {
		if rest, ok := strings.CutPrefix(name, prefix); ok && rest != "" && rest[0] >= '0' && rest[0] <= '9' {
			return true
		}
	}
	return false
}

// inputsByStorePath maps the store path of each locked input to its names.
func inputsByStorePath(inputs map[string]lockedInput) map[string][]string {
	byPath := map[string][]string{}
	for _, name := range sortedNames(inputs) {
		in := inputs[name]
		if path := lockedStorePath(in.NarHash, in.storeName()); path != "" {
			byPath[path] = append(byPath[path], name)
		}
	}
	return byPath
}

// drvInputs returns the locked inputs whose store paths a derivation names,
// in order and without repeats.
func drvInputs(f drvFacts, byPath map[string][]string) []string {
	var names []string
	for _, ref := range f.paths {
		for _, name := range byPath[ref] {
			if !contains(names, name) {
				names = append(names, name)
			}
		}
	}
	return names
}

// definitionRoot is a source tree searched for package definitions: an own
// input's store path or the flake directory, with how the context names it.
type definitionRoot struct {
	label string
	dir   string
}

// Bounds of the definition search, so a huge tree cannot stall triage.
const (
	maxDefinitionFiles    = 20000
	maxDefinitionFileSize = 1 << 20
	maxDefinitionsShown   = 5
)

// definitionSearch is what findDefinitions found: for each pname, the files
// that set it, as "<label> <relative path>"; how many roots it walked; and
// the labels of roots whose walk stopped at maxDefinitionFiles.
type definitionSearch struct {
	found     map[string][]string
	searched  int
	truncated []string
}

// findDefinitions finds, for each pname, the .nix files under the roots that
// set `pname = "<pname>"`. It reads each file once for all pnames and skips
// .git and node_modules; a root that cannot be read is not counted.
func findDefinitions(pnames []string, roots []definitionRoot) definitionSearch {
	search := definitionSearch{found: map[string][]string{}}
	found := search.found
	if len(pnames) == 0 {
		return search
	}
	patterns := map[string]*regexp.Regexp{}
	for _, pname := range pnames {
		patterns[pname] = regexp.MustCompile(`\bpname\s*=\s*"` + regexp.QuoteMeta(pname) + `"`)
	}
	for _, root := range roots {
		if info, err := os.Stat(root.dir); err != nil || !info.IsDir() {
			continue
		}
		search.searched++
		files := 0
		_ = filepath.WalkDir(root.dir, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if entry.IsDir() {
				if name := entry.Name(); path != root.dir && (name == ".git" || name == "node_modules") {
					return filepath.SkipDir
				}
				return nil
			}
			if !entry.Type().IsRegular() || !strings.HasSuffix(path, ".nix") {
				return nil
			}
			if files++; files > maxDefinitionFiles {
				search.truncated = append(search.truncated, root.label)
				return filepath.SkipAll
			}
			if info, err := entry.Info(); err != nil || info.Size() > maxDefinitionFileSize {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			rel, _ := filepath.Rel(root.dir, path)
			for _, pname := range pnames {
				if patterns[pname].Match(data) {
					found[pname] = append(found[pname], root.label+" "+rel)
				}
			}
			return nil
		})
	}
	return search
}

// maxInputCommits caps the commits listed between two locked revisions.
const maxInputCommits = 30

// inputCommits describes the commits between two locked revisions of an
// input: from the own input's checkout when there is one, or as a compare
// link for a GitHub input; nothing otherwise.
func inputCommits(was, now lockedInput, checkout string) string {
	if was.Rev == "" || now.Rev == "" || was.Rev == now.Rev {
		return ""
	}
	if checkout != "" {
		if info, err := os.Stat(checkout); err == nil && info.IsDir() {
			output, ok := readGit(checkout, "log", "--oneline", "--no-decorate", fmt.Sprintf("--max-count=%d", maxInputCommits), was.Rev+".."+now.Rev)
			switch {
			case !ok:
				return fmt.Sprintf("the checkout %s lacks one of the two revisions; fetch it to list the commits", checkout)
			case strings.TrimSpace(string(output)) == "":
				return fmt.Sprintf("no commits in %s..%s (the new revision does not descend from the old one)", short(was.Rev), short(now.Rev))
			}
			return fmt.Sprintf("commits from %s (newest first, at most %d):\n\n````text\n%s\n````", checkout, maxInputCommits, strings.TrimRight(string(output), "\n"))
		}
	}
	if was.Type == "github" && now.Type == "github" && was.Owner == now.Owner && was.Repo == now.Repo && was.Host == "" {
		return fmt.Sprintf("compare: https://github.com/%s/%s/compare/%s...%s", now.Owner, now.Repo, was.Rev, now.Rev)
	}
	return ""
}

func short(rev string) string {
	if len(rev) > 12 {
		return rev[:12]
	}
	return rev
}

// writeAttempted writes the derivations the failed build attempted, each
// marked fixed-output where it is, with the system it builds for and the
// locked inputs whose sources it names, and for an input that changed
// since the baseline, the change and its commits.
func writeAttempted(out *strings.Builder, attempted []attemptedDerivation, total int, facts []drvFacts, inputs map[string]lockedInput, own map[string]bool, nodeCheckouts map[string]string, baseline *baselineGroup, roots []definitionRoot) {
	out.WriteString("\n### Attempted derivations\n\nThe derivations the build attempted (none of their inputs was left to build). The inputs are the locked inputs whose store paths the derivation file names; a source that was filtered or copied into its own store path hides which input it came from.\n")
	if baseline != nil && baseline.old != nil {
		var names []string
		for _, t := range baseline.targets {
			names = append(names, t.host+" "+t.label)
		}
		fmt.Fprintf(out, "Changes are since the baseline of %s.\n", strings.Join(names, ", "))
	}
	byPath := inputsByStorePath(inputs)
	// A derivation whose sources name no locked input (a fetchFromGitHub
	// source, for example) is matched to own definitions by its pname.
	var pnames []string
	for _, f := range facts {
		if f.present && f.pname != "" && len(drvInputs(f, byPath)) == 0 && !contains(pnames, f.pname) {
			pnames = append(pnames, f.pname)
		}
	}
	search := findDefinitions(pnames, roots)
	definitions := search.found
	out.WriteString("\n")
	for i, a := range attempted {
		f := facts[i]
		var marks []string
		if a.fixed || f.fixed {
			marks = append(marks, "fixed-output: Nix reports a hash mismatch itself, outside the builder's log")
		}
		if f.system != "" {
			marks = append(marks, "builds for "+f.system)
		}
		if !f.present {
			marks = append(marks, "no longer in this machine's store")
		}
		fmt.Fprintf(out, "- %s", a.drv)
		if len(marks) > 0 {
			fmt.Fprintf(out, " (%s)", strings.Join(marks, "; "))
		}
		out.WriteString("\n")
		if !f.present {
			continue
		}
		names := drvInputs(f, byPath)
		if len(names) == 0 {
			out.WriteString("  - Inputs: no locked input's store path appears in it.\n")
			defs := definitions[f.pname]
			switch {
			case f.pname == "" || search.searched == 0:
			case len(defs) > 0:
				shown := defs
				if len(shown) > maxDefinitionsShown {
					shown = shown[:maxDefinitionsShown]
				}
				fmt.Fprintf(out, "  - Own definitions of pname `%s`, so this may be the operator's package rather than upstream's: %s", f.pname, strings.Join(shown, ", "))
				if len(defs) > len(shown) {
					fmt.Fprintf(out, ", and %d more", len(defs)-len(shown))
				}
				out.WriteString("\n")
			default:
				trees := "source trees"
				if search.searched == 1 {
					trees = "source tree"
				}
				fmt.Fprintf(out, "  - No file in the %d %s searched (the own inputs in this store and the flake directory, where present) sets pname `%s`. It is upstream's package, or an override of it: an overlay's `overrideAttrs` keeps upstream's pname, so search the flake and own inputs for `%s` before calling it upstream.\n", search.searched, trees, f.pname, f.pname)
				if len(search.truncated) > 0 {
					fmt.Fprintf(out, "  - The search stopped at %d files in %s, so it is incomplete there.\n", maxDefinitionFiles, strings.Join(search.truncated, ", "))
				}
			}
			continue
		}
		for _, name := range names {
			mark := "upstream"
			if own[name] {
				mark = "own"
			}
			fmt.Fprintf(out, "  - Input `%s` (%s)", name, mark)
			if baseline == nil || baseline.old == nil {
				out.WriteString("\n")
				continue
			}
			was, ok := baseline.old[name]
			now := inputs[name]
			switch {
			case !ok:
				out.WriteString(": added since the baseline\n")
			case was.whole == now.whole:
				out.WriteString(": unchanged since the baseline\n")
			default:
				fmt.Fprintf(out, ": changed since the baseline, %s -> %s\n", was.identity(), now.identity())
				if commits := inputCommits(was, now, nodeCheckouts[name]); commits != "" {
					fmt.Fprintf(out, "    - %s\n", strings.ReplaceAll(commits, "\n", "\n      "))
				}
			}
		}
	}
	if total > len(attempted) {
		fmt.Fprintf(out, "- and %d more; build.log lists them all.\n", total-len(attempted))
	}
}

// maxEarlierRuns caps the earlier runs of the same lock listed.
const maxEarlierRuns = 10

// writeEarlierRuns lists the earlier kept runs of the same flake that
// deployed the same lock, newest first, with their results, so a failure
// shows whether it is new or part of a pattern.
func writeEarlierRuns(out *strings.Builder, dir string, info runInfo) {
	out.WriteString("\n## Earlier runs of this lock\n\n")
	if info.LockSHA256 == "" {
		out.WriteString("The run has no lock to compare.\n")
		return
	}
	var same []keptRun
	for _, run := range keptRuns(filepath.Dir(dir), info) {
		if run.info.LockSHA256 == info.LockSHA256 {
			same = append(same, run)
		}
	}
	if len(same) == 0 {
		out.WriteString("No earlier kept run of this flake deployed the same lock.\n")
		return
	}
	out.WriteString("Earlier kept runs of this flake with the same lock, newest first:\n\n")
	for i, run := range same {
		if i == maxEarlierRuns {
			fmt.Fprintf(out, "- and %d older\n", len(same)-maxEarlierRuns)
			break
		}
		result := "did not finish"
		if run.info.Failed != nil {
			result = "ok"
			if *run.info.Failed {
				result = "failed"
			}
		}
		var modes []string
		if run.info.DryRun {
			modes = append(modes, "dry-run")
		}
		if run.info.BuildOnly {
			modes = append(modes, "build-only")
		}
		var failed []string
		for _, host := range run.report.Hosts {
			if host.System.Status == "failed" {
				failed = append(failed, host.Host+" system")
			}
			if host.HomeManager.Status == "failed" {
				failed = append(failed, host.Host+" home-manager")
			}
		}
		fmt.Fprintf(out, "- %s: %s, `%s`", run.id, result, strings.Join(run.info.Args, " "))
		if len(modes) > 0 {
			fmt.Fprintf(out, " (%s)", strings.Join(modes, ", "))
		}
		if len(failed) > 0 {
			fmt.Fprintf(out, "; failed: %s", strings.Join(failed, ", "))
		}
		out.WriteString("\n")
	}
}

// failedUnitsScript lists a host's failed units with the time each entered
// that state and its first failure in the journal of the current boot. A
// listing that fails, such as when activation left systemd's bus
// unreachable, fails the script rather than reading as no failed units. It
// runs inside sh -c '...', so it has no single quotes.
const failedUnitsScript = `command -v systemctl >/dev/null || { echo "no-systemd"; exit 0; }; ` +
	`units=$(systemctl --failed --plain --no-legend --no-pager) || exit 1; ` +
	`printf "%s\n" "$units" | while read -r unit rest; do [ -n "$unit" ] || continue; ` +
	`since=$(systemctl show -p StateChangeTimestamp --value "$unit" 2>/dev/null); ` +
	`first=$(journalctl -b -u "$unit" -o short-iso --no-pager -q -g "Failed with result|Failed to start" 2>/dev/null | head -n 1 | cut -d " " -f 1); ` +
	`printf "unit\t%s\t%s\t%s\n" "$unit" "$since" "$first"; done`

// readLiveState reads the live state of hosts with a failed activation;
// tests replace it.
var readLiveState = defaultReadLiveState

// defaultReadLiveState reads nixfleet status of the hosts and each Linux
// host's failed units, and says when and how long that took.
func defaultReadLiveState(configFile string, hosts []string) string {
	var out strings.Builder
	if configFile == "" {
		fmt.Fprintf(&out, "No inventory is configured (--config FILE or NIX_DEPLOY_CONFIG), so live state was not read. Run `nixfleet status %s` and `systemctl --failed` on each host.\n", strings.Join(hosts, " "))
		return out.String()
	}
	s, _, err := openStatusCheck(configFile)
	if err != nil {
		fmt.Fprintf(&out, "Live state was not read: %v. Run `nixfleet status %s` and `systemctl --failed` on each host.\n", err, strings.Join(hosts, " "))
		return out.String()
	}
	s.ui = &ui{}
	started := time.Now()
	report := s.collect(hosts)
	var status bytes.Buffer
	printStatus(&status, s.ui, report)
	units := map[string]string{}
	for _, h := range report.Hosts {
		if h.Reachable && h.OS == "Linux" {
			units[h.Host] = s.failedUnits(h.Host)
		}
	}
	fmt.Fprintf(&out, "Read at %s, in %s, by `nixfleet status %s` and `systemctl --failed` on each Linux host. It goes stale quickly: read it again before relying on it.\n\n````text\n%s````\n",
		started.UTC().Format(time.RFC3339), formatDuration(time.Since(started)), strings.Join(hosts, " "), status.String())
	for _, h := range report.Hosts {
		if text, ok := units[h.Host]; ok {
			fmt.Fprintf(&out, "\n### Failed units on %s\n\n%s", h.Host, text)
		}
	}
	return out.String()
}

// failedUnits reads a host's failed units as a Markdown table.
func (s *statusCheck) failedUnits(host string) string {
	argv := []string{"sh", "-c", failedUnitsScript}
	if host != s.self {
		argv = []string{"ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=" + s.connectTimeout, "-o", "ConnectionAttempts=1"}
		argv = append(argv, s.sshArgs...)
		argv = append(argv, s.inv.systemEndpoint(host), bootcheck.RemoteCommand(failedUnitsScript))
	}
	stdout, stderr, status := s.run(statusReadTimeout, argv...)
	if status != 0 {
		reason := oneLine(stderr)
		if reason == "" {
			reason = fmt.Sprintf("%s exited with status %d", argv[0], status)
		}
		return "Not read: " + reason + "\n"
	}
	if strings.TrimSpace(stdout) == "no-systemd" {
		return "The host has no systemctl.\n"
	}
	var rows []string
	for _, line := range strings.Split(stdout, "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) != 4 || fields[0] != "unit" {
			continue
		}
		since, first := fields[2], fields[3]
		if since == "" {
			since = "unknown"
		}
		if first == "" {
			first = "not found (or the journal is not readable)"
		}
		rows = append(rows, fmt.Sprintf("| %s | %s | %s |", fields[1], since, first))
	}
	if len(rows) == 0 {
		return "No failed system units. User units are not read: for a Home Manager failure, run `systemctl --user --failed` as the user on the host.\n"
	}
	return "System units only; user units are not read. \"Failed since\" is when each unit's current failure began: check whether it recovered between an earlier failure and the switch before blaming or clearing the deploy.\n\n| Unit | Failed since | First failure this boot |\n|---|---|---|\n" + strings.Join(rows, "\n") + "\n"
}

// closurePriority matches the packages of a closure diff that most often
// explain an activation failure: kernels, drivers and firmware.
var closurePriority = regexp.MustCompile(`(?i)^(linux|kernel|nvidia|[^:]*firmware|[^:]*driver|mesa)`)

// maxClosureLines caps the closure diff lines shown in the context.
const maxClosureLines = 40

// writeClosureDiff shows a target's closure diff log, kernel and driver
// changes first.
func writeClosureDiff(out *strings.Builder, log string) {
	data, err := os.ReadFile(log)
	if err != nil {
		return
	}
	lines := strings.Split(strings.TrimRight(ansiPattern.ReplaceAllString(string(data), ""), "\n"), "\n")
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "nix store diff-closures ") {
		fmt.Fprintf(out, "\nClosure diff: %s\n", strings.Join(lines, " "))
		return
	}
	var first, rest []string
	for _, line := range lines[1:] {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if closurePriority.MatchString(strings.TrimSpace(line)) {
			first = append(first, line)
		} else {
			rest = append(rest, line)
		}
	}
	shown := append(first, rest...)
	if len(shown) == 0 {
		fmt.Fprintf(out, "\nClosure diff from the generation the host ran to the failed one (%s): no differences.\n", log)
		return
	}
	cut := ""
	if len(shown) > maxClosureLines {
		cut = fmt.Sprintf(" The first %d of %d lines.", maxClosureLines, len(shown))
		shown = shown[:maxClosureLines]
	}
	fmt.Fprintf(out, "\nClosure diff from the generation the host ran to the failed one, kernel and driver changes first (%s).%s\n\n````text\n%s\n````\n", log, cut, strings.Join(shown, "\n"))
}
