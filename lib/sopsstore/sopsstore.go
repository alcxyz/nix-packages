// Package sopsstore stores one private key in a new SOPS file without
// showing it to anyone. It runs only `sops encrypt`, which needs the
// recipients' public keys alone, so an agent may run it.
package sopsstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// DefaultTimeout bounds sops when File.Timeout is zero.
const DefaultTimeout = 5 * time.Minute

// KeyPattern is what a key name in the SOPS document may look like.
var KeyPattern = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// File is a new SOPS file holding one key under one name. Path is relative
// to the current directory, whose .sops.yaml creation rules apply to it.
type File struct {
	Path    string
	Key     string
	Command string        // the sops program; "" means sops
	Timeout time.Duration // bounds each sops run; 0 means DefaultTimeout
}

// ValidPath checks the file name: sops takes the format from it, and the
// document is YAML.
func ValidPath(path string) error {
	ext := strings.ToLower(filepath.Ext(path))
	if ext != ".yaml" && ext != ".yml" {
		return errors.New("must end in .yaml or .yml")
	}
	return nil
}

func (f File) command() string {
	if f.Command == "" {
		return "sops"
	}
	return f.Command
}

func (f File) timeout() time.Duration {
	if f.Timeout == 0 {
		return DefaultTimeout
	}
	return f.Timeout
}

// Check runs before the key exists, so that a store that cannot work is
// found first. It checks that Path does not exist yet, that its directory
// does, that sops is installed, and that sops would encrypt the key under
// the creation rules for Path, by encrypting a synthetic key.
func (f File) Check(ctx context.Context) error {
	if _, err := os.Lstat(f.Path); err == nil {
		return fmt.Errorf("%s already exists; not overwriting it", f.Path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	dir := filepath.Dir(f.Path)
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return fmt.Errorf("the directory %s does not exist", dir)
	}
	if _, err := exec.LookPath(f.command()); err != nil {
		return fmt.Errorf("sops: %v", err)
	}

	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return err
	}
	marker := "sopsstorePreflight" + hex.EncodeToString(buf)
	synthetic := "-----BEGIN PREFLIGHT KEY-----\n" +
		marker + "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\n" +
		"-----END PREFLIGHT KEY-----\n"
	out, err := f.encrypt(ctx, synthetic)
	if err == nil && bytes.Contains(out, []byte(marker)) {
		err = errors.New("output contains the synthetic key")
	}
	if err == nil {
		err = f.checkEncrypted(out, synthetic)
	}
	if err != nil {
		return fmt.Errorf("sops would not encrypt %s in %s; check .sops.yaml creation rules (%v)", f.Key, f.Path, err)
	}
	return nil
}

// Document is the YAML document holding the key. The key name is quoted,
// so that a name such as 123 or true stays a string.
func (f File) Document(key string) string {
	var document strings.Builder
	document.WriteString(strconv.Quote(f.Key) + ": |\n")
	for _, line := range strings.Split(strings.TrimSpace(key), "\n") {
		document.WriteString("    " + strings.TrimRight(line, "\r") + "\n")
	}
	return document.String()
}

// encrypt runs sops encrypt on the key's document and returns its output.
// It runs in the current directory, so the repository's .sops.yaml
// creation rules apply to the file name.
func (f File) encrypt(ctx context.Context, key string) ([]byte, error) {
	var out bytes.Buffer
	timeout := f.timeout()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := BoundedCommand(ctx, f.command(), "encrypt", "--filename-override", f.Path, "/dev/stdin")
	cmd.Stdin = strings.NewReader(f.Document(key))
	cmd.Stdout = &out
	if err := RunError(ctx, "sops encrypt", timeout, cmd.Run()); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// minKeyLineCheck is the shortest key line looked for in sops's output;
// shorter ones could occur in ciphertext by chance.
const minKeyLineCheck = 16

// checkEncrypted checks that sops's output holds the key's value encrypted
// and none of the key's lines in plaintext.
func (f File) checkEncrypted(out []byte, key string) error {
	for _, line := range strings.Split(key, "\n") {
		line = strings.TrimSpace(line)
		if len(line) >= minKeyLineCheck && bytes.Contains(out, []byte(line)) {
			return errors.New("output contains the key in plaintext")
		}
	}
	quoted := strconv.Quote(f.Key) + ":"
	plain := f.Key + ":"
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.HasPrefix(line, quoted) && !strings.HasPrefix(line, plain) {
			continue
		}
		if strings.Contains(line, "ENC[AES256_GCM,") {
			return nil
		}
	}
	return fmt.Errorf("output has no ENC[AES256_GCM, value for %s", f.Key)
}

// Describe names the file and key, for reporting where the key went.
func (f File) Describe() string {
	return fmt.Sprintf("%s (SOPS key %s)", f.Path, f.Key)
}

// Save encrypts the key and writes it to Path, which must still not
// exist. Neither the key nor sops's output is ever reported.
func (f File) Save(ctx context.Context, key string) error {
	encrypted, err := f.encrypt(ctx, key)
	if err != nil {
		return err
	}
	if err := f.checkEncrypted(encrypted, key); err != nil {
		return fmt.Errorf("sops did not encrypt the key (%v); nothing was written to %s", err, f.Path)
	}
	file, err := os.OpenFile(f.Path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(encrypted); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

// BoundedCommand runs a program in its own process group. When ctx ends
// (a time limit, or an interrupt), the whole group is killed, so a child
// such as gpg under pass cannot finish writing later, and Run waits at most
// a few seconds more for the output pipes to close. The group is a
// background group of the terminal, so a program that prompts there stops
// until the time limit kills it.
func BoundedCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

// RunError describes the error of a BoundedCommand run under a timeout,
// without its output.
func RunError(ctx context.Context, what string, timeout time.Duration, err error) error {
	if err == nil {
		return nil
	}
	// A killed group exits with -1; say why it was stopped.
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%s was stopped after %v", what, timeout)
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return fmt.Errorf("%s was stopped by an interrupt", what)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return fmt.Errorf("%s failed with exit status %d (output withheld)", what, exitErr.ExitCode())
	}
	return fmt.Errorf("%s failed: %v", what, err)
}
