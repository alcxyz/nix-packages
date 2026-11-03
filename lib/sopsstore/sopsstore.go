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

const dns1123Label = `[a-z0-9]([-a-z0-9]*[a-z0-9])?`

var (
	secretKeyPattern = regexp.MustCompile(`^[-._a-zA-Z0-9]+$`)
	labelPattern     = regexp.MustCompile(`^` + dns1123Label + `$`)
	subdomainPattern = regexp.MustCompile(`^` + dns1123Label + `(\.` + dns1123Label + `)*$`)
)

// File is a new SOPS file holding one key under one name. Path is relative
// to the current directory, whose .sops.yaml creation rules apply to it.
// With Secret.Name set, the file is a Kubernetes Secret manifest holding
// the key under stringData; otherwise the key is the document's only entry.
type File struct {
	Path    string
	Key     string
	Secret  Secret
	Command string        // the sops program; "" means sops
	Timeout time.Duration // bounds each sops run; 0 means DefaultTimeout
}

// Secret names the Kubernetes Secret a File holds, when Name is set.
type Secret struct {
	Name      string
	Namespace string
}

// ValidSecretName checks a Secret's name, a DNS-1123 subdomain.
func ValidSecretName(name string) error {
	if len(name) > 253 || !subdomainPattern.MatchString(name) {
		return errors.New("must be a DNS-1123 subdomain: at most 253 lowercase letters, digits, - and ., starting and ending with a letter or digit")
	}
	return nil
}

// ValidNamespace checks a namespace's name, a DNS-1123 label.
func ValidNamespace(name string) error {
	if len(name) > 63 || !labelPattern.MatchString(name) {
		return errors.New("must be a DNS-1123 label: at most 63 lowercase letters, digits and -, starting and ending with a letter or digit")
	}
	return nil
}

// ValidSecretKey checks a key of a Secret's data as Kubernetes does: at
// most 253 of [-._a-zA-Z0-9], and not ".", ".." or starting with "..".
func ValidSecretKey(key string) error {
	if len(key) > 253 || !secretKeyPattern.MatchString(key) || key == "." || strings.HasPrefix(key, "..") {
		return errors.New("must be at most 253 letters, digits, -, _ and ., and neither . nor start with ..")
	}
	return nil
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

// Document is the YAML document holding the key. Names are quoted, so that
// a name such as 123 or true stays a string.
func (f File) Document(key string) string {
	var document strings.Builder
	indent := "    "
	if f.Secret.Name != "" {
		document.WriteString("apiVersion: v1\nkind: Secret\nmetadata:\n")
		document.WriteString("    name: " + strconv.Quote(f.Secret.Name) + "\n")
		document.WriteString("    namespace: " + strconv.Quote(f.Secret.Namespace) + "\n")
		document.WriteString("type: Opaque\nstringData:\n    ")
		indent = "        "
	}
	document.WriteString(strconv.Quote(f.Key) + ": |\n")
	for _, line := range strings.Split(strings.TrimSpace(key), "\n") {
		document.WriteString(indent + strings.TrimRight(line, "\r") + "\n")
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
	if f.Secret.Name != "" {
		return f.checkSecret(out)
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

// checkSecret checks that sops's output is the Secret manifest with the
// key's value encrypted under stringData and the rest readable, as an
// encrypted_regex of ^(data|stringData)$ leaves it.
func (f File) checkSecret(out []byte) error {
	top, children, err := yamlEntries(string(out))
	if err != nil {
		return err
	}
	for _, want := range [][2]string{{"apiVersion", "v1"}, {"kind", "Secret"}, {"type", "Opaque"}} {
		if top[want[0]] != want[1] {
			return fmt.Errorf("output has no readable %s: %s", want[0], want[1])
		}
	}
	for _, name := range []string{"metadata", "stringData", "sops"} {
		if value, ok := top[name]; !ok || value != "" {
			return fmt.Errorf("output has no %s mapping", name)
		}
	}
	if len(top) != 6 {
		return errors.New("output has unexpected top-level entries")
	}
	metadata := children["metadata"]
	if len(metadata) != 2 || metadata["name"] != f.Secret.Name || metadata["namespace"] != f.Secret.Namespace {
		return errors.New("output has no readable metadata name and namespace")
	}
	data := children["stringData"]
	value := data[f.Key]
	if len(data) != 1 || !strings.HasPrefix(value, "ENC[AES256_GCM,") || !strings.HasSuffix(value, "]") {
		return fmt.Errorf("output has no ENC[AES256_GCM, value for stringData.%s", f.Key)
	}
	return nil
}

// yamlEntries reads the top-level entries of a block-style YAML document,
// such as sops writes, and the direct children of each top-level mapping,
// with scalars unquoted. A mapping's own value is "". List items, comments
// and deeper levels are skipped; a repeated key is an error.
func yamlEntries(document string) (map[string]string, map[string]map[string]string, error) {
	top := map[string]string{}
	children := map[string]map[string]string{}
	section := ""
	childIndent := 0
	for _, line := range strings.Split(document, "\n") {
		text := strings.TrimLeft(line, " ")
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		indent := len(line) - len(text)
		key, value, ok := yamlEntry(text)
		if indent == 0 {
			section = ""
			if !ok {
				continue
			}
			if _, dup := top[key]; dup {
				return nil, nil, fmt.Errorf("output repeats %s", key)
			}
			top[key] = value
			section = key
			childIndent = 0
			continue
		}
		if section == "" {
			continue
		}
		if childIndent == 0 {
			childIndent = indent
		}
		if indent != childIndent || !ok {
			continue
		}
		if children[section] == nil {
			children[section] = map[string]string{}
		}
		if _, dup := children[section][key]; dup {
			return nil, nil, fmt.Errorf("output repeats %s.%s", section, key)
		}
		children[section][key] = value
	}
	return top, children, nil
}

// yamlEntry splits "key: value" or "key:", unquoting both.
func yamlEntry(text string) (string, string, bool) {
	if text == "-" || strings.HasPrefix(text, "- ") {
		return "", "", false
	}
	var key, value string
	if strings.HasSuffix(text, ":") {
		key = strings.TrimSuffix(text, ":")
	} else {
		i := strings.Index(text, ": ")
		if i < 0 {
			return "", "", false
		}
		key, value = text[:i], strings.TrimSpace(text[i+2:])
	}
	return yamlUnquote(key), yamlUnquote(value), true
}

func yamlUnquote(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		if unquoted, err := strconv.Unquote(s); err == nil {
			return unquoted
		}
	}
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		return strings.ReplaceAll(s[1:len(s)-1], "''", "'")
	}
	return s
}

// Describe names the file and key, for reporting where the key went.
func (f File) Describe() string {
	if f.Secret.Name != "" {
		return fmt.Sprintf("%s (Secret %s/%s, stringData key %s)", f.Path, f.Secret.Namespace, f.Secret.Name, f.Key)
	}
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
