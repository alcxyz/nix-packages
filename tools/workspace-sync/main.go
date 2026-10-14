package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

const usage = `workspace-sync [--status | --directories]

Create missing workspace directories, clone missing repositories, and add missing
relative links to existing canonical targets. Existing paths are preserved.
--status reports the layout without changing it; --directories creates only
configured directories (used during Home Manager activation).
`

// Keep the original jq predicate: its stream, null, and regular-expression
// semantics are part of the accepted shell contract, including extra fields.
const validation = `
  def safe_path:
    type == "string" and length > 0 and
    (split("/") | all(.[]; . != "." and . != ".." and test("^[A-Za-z0-9._-]+$")));
  (.directories | type == "array" and all(.[]; safe_path)) and
  (.repositories | type == "array" and all(.[];
    (.path | safe_path) and (.url | type == "string" and length > 0) and
    (.branch == null or (.branch | type == "string")))) and
  (.links | type == "array" and all(.[];
    (.path | safe_path) and (.target | safe_path) and
    (.profiles | type == "array" and all(.[]; type == "string"))))
`

type repository struct {
	path, url, branch string
}

type link struct {
	path, canonical string
}

type counters struct {
	existing, cloned, missing, skipped, failed              int
	linksExisting, linksCreated, linksMissing, linksSkipped int
	failures                                                []string
}

type workspace struct {
	root, manifest, mode string
	out, err             io.Writer
	counts               counters
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, out, err io.Writer) int {
	if len(args) < 2 {
		fmt.Fprintf(err, "%s: line %d: $%d: unbound variable\n", os.Args[0], 4+len(args), 1+len(args))
		return 1
	}
	w := workspace{root: args[0], manifest: args[1], mode: "sync", out: out, err: err}
	if len(args) > 2 {
		switch args[2] {
		case "--status":
			w.mode = "status"
		case "--directories":
			w.mode = "directories"
		case "--help", "-h":
			return writef(out, usage)
		case "":
		default:
			if writef(err, "workspace-sync: unknown argument: %s\n", args[2]) != 0 {
				return 1
			}
			return 2
		}
	}
	if w.command("jq", []string{"-e", validation, w.manifest}, os.Stdin, io.Discard, w.err) != 0 {
		if writef(err, "workspace-sync: invalid manifest\n") != 0 {
			return 1
		}
		return 2
	}
	if !w.prepareDirectory(".") {
		return 1
	}
	directoryFailures := 0
	for _, dir := range w.entries("-r", ".directories[]") {
		if !w.prepareDirectory(dir) {
			directoryFailures++
		}
	}
	if w.mode == "directories" {
		if directoryFailures > 0 {
			fmt.Fprintf(err, "workspace-sync: %d directory conflicts or failures\n", directoryFailures)
			return 1
		}
		return 0
	}

	for _, entry := range w.entries("-c", ".repositories[]") {
		r, code := w.repository(entry)
		if code != 0 {
			return code
		}
		if code = w.reconcileRepository(r); code != 0 {
			return code
		}
	}
	for _, entry := range w.entries("-c", ".links[]") {
		l, code := w.link(entry)
		if code != 0 {
			return code
		}
		if code = w.reconcileLink(l); code != 0 {
			return code
		}
	}
	c := &w.counts
	if w.mode == "status" {
		return writef(out, "\nworkspace-sync status: %d existing, %d missing, %d skipped; %d links existing, %d links missing, %d links skipped\n", c.existing, c.missing, c.skipped, c.linksExisting, c.linksMissing, c.linksSkipped)
	}
	if writef(out, "\nworkspace-sync summary: %d existing, %d cloned, %d skipped, %d failed; %d links existing, %d links created, %d links missing, %d links skipped\n", c.existing, c.cloned, c.skipped, c.failed, c.linksExisting, c.linksCreated, c.linksMissing, c.linksSkipped) != 0 {
		return 1
	}
	if c.failed > 0 {
		if writef(err, "workspace-sync failed repositories:\n") != 0 {
			return 1
		}
		for _, failure := range c.failures {
			if writef(err, "  - %s\n", failure) != 0 {
				return 1
			}
		}
		return 1
	}
	return 0
}

func writef(out io.Writer, format string, args ...interface{}) int {
	if _, err := fmt.Fprintf(out, format, args...); err != nil {
		return 1
	}
	return 0
}

