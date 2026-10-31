package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// triageWarning is one distinct warning of a run, with the targets that
// printed it in target order.
type triageWarning struct {
	kind    string // evaluation warning or trace: warning
	lines   []string
	cut     bool
	targets []target
}

// warningStarts are the line prefixes that start a message in an
// evaluation log, which also end the message before them.
var warningStarts = []string{"evaluation warning:", "trace: ", "warning:", "error:", "deploy:"}

// nixStatusPattern matches the progress and status lines Nix 2.35 prints
// to standard error during an evaluation (from libnixstore, libnixmain and
// libnixfetchers), ANSI escapes removed. They end a warning.
var nixStatusPattern = regexp.MustCompile(`^(building '|copying path '|copying '|downloading '|` +
	`fetching (Git repository|Mercurial repository|the input|input|URL) '|fetching \S+ from |` +
	`querying info about |querying \S+ on |unpacking '.*' into the Git cache|` +
	`these [0-9]+ (derivations will be built|paths will be fetched)|this (derivation will be built|path will be fetched)|` +
	`waiting for (another Nix process|lock on|exclusive access|auto-GC|the big|a machine|a free build user))`)

// isEvaluationResult tells whether a line is the JSON result deploy's
// evaluation prints: an object with exactly the string keys drv and out.
func isEvaluationResult(line string) bool {
	if !strings.HasPrefix(line, "{") {
		return false
	}
	var result map[string]json.RawMessage
	if json.Unmarshal([]byte(line), &result) != nil || len(result) != 2 {
		return false
	}
	return isString(result["drv"]) && isString(result["out"])
}

// extractWarnings returns the evaluation and trace warnings of an
// evaluation log, ANSI escapes removed. A message runs until the next line
// that starts a message, a Nix progress or status line, or the evaluation's
// JSON result, since Nix indents the continuation of an evaluation warning
// but prints a trace message as it is; trailing blank lines are dropped and
// a block is cut at maxErrorLines.
func extractWarnings(text string) []triageWarning {
	var found []triageWarning
	var current *triageWarning
	finish := func() {
		if current == nil {
			return
		}
		for len(current.lines) > 1 && strings.TrimSpace(current.lines[len(current.lines)-1]) == "" {
			current.lines = current.lines[:len(current.lines)-1]
		}
		found = append(found, *current)
		current = nil
	}
	for _, line := range strings.Split(ansiPattern.ReplaceAllString(text, ""), "\n") {
		line = strings.TrimRight(line, "\r")
		starts := false
		for _, prefix := range warningStarts {
			if strings.HasPrefix(line, prefix) {
				starts = true
			}
		}
		if starts || nixStatusPattern.MatchString(line) || isEvaluationResult(line) {
			finish()
			switch {
			case strings.HasPrefix(line, "evaluation warning:"):
				current = &triageWarning{kind: "evaluation warning", lines: []string{line}}
			case strings.HasPrefix(line, "trace: warning:"):
				current = &triageWarning{kind: "trace: warning", lines: []string{line}}
			}
			continue
		}
		if current == nil {
			continue
		}
		if len(current.lines) == maxErrorLines {
			current.cut = true
			continue
		}
		current.lines = append(current.lines, line)
	}
	finish()
	return found
}

// warningKey is a warning's message with its whitespace normalised.
func warningKey(w triageWarning) string {
	return strings.Join(strings.Fields(strings.Join(w.lines, " ")), " ")
}

