package review

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// gitArgs prefixes every git call, so no repository hook can run.
var gitArgs = []string{"-c", "core.hooksPath=/dev/null"}

// Git runs git in repository and returns its standard output.
func Git(repository string, args ...string) (string, error) {
	cmd := exec.Command("git", append(append([]string{}, gitArgs...), args...)...)
	cmd.Dir = repository
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", args[0], lastLine(detail))
	}
	return string(out), nil
}

// ResolveCommit returns the full commit name of ref in repository.
func ResolveCommit(repository, ref string) (string, error) {
	out, err := Git(repository, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

type treeEntry struct {
	mode   string
	object string
	target string
}

// WriteTree writes the files of commit to destination from raw blobs, so no
// checkout filter, hook or attribute runs. Symbolic links become text files
// naming their target, and submodules and nested .git directories are
// skipped, so nothing in the copy points outside it. destination must not
// exist; the tree appears there only once it is complete.
func WriteTree(repository, commit, destination string) error {
	if _, err := os.Lstat(destination); err == nil {
		return fmt.Errorf("%s already exists", destination)
	}
	listing, err := Git(repository, "ls-tree", "-r", "-z", "--full-tree", commit)
	if err != nil {
		return err
	}
	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(parent, ".tree-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)

	var entries []treeEntry
	for _, record := range strings.Split(listing, "\x00") {
		if record == "" {
			continue
		}
		meta, path, ok := strings.Cut(record, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 {
			return fmt.Errorf("cannot read ls-tree record %q", record)
		}
		if fields[1] != "blob" || nestedGit(path) {
			continue
		}
		target := filepath.Join(staging, filepath.FromSlash(path))
		if !strings.HasPrefix(target, staging+string(filepath.Separator)) {
			return fmt.Errorf("refusing path outside the checkout: %q", path)
		}
		entries = append(entries, treeEntry{mode: fields[0], object: fields[2], target: target})
	}

	if err := writeBlobs(repository, entries); err != nil {
		return err
	}
	return os.Rename(staging, destination)
}

func nestedGit(path string) bool {
	for _, part := range strings.Split(path, "/") {
		if strings.EqualFold(part, ".git") {
			return true
		}
	}
	return false
}

func writeBlobs(repository string, entries []treeEntry) error {
	cmd := exec.Command("git", append(append([]string{}, gitArgs...), "cat-file", "--batch")...)
	cmd.Dir = repository
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	reader := bufio.NewReader(stdout)
	writeErr := func() error {
		for _, entry := range entries {
			if _, err := io.WriteString(stdin, entry.object+"\n"); err != nil {
				return err
			}
			header, err := reader.ReadString('\n')
			if err != nil {
				return fmt.Errorf("cannot read blob %s: %w", entry.object, err)
			}
			fields := strings.Fields(header)
			if len(fields) != 3 || fields[1] != "blob" {
				return fmt.Errorf("cannot read blob %s", entry.object)
			}
			size, err := strconv.ParseInt(fields[2], 10, 64)
			if err != nil {
				return fmt.Errorf("cannot read blob %s: %w", entry.object, err)
			}
			content := make([]byte, size)
			if _, err := io.ReadFull(reader, content); err != nil {
				return fmt.Errorf("cannot read blob %s: %w", entry.object, err)
			}
			if _, err := reader.Discard(1); err != nil {
				return fmt.Errorf("cannot read blob %s: %w", entry.object, err)
			}
			if entry.mode == "120000" {
				content = append(append([]byte("symbolic link to "), content...), '\n')
			}
			perm := os.FileMode(0o644)
			if entry.mode == "100755" {
				perm = 0o755
			}
			if err := os.MkdirAll(filepath.Dir(entry.target), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(entry.target, content, perm); err != nil {
				return err
			}
		}
		return nil
	}()
	stdin.Close()
	if writeErr != nil {
		// git may be blocked writing a blob nobody reads any more.
		cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	if writeErr != nil {
		return writeErr
	}
	if waitErr != nil {
		return fmt.Errorf("git cat-file: %w", waitErr)
	}
	return nil
}

func lastLine(text string) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	return lines[len(lines)-1]
}
