package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// flakeLock is the part of a flake.lock the triage context reads.
type flakeLock struct {
	Root  string              `json:"root"`
	Nodes map[string]lockNode `json:"nodes"`
}

type lockNode struct {
	Locked json.RawMessage            `json:"locked"`
	Inputs map[string]json.RawMessage `json:"inputs"`
}

// lockedInput holds the fields of a locked node the context shows; whole is
// the entire locked object in a canonical form, which the diff compares so
// that no field can be missed.
type lockedInput struct {
	Type         string `json:"type"`
	Owner        string `json:"owner"`
	Repo         string `json:"repo"`
	Host         string `json:"host"`
	URL          string `json:"url"`
	Path         string `json:"path"`
	Dir          string `json:"dir"`
	Name         string `json:"name"`
	Rev          string `json:"rev"`
	NarHash      string `json:"narHash"`
	LastModified int64  `json:"lastModified"`
	whole        string
}

// lockInputs returns the locked nodes of a lock by name, without the root.
func lockInputs(data []byte) (map[string]lockedInput, error) {
	var lock flakeLock
	if err := json.Unmarshal(data, &lock); err != nil {
		return nil, err
	}
	inputs := map[string]lockedInput{}
	for name, node := range lock.Nodes {
		if name == lock.Root || node.Locked == nil {
			continue
		}
		var in lockedInput
		var fields map[string]any
		if err := json.Unmarshal(node.Locked, &in); err != nil {
			return nil, fmt.Errorf("node %s: %w", name, err)
		}
		if err := json.Unmarshal(node.Locked, &fields); err != nil {
			return nil, fmt.Errorf("node %s: %w", name, err)
		}
		// Marshalling a map sorts its keys.
		whole, _ := json.Marshal(fields)
		in.whole = string(whole)
		inputs[name] = in
	}
	return inputs, nil
}