// collectWarnings returns the distinct warnings of a run's evaluation logs,
// in order of first appearance, with targets sorted by host, system first.
func collectWarnings(logs string) []*triageWarning {
	entries, _ := os.ReadDir(logs)
	var targets []target
	for _, entry := range entries {
		name, ok := strings.CutSuffix(entry.Name(), "-evaluate.log")
		phase, host, found := strings.Cut(name, "-")
		if ok && found && host != "" && (phase == "system" || phase == "hm") {
			targets = append(targets, target{phase: phase, host: host})
		}
	}
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].host != targets[j].host {
			return targets[i].host < targets[j].host
		}
		return targets[i].phase == "system" && targets[j].phase == "hm"
	})
	var warnings []*triageWarning
	byKey := map[string]*triageWarning{}
	for _, t := range targets {
		data, err := os.ReadFile(filepath.Join(logs, t.phase+"-"+t.host+"-evaluate.log"))
		if err != nil {
			continue
		}
		for _, w := range extractWarnings(string(data)) {
			key := w.kind + "\x00" + warningKey(w)
			seen := byKey[key]
			if seen == nil {
				seen = &triageWarning{kind: w.kind, lines: w.lines, cut: w.cut}
				byKey[key] = seen
				warnings = append(warnings, seen)
			}
			if len(seen.targets) == 0 || seen.targets[len(seen.targets)-1] != t {
				seen.targets = append(seen.targets, t)
			}
		}
	}
	return warnings
}

// optionPathPattern matches a backquoted option path, `a.b' or `a.b`: two
// or more plain attribute names.
var optionPathPattern = regexp.MustCompile("`([A-Za-z_][A-Za-z0-9_-]*(?:\\.[A-Za-z_][A-Za-z0-9_-]*)+)['`]")

// optionWordPattern is the word that makes a backquoted name an option.
var optionWordPattern = regexp.MustCompile(`(?i)\boptions?\b`)

// sentenceEndPattern ends a sentence of a warning.
var sentenceEndPattern = regexp.MustCompile(`[.!?](\s|$)|\n\s*\n`)

// optionPaths returns the names a warning calls options, without repeats:
// a backquoted dotted name in a sentence that says option or options before
// or after it, as the module system's renamed, obsolete and removed option
// messages do ("The option `a.b' ... renamed to `c.d'"). It is a heuristic:
// the names are hints. A name ending in .nix is a file.
func optionPaths(lines []string) []string {
	text := strings.Join(lines, "\n")
	var paths []string
	for _, match := range optionPathPattern.FindAllStringSubmatchIndex(text, -1) {
		path := text[match[2]:match[3]]
		before, after := text[:match[0]], text[match[1]:]
		if ends := sentenceEndPattern.FindAllStringIndex(before, -1); len(ends) > 0 {
			before = before[ends[len(ends)-1][1]:]
		}
		if end := sentenceEndPattern.FindStringIndex(after); end != nil {
			after = after[:end[0]]
		}
		called := optionWordPattern.MatchString(before) || optionWordPattern.MatchString(after)
		if called && !strings.HasSuffix(path, ".nix") && !contains(paths, path) {
			paths = append(paths, path)
		}
	}
	return paths
}

var storePrefixPattern = regexp.MustCompile(`^/nix/store/[0-9a-z]{32}-[^/:]+`)

// fileInput names the locked input a file comes from, by its store path or
// by its flake reference, and whether it is own. A flake reference matches
// an input on its revision and its source (type with owner and repository,
// or URL); a revision several inputs share that the source does not settle
// is ambiguous.
func fileInput(file string, inputs map[string]lockedInput, own map[string]bool) string {
	node := ""
	note := ""
	if prefix := storePrefixPattern.FindString(file); prefix != "" {
		for _, name := range sortedNames(inputs) {
			if lockedStorePath(inputs[name].NarHash, inputs[name].storeName()) == prefix {
				node = name
				break
			}
		}
	} else if text, _, ok := strings.Cut(strings.TrimPrefix(file, "«"), "»"); ok {
		ref := parseFlakeRef(text)
		var byRev, bySource []string
		for _, name := range sortedNames(inputs) {
			in := inputs[name]
			if ref.typ == "path" && in.Type == "path" && in.Path == ref.url {
				// A path input has no revision; its path identifies it.
				bySource = append(bySource, name)
				byRev = append(byRev, name)
				continue
			}
			if in.Rev == "" || in.Rev != ref.rev {
				continue
			}
			byRev = append(byRev, name)
			if ref.sameSource(in) {
				bySource = append(bySource, name)
			}
		}
		switch {
		case len(bySource) == 1:
			node = bySource[0]
		case len(byRev) == 1 && len(bySource) == 0:
			node, note = byRev[0], " (by revision only; the source differs from the lock's)"
		case len(byRev) > 1:
			var candidates []string
			for _, name := range byRev {
				candidates = append(candidates, inputLabel(name, own))
			}
			return "ambiguous: the revision matches " + strings.Join(candidates, "; ")
		}
	}
	if node == "" {
		return "not a locked input of the run (the flake itself, a patched or generated source such as applyPatches or import-from-derivation output, or another path)"
	}
	return inputLabel(node, own) + note
}