func (w *workspace) command(name string, args []string, in io.Reader, out, err io.Writer) int {
	cmd := exec.Command(name, args...)
	// The shell baseline accepts relative PATH entries, including ".".
	if errors.Is(cmd.Err, exec.ErrDot) {
		cmd.Err = nil
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, out, err
	e := cmd.Run()
	if errors.Is(e, syscall.ENOEXEC) {
		// Bash's executable-file fallback is part of the existing PATH
		// contract. Run that file with argument arrays, never a command string.
		fallback := exec.Command("bash", append([]string{"--", cmd.Path}, args...)...)
		if errors.Is(fallback.Err, exec.ErrDot) {
			fallback.Err = nil
		}
		fallback.Stdin, fallback.Stdout, fallback.Stderr = in, out, err
		e = fallback.Run()
	}
	if e != nil {
		if status, ok := e.(*exec.ExitError); ok {
			if status.ExitCode() >= 0 {
				return status.ExitCode()
			}
			if wait, ok := status.Sys().(syscall.WaitStatus); ok && wait.Signaled() {
				return 128 + int(wait.Signal())
			}
			return 1
		}
		fmt.Fprintf(err, "%s: %v\n", name, e)
		// Keep the approved Bash 5.2 status for both a missing command and
		// an executable whose interpreter is missing (Bash 5.3 uses 126).
		if os.IsNotExist(e) || errors.Is(e, exec.ErrNotFound) {
			return 127
		}
		return 126
	}
	return 0
}

// Bash command substitutions remove NUL bytes and all trailing newlines.
func substitution(s string) string {
	return strings.TrimRight(strings.ReplaceAll(s, "\x00", ""), "\n")
}

func (w *workspace) capture(name string, args []string, in io.Reader) (string, int) {
	var out bytes.Buffer
	code := w.command(name, args, in, &out, w.err)
	return substitution(out.String()), code
}

// Process-substitution producers in the shell can fail after yielding entries;
// their failure does not abort the consuming loop. Preserve that distinction.
func (w *workspace) entries(format, query string) []string {
	var out bytes.Buffer
	w.command("jq", []string{format, query, w.manifest}, os.Stdin, &out, w.err)
	text := strings.ReplaceAll(out.String(), "\x00", "")
	if text == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n")
}

func (w *workspace) repository(entry string) (repository, int) {
	var r repository
	for i, query := range []string{".path", ".url", ".branch // empty"} {
		value, status := w.capture("jq", []string{"-r", query}, strings.NewReader(entry+"\n"))
		if status != 0 {
			return r, status
		}
		switch i {
		case 0:
			r.path = value
		case 1:
			r.url = value
		case 2:
			r.branch = value
		}
	}
	return r, 0
}

func (w *workspace) link(entry string) (link, int) {
	path, code := w.capture("jq", []string{"-r", ".path"}, strings.NewReader(entry+"\n"))
	if code != 0 {
		return link{}, code
	}
	canonical, code := w.capture("jq", []string{"-r", ".target"}, strings.NewReader(entry+"\n"))
	return link{path: path, canonical: canonical}, code
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func symlink(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode()&os.ModeSymlink != 0
}

func directory(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func (w *workspace) prepareDirectory(relative string) bool {
	current := w.root
	if !w.prepareComponent(current, true) {
		return false
	}
	if w.mode == "status" && !directory(current) {
		return true
	}
	if relative == "." {
		return true
	}
	// read -r consumes only the first line and discards a trailing IFS slash.
	parts := strings.Split(strings.SplitN(relative, "\n", 2)[0], "/")
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	for _, part := range parts {
		current += "/" + part
		if !w.prepareComponent(current, false) {
			return false
		}
		if w.mode == "status" && !directory(current) {
			return true
		}
	}
	return true
}

func (w *workspace) prepareComponent(path string, root bool) bool {
	if symlink(path) || (exists(path) && !directory(path)) {
		fmt.Fprintf(w.err, "conflict: %s is not a plain directory\n", path)
		return false
	}
	if directory(path) || w.mode == "status" {
		return true
	}
	var err error
	if root {
		err = os.MkdirAll(path, 0777)
	} else {
		err = os.Mkdir(path, 0777)
	}
	if err != nil {
		fmt.Fprintf(w.err, "failed: could not create %s\n", path)
		return false
	}
	return true
}

func parent(path string) string {
	if slash := strings.LastIndexByte(path, '/'); slash >= 0 {
		return path[:slash]
	}
	return "."
}

func (w *workspace) reconcileRepository(r repository) int {
	c := &w.counts
	target := w.root + "/" + r.path
	if !w.prepareDirectory(parent(r.path)) {
		c.skipped++
		return 0
	}
	if symlink(target) {
		if writef(w.err, "conflict: %s is a symlink\n", target) != 0 {
			return 1
		}
		c.skipped++
		return 0
	}
	if exists(target+"/.git") && w.command("git", []string{"-C", target, "rev-parse", "--git-dir"}, nil, io.Discard, io.Discard) == 0 {
		if writef(w.out, "exists: %s\n", target) != 0 {
			return 1
		}
		c.existing++
		return 0
	}
	if exists(target) {
		if writef(w.err, "conflict: %s exists but is not a git repository\n", target) != 0 {
			return 1
		}
		c.skipped++
		return 0
	}
	if w.mode == "status" {
		if writef(w.out, "missing: %s -> %s\n", target, r.url) != 0 {
			return 1
		}
		c.missing++
		return 0
	}
	args := []string{"clone"}
	failure := r.path + " -> " + r.url
	if r.branch != "" {
		args = append(args, "--branch", r.branch)
		failure += " (branch: " + r.branch + ")"
	}
	args = append(args, r.url, target)
	if w.command("git", args, nil, w.out, w.err) == 0 {
		c.cloned++
		return 0
	}
	message := fmt.Sprintf("failed: %s -> %s", target, r.url)
	if r.branch != "" {
		message += " (branch: " + r.branch + ")"
	}
	if writef(w.err, "%s\n", message) != 0 {
		return 1
	}
	c.failed++
	c.failures = append(c.failures, failure)
	return 0
}

func (w *workspace) reconcileLink(l link) int {
	c := &w.counts
	target := w.root + "/" + l.path
	if !w.prepareDirectory(parent(l.path)) {
		c.linksSkipped++
		return 0
	}
	// Keep coreutils canonicalization and ln exit statuses. They also preserve
	// realpath -m behavior for missing components and symlink targets on Darwin.
	dir, _ := w.capture("dirname", []string{target}, nil)
	relative, code := w.capture("realpath", []string{"-m", "--relative-to=" + dir, w.root + "/" + l.canonical}, nil)
	if code != 0 {
		return code
	}
	if symlink(target) {
		value, _ := w.capture("readlink", []string{"--", target}, nil)
		if value == relative && exists(target) {
			if writef(w.out, "link exists: %s -> %s\n", target, relative) != 0 {
				return 1
			}
			c.linksExisting++
		} else {
			if writef(w.err, "conflict: %s is an existing symlink\n", target) != 0 {
				return 1
			}
			c.linksSkipped++
		}
		return 0
	}
	if exists(target) {
		if writef(w.err, "conflict: %s already exists\n", target) != 0 {
			return 1
		}
		c.linksSkipped++
		return 0
	}
	if !exists(w.root + "/" + l.canonical) {
		if writef(w.out, "link target missing: %s -> %s\n", target, l.canonical) != 0 {
			return 1
		}
		c.linksMissing++
		return 0
	}
	resolvedRoot, code := w.capture("realpath", []string{"-e", "--", w.root}, nil)
	if code != 0 {
		return code
	}
	resolvedTarget, code := w.capture("realpath", []string{"-e", "--", w.root + "/" + l.canonical}, nil)
	if code != 0 {
		return code
	}
	if !strings.HasPrefix(resolvedTarget, resolvedRoot+"/") {
		if writef(w.err, "conflict: %s resolves outside workspace\n", w.root+"/"+l.canonical) != 0 {
			return 1
		}
		c.linksSkipped++
		return 0
	}
	if w.mode == "status" {
		if writef(w.out, "link missing: %s -> %s\n", target, relative) != 0 {
			return 1
		}
		c.linksMissing++
		return 0
	}
	if code = w.command("ln", []string{"-s", "--", relative, target}, nil, w.out, w.err); code != 0 {
		return code
	}
	if writef(w.out, "link created: %s -> %s\n", target, relative) != 0 {
		return 1
	}
	c.linksCreated++
	return 0
}