func sortedNames(inputs map[string]lockedInput) []string {
	var names []string
	for name := range inputs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (in lockedInput) source() string {
	if in.Dir != "" {
		return in.location() + "?dir=" + in.Dir
	}
	return in.location()
}

func (in lockedInput) location() string {
	switch {
	case in.Owner != "" && in.Host != "":
		return in.Type + ":" + in.Host + "/" + in.Owner + "/" + in.Repo
	case in.Owner != "":
		return in.Type + ":" + in.Owner + "/" + in.Repo
	case in.URL != "":
		return in.Type + "+" + in.URL
	case in.Path != "":
		return in.Type + ":" + in.Path
	}
	return in.Type
}

// identity is the locked revision, or the NAR hash without one.
func (in lockedInput) identity() string {
	if in.Rev != "" {
		return in.Rev
	}
	if in.NarHash != "" {
		return in.NarHash
	}
	return "unknown"
}

func (in lockedInput) date() string {
	if in.LastModified == 0 {
		return "unknown"
	}
	return time.Unix(in.LastModified, 0).UTC().Format("2006-01-02 15:04 UTC")
}

func (in lockedInput) storePath() string {
	if path := lockedStorePath(in.NarHash, in.storeName()); path != "" {
		return path
	}
	return "unknown"
}

// lockedStorePath returns the store path Nix gives a locked flake input with
// the SRI SHA-256 NAR hash narHash and the store name name (the input's
// name, source by default), or nothing for another hash or an invalid name.
// Nix copies every fetched input, a file input included (checked with Nix
// 2.35), to the store as a NAR, so it is a recursive SHA-256 fixed-output
// path without references, which makeFixedOutputPath turns into
// makeStorePath("source", hash, name): the SHA-256 of
// "source:sha256:<hex>:/nix/store:<name>", folded to 20 bytes and printed
// in Nix's base32.
func lockedStorePath(narHash, name string) string {
	encoded, ok := strings.CutPrefix(narHash, "sha256-")
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if !ok || err != nil || len(raw) != sha256.Size || !validStoreName(name) {
		return ""
	}
	sum := sha256.Sum256([]byte("source:sha256:" + hex.EncodeToString(raw) + ":/nix/store:" + name))
	var folded [20]byte
	for i, b := range sum {
		folded[i%len(folded)] ^= b
	}
	return "/nix/store/" + nixBase32(folded[:]) + "-" + name
}

// storeNamePattern is the characters and length Nix 2.35 accepts in a store
// path name; validStoreName adds its other rule.
var storeNamePattern = regexp.MustCompile(`^[A-Za-z0-9+._?=-]{1,211}$`)

// validStoreName tells whether Nix accepts name in a store path: its
// characters and length, and a first dash-separated component other than
// "." or ".." (so ".", "..", ".-x" and "..-x" are rejected; ".probe" and
// "-x" are not).
func validStoreName(name string) bool {
	first, _, _ := strings.Cut(name, "-")
	return storeNamePattern.MatchString(name) && first != "." && first != ".."
}

// storeName is the input's store path name: its name, or source.
func (in lockedInput) storeName() string {
	if in.Name != "" {
		return in.Name
	}
	return "source"
}

// nixBase32 is Nix's base32: its own alphabet, read from the last 5-bit
// group to the first.
func nixBase32(data []byte) string {
	const alphabet = "0123456789abcdfghijklmnpqrsvwxyz"
	size := (len(data)*8-1)/5 + 1
	out := make([]byte, 0, size)
	for n := size - 1; n >= 0; n-- {
		bit := n * 5
		i, j := bit/8, uint(bit%8)
		c := data[i] >> j
		if i+1 < len(data) {
			c |= data[i+1] << (8 - j)
		}
		out = append(out, alphabet[c&0x1f])
	}
	return string(out)
}

// keptRun is a run beside the failed one that may give a baseline.
type keptRun struct {
	id     string
	info   runInfo
	report runReport
}

// keptRuns returns the runs in runs that started before failed, in the same
// flake directory, with a report and a flake.lock, newest first.
func keptRuns(runs string, failed runInfo) []keptRun {
	entries, _ := os.ReadDir(runs)
	var kept []keptRun
	for i := len(entries) - 1; i >= 0; i-- {
		id := entries[i].Name()
		dir := filepath.Join(runs, id)
		if id == failed.ID || !runIDPattern.MatchString(id) || !fileExists(filepath.Join(dir, "flake.lock")) {
			continue
		}
		info, err := readRunInfo(dir)
		if err != nil || !info.Started.Before(failed.Started) || info.FlakeDir != failed.FlakeDir {
			continue
		}
		var rep runReport
		data, err := os.ReadFile(filepath.Join(dir, "report.json"))
		if err != nil || json.Unmarshal(data, &rep) != nil {
			continue
		}
		kept = append(kept, keptRun{id: id, info: info, report: rep})
	}
	return kept
}

// goodRun returns the newest kept run whose report shows the target ok,
// or unchanged, or nothing. A build-only run's built proves no deploy, so it
// does not count; a dry run's unchanged does, since the host runs it.
func goodRun(kept []keptRun, t failedTarget) string {
	for _, run := range kept {
		for _, host := range run.report.Hosts {
			if host.Host != t.host {
				continue
			}
			status := host.System.Status
			if t.phase == "hm" {
				status = host.HomeManager.Status
			}
			if status == "ok" || status == "unchanged" {
				return run.id
			}
		}
	}
	return ""
}

// pathExists tells whether a store path exists; tests replace it.
var pathExists = fileExists

// readGit runs a read-only Git command in dir with a timeout and returns its
// output. Reading must neither take the index lock nor, in a partial clone,
// fetch a missing object into the repository.
func readGit(dir string, args ...string) ([]byte, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_NO_LAZY_FETCH=1")
	output, err := cmd.Output()
	return output, err == nil
}

// maxLockCommits bounds the commits the Git fallback reads.
const maxLockCommits = 50

// gitBaseline returns the newest commit at or before rev that changed the
// flake.lock in flakeDir to content other than lock, and that content. The
// flake directory may be below the root of its repository; Git reads the
// paths relative to it. A lock Git cannot read without fetching gives none.
func gitBaseline(flakeDir, rev string, lock []byte) (string, []byte) {
	if rev == "" {
		return "", nil
	}
	git := func(args ...string) ([]byte, bool) {
		return readGit(flakeDir, args...)
	}
	// Without a filter, Git's default history simplification keeps the
	// merges that changed the lock (a resolved conflict), which any
	// --diff-filter would drop.
	commits, ok := git("log", "--format=%H", fmt.Sprintf("--max-count=%d", maxLockCommits), rev, "--", "flake.lock")
	if !ok {
		return "", nil
	}
	for _, commit := range strings.Fields(string(commits)) {
		// The tree entry names the blob without reading it; ls-tree takes
		// the path relative to the flake directory.
		entry, ok := git("ls-tree", commit, "--", "./flake.lock")
		if !ok {
			return "", nil
		}
		fields := strings.Fields(string(entry))
		if len(fields) < 3 || fields[1] != "blob" {
			// The commit deleted the lock.
			continue
		}
		blob := fields[2]
		if _, ok := git("cat-file", "-e", blob); !ok {
			// A partial clone without this history ends the search.
			return "", nil
		}
		content, ok := git("cat-file", "blob", blob)
		if !ok {
			return "", nil
		}
		if !bytes.Equal(content, lock) {
			return commit, content
		}
	}
	return "", nil
}

// writeLockContext writes, for the failed targets, the lock each one last
// deployed well from and what changed since, grouped by baseline, and the
// store path of every input of the run's lock.
func writeLockContext(out *strings.Builder, dir string, info runInfo, failed []failedTarget, own map[string]bool, unlocked []string) {
	out.WriteString("\n## Lock diff\n\n")
	lock, err := os.ReadFile(filepath.Join(dir, "flake.lock"))
	if err != nil {
		out.WriteString("The run kept no flake.lock, so there is no lock diff and no input list.\n")
		return
	}
	inputs, err := lockInputs(lock)
	if err != nil {
		fmt.Fprintf(out, "The run's flake.lock cannot be read: %v\n", err)
		return
	}
	fmt.Fprintf(out, "The failed run's lock is %s, the one it evaluated.", filepath.Join(dir, "flake.lock"))
	if fileExists(filepath.Join(dir, "flake.start.lock")) {
		out.WriteString(" Nix rewrote the lock during evaluation; flake.start.lock is the one the run started with.")
	}
	out.WriteString("\n")

	// Group the targets by baseline, in order of appearance.
	kept := keptRuns(filepath.Dir(dir), info)
	var keys []string
	groups := map[string][]failedTarget{}
	for _, t := range failed {
		key := goodRun(kept, t)
		if groups[key] == nil {
			keys = append(keys, key)
		}
		groups[key] = append(groups[key], t)
	}
	for _, key := range keys {
		var names []string
		for _, t := range groups[key] {
			names = append(names, t.host+" "+t.label)
		}
		fmt.Fprintf(out, "\n### Since the baseline of %s\n\n", strings.Join(names, ", "))
		var base []byte
		if key != "" {
			path := filepath.Join(filepath.Dir(dir), key, "flake.lock")
			base, _ = os.ReadFile(path)
			fmt.Fprintf(out, "Baseline: %s, of run %s, the newest earlier run of this flake in which these targets were ok or unchanged.\n", path, key)
		} else if commit, content := gitBaseline(info.FlakeDir, info.GitRev, lock); commit != "" {
			base = content
			fmt.Fprintf(out, "Baseline: flake.lock at Git commit %s. No kept run shows these targets deployed, so this is an approximation: the newest earlier commit with another lock, not a known good deploy.\n", commit)
		} else {
			out.WriteString("No baseline: no kept run shows these targets deployed, and the Git history of flake.lock gave no earlier lock.\n")
			continue
		}
		old, err := lockInputs(base)
		if err != nil {
			fmt.Fprintf(out, "The baseline lock cannot be read: %v\n", err)
			continue
		}
		writeLockDiff(out, old, inputs)
	}

	writeLockedInputs(out, inputs, own, unlocked)
}

// writeLockedInputs writes the table of every input of the run's lock with
// its store path, marking the nodes of the operator's own inputs.
func writeLockedInputs(out *strings.Builder, inputs map[string]lockedInput, own map[string]bool, unlocked []string) {
	out.WriteString("\n## Locked inputs\n\nStore paths are computed from each input's narHash; \"in store\" says whether the path exists on this machine. \"Own\" marks the operator's own inputs, as the triage configuration lists them; every other input is upstream.")
	if len(own) == 0 && len(unlocked) == 0 {
		out.WriteString(" No own input is configured.")
	}
	if len(unlocked) > 0 {
		fmt.Fprintf(out, " Configured own inputs that are not in this lock (a misspelt name, or an input that follows another): %s; see \"Own inputs\".", strings.Join(unlocked, ", "))
	}
	out.WriteString("\n\n| Input | Own | Source | Store path | In store |\n|---|---|---|---|---|\n")
	for _, name := range sortedNames(inputs) {
		in := inputs[name]
		present := "unknown"
		if path := lockedStorePath(in.NarHash, in.storeName()); path != "" {
			present = "no"
			if pathExists(path) {
				present = "yes"
			}
		}
		mark := "no"
		if own[name] {
			mark = "yes"
		}
		fmt.Fprintf(out, "| %s | %s | %s | %s | %s |\n", name, mark, in.source(), in.storePath(), present)
	}
}

// writeLockDiff writes the inputs added, removed and changed from old to new.
func writeLockDiff(out *strings.Builder, old, new map[string]lockedInput) {
	var added, removed, changed []string
	for _, name := range sortedNames(new) {
		if _, ok := old[name]; !ok {
			added = append(added, name)
		} else if old[name].whole != new[name].whole {
			changed = append(changed, name)
		}
	}
	for _, name := range sortedNames(old) {
		if _, ok := new[name]; !ok {
			removed = append(removed, name)
		}
	}
	if len(added)+len(removed)+len(changed) == 0 {
		out.WriteString("\nThe lock is the same as the baseline.\n")
		return
	}
	for _, name := range added {
		in := new[name]
		fmt.Fprintf(out, "\n- Added `%s`: %s at %s (%s), %s\n", name, in.source(), in.identity(), in.date(), in.storePath())
	}
	for _, name := range removed {
		in := old[name]
		fmt.Fprintf(out, "\n- Removed `%s`: %s at %s (%s), %s\n", name, in.source(), in.identity(), in.date(), in.storePath())
	}
	for _, name := range changed {
		was, now := old[name], new[name]
		source := now.source()
		if was.source() != source {
			source = was.source() + " -> " + source
		}
		fmt.Fprintf(out, "\n- Changed `%s`: %s\n  - locked: %s -> %s\n  - last modified: %s -> %s\n  - store path: %s -> %s\n",
			name, source, was.identity(), now.identity(), was.date(), now.date(), was.storePath(), now.storePath())
	}
}