// flakeRef is the source a flake reference in a trace frame names. Nix
// 2.35 prints a forge input as <type>:<owner>/<repo>/<rev> with the host,
// when it is not the default, in the query (?host=...), and a Git, tarball
// or path input as its URL with the revision in the query
// (git+file:///src/x?ref=refs/heads/main&rev=<rev>, path:/src/x?narHash=...).
type flakeRef struct {
	typ, owner, repo, rev, host, url string
}

// forgeHosts are the default hosts of the forge input types.
var forgeHosts = map[string]string{"github": "github.com", "gitlab": "gitlab.com", "sourcehut": "git.sr.ht"}

func parseFlakeRef(text string) flakeRef {
	text, _, _ = strings.Cut(text, "#")
	location, query, _ := strings.Cut(text, "?")
	values, _ := url.ParseQuery(query)
	scheme, rest, _ := strings.Cut(location, ":")
	ref := flakeRef{rev: values.Get("rev")}
	if _, forge := forgeHosts[scheme]; forge {
		// A component may hold an escaped slash, as a GitLab subgroup does.
		parts := strings.Split(rest, "/")
		for i, part := range parts {
			if decoded, err := url.PathUnescape(part); err == nil {
				parts[i] = decoded
			}
		}
		ref.typ, ref.host = scheme, values.Get("host")
		if ref.host == "" {
			ref.host = forgeHosts[scheme]
		}
		if len(parts) >= 2 {
			ref.owner, ref.repo = parts[0], parts[1]
		}
		if len(parts) >= 3 {
			ref.rev = parts[2]
		}
		return ref
	}
	if typ, transport, ok := strings.Cut(scheme, "+"); ok {
		ref.typ, ref.url = typ, transport+":"+rest
		return ref
	}
	switch scheme {
	case "path":
		ref.typ, ref.url = "path", rest
	case "git":
		ref.typ, ref.url = "git", location
	case "http", "https", "file":
		ref.typ, ref.url = "tarball", location
	}
	return ref
}

// sameSource tells whether a locked input is the source the reference
// names: the same type and, for a forge, the same host, owner and
// repository, or else the same URL without its query.
func (ref flakeRef) sameSource(in lockedInput) bool {
	if in.Type != ref.typ {
		return false
	}
	if host, forge := forgeHosts[in.Type]; forge {
		if in.Host != "" {
			host = in.Host
		}
		return strings.EqualFold(host, ref.host) && strings.EqualFold(in.Owner, ref.owner) && strings.EqualFold(in.Repo, ref.repo)
	}
	locked, _, _ := strings.Cut(in.URL, "?")
	locked, _, _ = strings.Cut(locked, "#")
	return locked != "" && locked == ref.url
}

func inputLabel(node string, own map[string]bool) string {
	if own[node] {
		return "input `" + node + "`, own"
	}
	return "input `" + node + "`, upstream"
}

// triageDarwin tells whether this machine is a Mac; tests replace it.
var triageDarwin = runtime.GOOS == "darwin"

// warningCommand returns a target's attribute and its trace re-evaluation,
// to run from the flake directory.
func warningCommand(t target, homePrefix, self string, darwin bool) (string, string) {
	root, build := targetAttribute(t, homePrefix, self, darwin)
	return root + build, "nix eval --no-eval-cache --option abort-on-warn true --show-trace --raw " + shellQuote(root+build)
}

// writeWarningsContext writes context.md for the warnings of a finished
// run and returns its path; inv, when not nil, gives the Home Manager
// output prefix, and noInventory says why it is nil.
func writeWarningsContext(dir string, checkouts map[string]string, inv *inventory, noInventory string) (string, error) {
	info, _, err := readFinishedRun(dir)
	if err != nil {
		return "", err
	}
	lock, _ := os.ReadFile(filepath.Join(dir, "flake.lock"))
	inputs, lockErr := lockInputs(lock)
	own, unlocked := ownMarks(lock, checkouts)
	warnings := collectWarnings(filepath.Join(dir, "logs"))

	var out strings.Builder
	writeRunSummary(&out, dir, info)
	out.WriteString("## Warnings\n\n")
	if len(warnings) == 0 {
		out.WriteString("No evaluation log of this run holds an `evaluation warning:` or `trace: warning:` line. Warnings printed only to the terminal (the shared build and terminal activations) are not recorded.\n")
	} else {
		fmt.Fprintf(&out, "%d distinct warnings from the run's evaluation logs, deduplicated by message. The names listed as options are hints from the wording, not checked against the module system. `--option abort-on-warn true` stops an evaluation warning, not a trace warning. Files map to inputs through the store paths under \"Locked inputs\".\n", len(warnings))
	}
	for i, w := range warnings {
		var names []string
		for _, t := range w.targets {
			names = append(names, t.host+" "+phaseLabel(t.phase))
		}
		fmt.Fprintf(&out, "\n### Warning %d (%s)\n\nPrinted by: %s\n\n````text\n%s\n````\n", i+1, w.kind, strings.Join(names, ", "), strings.Join(w.lines, "\n"))
		if w.cut {
			fmt.Fprintf(&out, "\nThe warning is cut at %d lines.\n", maxErrorLines)
		}
		if paths := optionPaths(w.lines); len(paths) > 0 {
			fmt.Fprintf(&out, "\nNames the message calls options (heuristic): `%s`\n", strings.Join(paths, "`, `"))
		}
		if files := errorFiles(w.lines); len(files) > 0 {
			out.WriteString("\nFiles it names:\n\n")
			for _, file := range files {
				if lockErr != nil {
					fmt.Fprintf(&out, "- %s: not mapped, since the run has no readable flake.lock\n", file)
				} else {
					fmt.Fprintf(&out, "- %s: %s\n", file, fileInput(file, inputs, own))
				}
			}
		}
	}

	writeFlakeCheckout(&out, info)
	if len(warnings) > 0 {
		homePrefix := "<homeOutputPrefix>"
		if inv != nil {
			homePrefix = inv.HomeOutputPrefix
		}
		hostname, _ := os.Hostname()
		self, _, _ := strings.Cut(hostname, ".")
		if inv != nil {
			self = inv.canonical(self)
		}
		fmt.Fprintf(&out, "\n## Commands per target\n\nRun these from the flake directory, %s; nixfleet ran none of them. The trace re-evaluation stops at the target's first evaluation warning. The playbook's Warnings section says how to look up an option's definitions. Attributes are formed as deploy forms them on this machine.", info.FlakeDir)
		if inv == nil {
			out.WriteString(" " + noInventory + ", so Home Manager attributes show `<homeOutputPrefix>` for its prefix")
			if triageDarwin {
				out.WriteString(", and host aliases are not resolved, so a system target that is this Mac uses `darwinConfigurations` instead of the `nixosConfigurations` shown")
			}
			out.WriteString(".")
		}
		out.WriteString("\n")
		var targets []target
		for _, w := range warnings {
			for _, t := range w.targets {
				if !containsTarget(targets, t) {
					targets = append(targets, t)
				}
			}
		}
		sort.Slice(targets, func(i, j int) bool {
			if targets[i].host != targets[j].host {
				return targets[i].host < targets[j].host
			}
			return targets[i].phase == "system" && targets[j].phase == "hm"
		})
		for _, t := range targets {
			var numbers []string
			for i, w := range warnings {
				if containsTarget(w.targets, t) {
					numbers = append(numbers, strconv.Itoa(i+1))
				}
			}
			// Without an inventory this machine's name is not alias-resolved,
			// so on a Mac a system target is not assumed to be nix-darwin.
			darwin := triageDarwin && inv != nil
			attribute, trace := warningCommand(t, homePrefix, self, darwin)
			fmt.Fprintf(&out, "\n### %s %s\n\n- Attribute: `%s`\n", t.host, phaseLabel(t.phase), attribute)
			if inv == nil && triageDarwin && t.phase == "system" {
				darwinAttribute, _ := warningCommand(t, homePrefix, t.host, true)
				fmt.Fprintf(&out, "- On the operator's Mac, if this host is that Mac: `%s`\n", darwinAttribute)
			}
			fmt.Fprintf(&out, "- Warnings: %s\n\nTrace re-evaluation:\n\n```sh\n%s\n```\n", strings.Join(numbers, ", "), trace)
		}
	}

	writeOwnInputs(&out, lock, checkouts)
	if lockErr != nil {
		out.WriteString("\n## Locked inputs\n\nThe run kept no readable flake.lock, so files cannot be mapped to inputs.\n")
	} else {
		writeLockedInputs(&out, inputs, own, unlocked)
	}

	path := filepath.Join(dir, "context.md")
	if err := replaceFile(path, []byte(out.String())); err != nil {
		return "", err
	}
	return path, nil
}

// writeFlakeCheckout compares the flake checkout now with the revision and
// dirty state the run recorded, since the commands evaluate the checkout as
// it is now.
func writeFlakeCheckout(out *strings.Builder, info runInfo) {
	fmt.Fprintf(out, "\n## Flake checkout\n\nThe commands below evaluate %s as it is now; the warnings come from the run's revision and lock.\n\n", info.FlakeDir)
	recorded := "tracked files clean"
	switch {
	case info.GitDirty == nil:
		recorded = "whether tracked files differed is unknown"
	case *info.GitDirty:
		recorded = "tracked files differed"
	}
	if info.GitRev == "" {
		out.WriteString("- Run: no Git revision recorded\n")
	} else {
		fmt.Fprintf(out, "- Run: %s, %s\n", info.GitRev, recorded)
	}
	head, ok := readGit(info.FlakeDir, "rev-parse", "--verify", "-q", "HEAD")
	if !ok {
		out.WriteString("- Now: Git cannot read a HEAD commit there\n- Match: unknown; work from a worktree at the run's revision\n")
		return
	}
	now := strings.TrimSpace(string(head))
	status, ok := readGit(info.FlakeDir, "status", "--porcelain", "--untracked-files=no")
	dirty := !ok || len(strings.TrimSpace(string(status))) > 0
	state := "tracked files clean"
	if dirty {
		state = "tracked files differ"
	}
	lock := "differs from the run's"
	if data, err := os.ReadFile(filepath.Join(info.FlakeDir, "flake.lock")); err == nil && lockSum(data) == info.LockSHA256 {
		lock = "same as the run's"
	}
	fmt.Fprintf(out, "- Now: %s, %s; flake.lock %s\n", now, state, lock)
	switch {
	case now == info.GitRev && !dirty && info.GitDirty != nil && !*info.GitDirty && lock == "same as the run's":
		out.WriteString("- Match: yes, the checkout is at the run's clean revision and lock\n")
	default:
		out.WriteString("- Match: no; work from a worktree at the run's revision, or confirm the difference does not matter for these warnings\n")
	}
}

func phaseLabel(phase string) string {
	if phase == "hm" {
		return "home-manager"
	}
	return "system"
}

func containsTarget(targets []target, t target) bool {
	for _, have := range targets {
		if have == t {
			return true
		}
	}
	return false
}
