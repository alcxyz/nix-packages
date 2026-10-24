package main

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeSSH plays Forgejo's SSH server. Each connection without a command
// takes the next line of $state/modes (the last one repeats):
//
//	denied     the key is not registered
//	hostkey    the host key is unknown
//	transient  the connection fails
//	hopdenied  a proxy's own ssh is refused
//	user       the key belongs to a user account
//	readonly   a deploy key without write access
//	otherrepo  a deploy key of another repository
//	slow       a deploy key with write access, after two seconds
//	write      a deploy key with write access
//
// Every invocation's arguments are appended to $state/ssh-args.
const fakeSSH = `#!/bin/sh
state=$FAKE_STATE
printf '%s\n' "$@" >> "$state/ssh-args"
printf '=end=\n' >> "$state/ssh-args"
if [ "$1" = -G ]; then
	printf 'user git\nhostname resolved.example.org\nport 2222\n'
	cat "$state/ssh-config" 2>/dev/null
	printf 'userknownhostsfile /k/one /k/two\nglobalknownhostsfile /k/global\n'
	exit 0
fi
user=; prev=
for last; do
	[ "$prev" = -l ] && user=$last
	prev=$last
done
case "$last" in
git-receive-pack*)
	cat > "$state/receive-pack-stdin"
	mode=$(cat "$state/mode")
	if [ "$mode" = readonly ]; then
		echo >&2; echo "Forgejo: Deploy Key: 7:publisher is not authorized to write owner/site." >&2; exit 1
	fi
	if [ "$mode" = otherrepo ]; then
		echo >&2; echo "Forgejo: Public (Deploy) Key: 7:publisher is not authorized to write owner/site." >&2; exit 1
	fi
	printf '00a0capabilities^{}\n'
	exit 0;;
esac
mode=$(head -n 1 "$state/modes")
if [ "$(wc -l < "$state/modes")" -gt 1 ]; then
	sed -i '1d' "$state/modes"
fi
echo "$mode" > "$state/mode"
[ -s "$state/agent-key" ] || mode=denied
case "$mode" in
denied) echo "$user@resolved.example.org: Permission denied (publickey)." >&2; exit 255;;
hopdenied) printf 'ops@bastion.example.org: Permission denied (publickey).\nConnection closed by UNKNOWN port 65535\n' >&2; exit 255;;
slow) sleep 2; echo "Hi there! You've successfully authenticated with the deploy key named publisher, but Forgejo does not provide shell access."; exit 0;;
hostkey) echo "Host key verification failed." >&2; exit 255;;
transient) echo "ssh: connect to host x port 22: Connection refused" >&2; exit 255;;
user) echo "Hi there, alice! You've successfully authenticated with the key named laptop, but Forgejo does not provide shell access."; exit 0;;
*) echo "Hi there! You've successfully authenticated with the deploy key named publisher, but Forgejo does not provide shell access."; exit 0;;
esac
`

const fakeSSHAgent = `#!/bin/sh
printf '%s\n' "$@" > "$FAKE_STATE/agent-args"
: > "$3"
exec sleep 600
`

const fakeSSHAdd = `#!/bin/sh
printf '%s\n' "$@" > "$FAKE_STATE/add-args"
printf '%s\n' "$SSH_AUTH_SOCK" > "$FAKE_STATE/add-sock"
cat > "$FAKE_STATE/agent-key"
`

// fakeSops encrypts like gh-app's fake: it replaces the key's value with an
// ENC[...] value. With $state/sops-fails-real it fails on any input but the
// preflight's synthetic key.
const fakeSops = `#!/bin/sh
state=$FAKE_STATE
printf '%s\n' "$@" >> "$state/sops-args"
cat > "$state/sops-stdin"
if [ -f "$state/sops-fails" ]; then exit 1; fi
if [ -f "$state/sops-plaintext" ]; then cat "$state/sops-stdin"; exit 0; fi
if [ -f "$state/sops-fails-real" ] && ! grep -q PREFLIGHT "$state/sops-stdin"; then exit 1; fi
key=$(head -n 1 "$state/sops-stdin" | sed 's/: |$//')
printf '%s: ENC[AES256_GCM,data:Zm9vYmFy,iv:aXY=,tag:dGFn,type:str]\nsops:\n    version: 3.9.0\n' "$key"
`

type fixture struct {
	state string // the fakes' own records, outside the checked directories
	work  string // the current directory, holding the SOPS target
	tmp   string // TMPDIR, where the agent's directory goes
}

func setup(t *testing.T, modes string) fixture {
	t.Helper()
	f := fixture{state: t.TempDir(), work: t.TempDir(), tmp: t.TempDir()}
	bin := t.TempDir()
	for name, script := range map[string]string{
		"ssh": fakeSSH, "ssh-agent": fakeSSHAgent, "ssh-add": fakeSSHAdd, "sops": fakeSops,
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(f.state, "modes"), []byte(modes), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(f.work, "keys"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_STATE", f.state)
	t.Setenv("TMPDIR", f.tmp)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(f.work); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(wd) })

	oldInterval := pollInterval
	pollInterval = 10 * time.Millisecond
	t.Cleanup(func() { pollInterval = oldInterval })
	return f
}

func (f fixture) read(name string) string {
	data, _ := os.ReadFile(filepath.Join(f.state, name))
	return string(data)
}

func (f fixture) exists(name string) bool {
	_, err := os.Stat(filepath.Join(f.state, name))
	return err == nil
}

func baseArgs(extra ...string) []string {
	return append([]string{
		"--repo", "owner/site",
		"--forgejo", "https://forgejo.example.org/",
		"--sops-file", "keys/site-deploy-key.yaml",
		"--sops-key", "site_deploy_key",
	}, extra...)
}

func runTool(t *testing.T, args []string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// assertKeyHidden checks that no line of the private key appears in the
// output, in any program's arguments, or in any file under the working
// directory or TMPDIR.
func assertKeyHidden(t *testing.T, f fixture, private string, outputs ...string) {
	t.Helper()
	var lines []string
	for _, line := range strings.Split(private, "\n") {
		if strings.Contains(line, "-----") || len(line) < 16 {
			continue
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		t.Fatal("no private key lines to look for")
	}
	haystacks := map[string]string{}
	for i, out := range outputs {
		haystacks["output "+string(rune('0'+i))] = out
	}
	for _, name := range []string{"ssh-args", "agent-args", "add-args", "sops-args"} {
		haystacks[name] = f.read(name)
	}
	for _, dir := range []string{f.work, f.tmp} {
		filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err == nil && d.Type().IsRegular() {
				data, _ := os.ReadFile(path)
				haystacks[path] = string(data)
			}
			return nil
		})
	}
	for where, text := range haystacks {
		for _, line := range lines {
			if strings.Contains(text, line) {
				t.Errorf("the private key appears in %s", where)
				break
			}
		}
	}
}

func TestProvisionStoresKey(t *testing.T) {
	f := setup(t, "denied\ndenied\nreadonly\nwrite\n")
	code, stdout, stderr := runTool(t, baseArgs())
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}

	private := f.read("agent-key")
	if !strings.HasPrefix(private, "-----BEGIN OPENSSH PRIVATE KEY-----\n") {
		t.Fatalf("the agent got %d bytes that are not an OpenSSH private key", len(private))
	}
	assertKeyHidden(t, f, private, stdout, stderr)

	// The same key went to the agent and to sops.
	document := f.read("sops-stdin")
	if !strings.HasPrefix(document, `"site_deploy_key": |`+"\n") {
		t.Errorf("sops document starts %q", strings.SplitN(document, "\n", 2)[0])
	}
	for _, line := range strings.Split(strings.TrimSpace(private), "\n") {
		if !strings.Contains(document, "    "+line+"\n") {
			t.Fatal("sops did not get the agent's key")
		}
	}
	written, err := os.ReadFile(filepath.Join(f.work, "keys", "site-deploy-key.yaml"))
	if err != nil || !strings.HasPrefix(string(written), `"site_deploy_key": ENC[AES256_GCM,`) {
		t.Errorf("target = %q, %v", written, err)
	}

	for _, want := range []string{
		"public key:   ssh-ed25519 AAAA",
		" site-deploy-key\n",
		"fingerprint:  SHA256:",
		"https://forgejo.example.org/owner/site/settings/keys",
		`deploy key "publisher" has no write access (Forgejo: Deploy Key: 7:publisher is not authorized to write owner/site.)`,
		`verified:     deploy key "publisher" can push to owner/site`,
		"key:          stored in keys/site-deploy-key.yaml (SOPS key site_deploy_key)",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	if stderr != "" {
		t.Errorf("stderr = %q", stderr)
	}
	if got := f.read("receive-pack-stdin"); got != "0000" {
		t.Errorf("git-receive-pack got %q, want only a flush packet", got)
	}
	if entries, _ := os.ReadDir(f.tmp); len(entries) != 0 {
		t.Errorf("TMPDIR still holds %v", entries)
	}
	if got := f.read("add-args"); !strings.HasPrefix(got, "-q\n-t\n") || !strings.HasSuffix(got, "\n-\n") {
		t.Errorf("ssh-add args = %q", got)
	}
}

func TestSSHUsesOnlyTheNewKeyAndKnownHosts(t *testing.T) {
	f := setup(t, "denied\nwrite\n")
	if code, stdout, stderr := runTool(t, baseArgs("--ssh-host", "alias.example.org")); code != 0 {
		t.Fatalf("exit %d\n%s%s", code, stdout, stderr)
	}
	calls := strings.Split(strings.TrimSuffix(f.read("ssh-args"), "=end=\n"), "=end=\n")
	if len(calls) < 4 {
		t.Fatalf("ssh calls = %q", calls)
	}
	if calls[0] != "-G\n-l\ngit\nalias.example.org\n" {
		t.Errorf("ssh -G call = %q", calls[0])
	}
	sock := strings.TrimSpace(f.read("add-sock"))
	for _, call := range calls[1:] {
		for _, want := range []string{
			"-F\n/dev/null\n",
			"IdentityAgent=\"" + sock + "\"\n",
			"IdentitiesOnly=yes\n",
			"StrictHostKeyChecking=yes\n",
			"UserKnownHostsFile=/k/one /k/two\n",
			"GlobalKnownHostsFile=/k/global\n",
			"HostName=resolved.example.org\n-p\n2222\n-l\ngit\n--\nalias.example.org\n",
		} {
			if !strings.Contains(call, want) {
				t.Errorf("ssh call lacks %q:\n%s", want, call)
			}
		}
	}
	if last := calls[len(calls)-1]; !strings.HasSuffix(last, "alias.example.org\ngit-receive-pack 'owner/site.git'\n") {
		t.Errorf("last ssh call = %q", last)
	}
}

func TestTransportFromSSHConfig(t *testing.T) {
	f := setup(t, "denied\nwrite\n")
	config := "proxycommand cloud-proxy %h\nhostkeyalias forge-alias\n"
	if err := os.WriteFile(filepath.Join(f.state, "ssh-config"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	known := filepath.Join(t.TempDir(), "known_hosts")
	os.WriteFile(known, nil, 0o644)
	t.Setenv("SSH_AUTH_SOCK", "/user/agent.sock")
	t.Setenv("SHELL", "/bin/zsh")
	if code, stdout, stderr := runTool(t, baseArgs("--known-hosts", known, "--ssh-port", "2200", "--ssh-user", "forge")); code != 0 {
		t.Fatalf("exit %d\n%s%s", code, stdout, stderr)
	}
	calls := strings.Split(f.read("ssh-args"), "=end=\n")
	if calls[0] != "-G\n-l\nforge\n-p\n2200\nforgejo.example.org\n" {
		t.Errorf("ssh -G call = %q", calls[0])
	}
	for _, want := range []string{
		"ProxyCommand=env SSH_AUTH_SOCK='/user/agent.sock' '/bin/zsh' -c 'cloud-proxy %h'\n",
		"HostKeyAlias=forge-alias\n",
		"UserKnownHostsFile=\"" + known + "\"\n",
		"GlobalKnownHostsFile=/dev/null\n",
	} {
		if !strings.Contains(calls[1], want) {
			t.Errorf("ssh call lacks %q:\n%s", want, calls[1])
		}
	}
	if strings.Contains(calls[1], "/k/one") {
		t.Error("--known-hosts did not replace the configured files")
	}
}

// Paths with spaces, quotes and % reach ssh as single, literal values: the
// real ssh parses the options as intended.
func TestSSHArgsParseInRealSSH(t *testing.T) {
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("ssh is not installed")
	}
	dir := filepath.Join(t.TempDir(), `a b"c\d`)
	known := filepath.Join(dir, "known hosts")
	tr := transport{alias: "alias.example.org", host: "resolved.example.org", port: "2222", user: "git",
		hostKeyOpts: []string{"-o", "UserKnownHostsFile=" + optionValue(known)}}
	a := &agent{dir: dir, socket: filepath.Join(dir, "agent.sock")}
	out, err := exec.Command(ssh, append([]string{"-G"}, tr.sshArgs(a)...)...).Output()
	if err != nil {
		t.Fatalf("ssh -G: %v", err)
	}
	for _, want := range []string{
		"hostname resolved.example.org\n",
		"port 2222\n",
		"userknownhostsfile " + known + "\n",
		"identityagent " + a.socket + "\n",
		"identityfile " + a.publicKeyFile() + "\n",
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("ssh -G lacks %q:\n%s", want, out)
		}
	}
	if got := optionValue("/x/100%"); got != `"/x/100%%"` {
		t.Errorf("optionValue = %s", got)
	}
}

// Refusals before the key exists: no agent is started and nothing is
// printed or written.
func TestRefusesBeforeCreatingTheKey(t *testing.T) {
	for _, tt := range []struct {
		name  string
		args  []string
		setup func(f fixture)
		want  string
	}{
		{"existing target", baseArgs(), func(f fixture) {
			os.WriteFile(filepath.Join(f.work, "keys", "site-deploy-key.yaml"), nil, 0o600)
		}, "already exists; not overwriting it"},
		{"missing directory", baseArgs(), func(f fixture) {
			os.Remove(filepath.Join(f.work, "keys"))
		}, "the directory keys does not exist"},
		{"no creation rule", baseArgs(), func(f fixture) {
			os.WriteFile(filepath.Join(f.state, "sops-fails"), nil, 0o644)
		}, "sops would not encrypt site_deploy_key in keys/site-deploy-key.yaml; check .sops.yaml creation rules"},
		{"plaintext sops output", baseArgs(), func(f fixture) {
			os.WriteFile(filepath.Join(f.state, "sops-plaintext"), nil, 0o644)
		}, "check .sops.yaml creation rules"},
		{"missing known hosts", baseArgs("--known-hosts", "/nonexistent/known_hosts"), nil, "--known-hosts"},
		{"unknown host key", baseArgs(), func(f fixture) {
			os.WriteFile(filepath.Join(f.state, "modes"), []byte("hostkey\n"), 0o644)
		}, "the SSH host key of forgejo.example.org is not known or does not match (Host key verification failed.)"},
		{"unreachable server", baseArgs(), func(f fixture) {
			os.WriteFile(filepath.Join(f.state, "modes"), []byte("transient\n"), 0o644)
		}, "could not check the key with forgejo.example.org: ssh: connect to host"},
		{"proxy refused", baseArgs(), func(f fixture) {
			os.WriteFile(filepath.Join(f.state, "modes"), []byte("hopdenied\n"), 0o644)
		}, "could not check the key with forgejo.example.org: Connection closed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := setup(t, "denied\nwrite\n")
			if tt.setup != nil {
				tt.setup(f)
			}
			code, stdout, stderr := runTool(t, tt.args)
			if code != 1 || !strings.Contains(stderr, tt.want) {
				t.Errorf("exit %d, stderr %q, want %q", code, stderr, tt.want)
			}
			if stdout != "" {
				t.Errorf("stdout = %q", stdout)
			}
			if f.exists("agent-key") && tt.name != "unknown host key" && tt.name != "unreachable server" && tt.name != "proxy refused" {
				t.Error("a key was created")
			}
			if entries, _ := os.ReadDir(f.tmp); len(entries) != 0 {
				t.Errorf("TMPDIR still holds %v", entries)
			}
		})
	}
}

func TestFailuresAfterTheKeyIsShown(t *testing.T) {
	for _, tt := range []struct {
		name  string
		modes string
		args  []string
		setup func(f fixture)
		want  []string
	}{
		{"user key", "denied\nuser\n", baseArgs(), nil,
			[]string{"added to a user account", "nothing was stored", "delete it (SHA256:"}},
		{"host key changes", "denied\nhostkey\n", baseArgs(), nil,
			[]string{"is not known or does not match", "nothing was stored"}},
		{"timeout", "denied\n", baseArgs("--wait", "200ms"), nil,
			[]string{"timed out after 200ms", "nothing was stored", "delete it (SHA256:"}},
		{"probe outlasts the wait", "denied\nslow\n", baseArgs("--wait", "300ms"), nil,
			[]string{"timed out after 300ms", "nothing was stored"}},
		{"store fails", "denied\nwrite\n", baseArgs(), func(f fixture) {
			os.WriteFile(filepath.Join(f.state, "sops-fails-real"), nil, 0o644)
		}, []string{"the deploy key works, but storing its private key failed: sops encrypt failed with exit status 1 (output withheld)", "delete the deploy key (SHA256:"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := setup(t, tt.modes)
			if tt.setup != nil {
				tt.setup(f)
			}
			code, stdout, stderr := runTool(t, tt.args)
			if code != 1 {
				t.Errorf("exit %d", code)
			}
			for _, want := range tt.want {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr %q lacks %q", stderr, want)
				}
			}
			if !strings.Contains(stdout, "public key:   ssh-ed25519 ") {
				t.Errorf("stdout = %q", stdout)
			}
			if _, err := os.Stat(filepath.Join(f.work, "keys", "site-deploy-key.yaml")); err == nil {
				t.Error("the target was written")
			}
			assertKeyHidden(t, f, f.read("agent-key"), stdout, stderr)
			if entries, _ := os.ReadDir(f.tmp); len(entries) != 0 {
				t.Errorf("TMPDIR still holds %v", entries)
			}
		})
	}
}

func TestParseArgs(t *testing.T) {
	opts, err := parseArgs(baseArgs())
	if err != nil {
		t.Fatal(err)
	}
	if opts.title != "site-deploy-key" || opts.sshHost != "forgejo.example.org" || opts.forgejo != "https://forgejo.example.org" ||
		opts.sshUser != "git" || opts.wait != 30*time.Minute {
		t.Errorf("defaults = %+v", opts)
	}

	replace := func(flag, value string) []string {
		args := baseArgs()
		for i := range args {
			if args[i] == flag {
				args[i+1] = value
			}
		}
		return args
	}
	for _, args := range [][]string{
		replace("--repo", "owner/site'; touch x; '"),
		replace("--repo", "owner/site.git"),
		replace("--repo", "owner"),
		replace("--repo", "../site"),
		replace("--forgejo", "forgejo.example.org"),
		replace("--forgejo", "https://user@forgejo.example.org"),
		replace("--sops-file", "keys/site.json"),
		replace("--sops-key", "site-key"),
		baseArgs("--ssh-host", "-oProxyCommand=x"),
		baseArgs("--ssh-user", "-x"),
		baseArgs("--title", "line\nbreak"),
		baseArgs("--ssh-port", "70000"),
		baseArgs("--wait", "0s"),
		baseArgs("extra"),
	} {
		if _, err := parseArgs(args); err == nil {
			t.Errorf("accepted %q", args)
		}
	}
}

// The private key is in the format ssh-keygen reads, and the printed
// public key and fingerprint are its own.
func TestOpenSSHPrivateKeyFormat(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen is not installed")
	}
	key, err := newDeployKey("test key")
	if err != nil {
		t.Fatal(err)
	}
	private, err := key.openSSHPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "id")
	if err := os.WriteFile(keyFile, []byte(private), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("ssh-keygen", "-y", "-f", keyFile).Output()
	if err != nil {
		t.Fatalf("ssh-keygen -y: %v", err)
	}
	if got, want := strings.TrimSpace(string(out)), key.authorizedKey(); got != want {
		t.Errorf("public key = %q, want %q", got, want)
	}
	out, err = exec.Command("ssh-keygen", "-l", "-E", "sha256", "-f", keyFile).Output()
	if err != nil {
		t.Fatalf("ssh-keygen -l: %v", err)
	}
	if fields := strings.Fields(string(out)); len(fields) < 2 || fields[1] != key.fingerprint() {
		t.Errorf("ssh-keygen -l = %q, want %s", out, key.fingerprint())
	}
}

// A key that works at once was not just created: refuse it.
func TestRefusesKeyAcceptedAtOnce(t *testing.T) {
	f := setup(t, "write\n")
	code, stdout, stderr := runTool(t, baseArgs())
	if code != 1 || !strings.Contains(stderr, "accepted a key that was just created") || stdout != "" {
		t.Errorf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if _, err := os.Stat(filepath.Join(f.work, "keys", "site-deploy-key.yaml")); err == nil {
		t.Error("the target was written")
	}
}

func TestAgentStopsOnInterrupt(t *testing.T) {
	f := setup(t, "denied\n")
	ctx, cancel := context.WithCancel(context.Background())
	opts, err := parseArgs(baseArgs())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	var stdout bytes.Buffer
	go func() { done <- provision(ctx, opts, &stdout) }()
	for start := time.Now(); !f.exists("agent-key"); time.Sleep(10 * time.Millisecond) {
		if time.Since(start) > 10*time.Second {
			t.Fatal("the key never reached the agent")
		}
	}
	cancel()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "interrupted") {
			t.Errorf("error = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("provision did not stop")
	}
	if entries, _ := os.ReadDir(f.tmp); len(entries) != 0 {
		t.Errorf("TMPDIR still holds %v", entries)
	}
}

func TestProxyJumpKeepsTheUserConfiguration(t *testing.T) {
	f := setup(t, "denied\nwrite\n")
	config := "proxyjump ops@bastion.example.org:2222,jump2.example.org\n"
	if err := os.WriteFile(filepath.Join(f.state, "ssh-config"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSH_AUTH_SOCK", "")
	os.Unsetenv("SSH_AUTH_SOCK")
	if code, stdout, stderr := runTool(t, baseArgs()); code != 0 {
		t.Fatalf("exit %d\n%s%s", code, stdout, stderr)
	}
	calls := strings.Split(f.read("ssh-args"), "=end=\n")
	want := "ProxyCommand=env -u SSH_AUTH_SOCK ssh -o BatchMode=yes -J 'ops@bastion.example.org:2222' -W '[%h]:%p' -- 'jump2.example.org'\n"
	if !strings.Contains(calls[1], want) || strings.Contains(calls[1], "ProxyJump=") {
		t.Errorf("ssh call lacks %q or passes ProxyJump on:\n%s", want, calls[1])
	}
}

func TestProxyCommand(t *testing.T) {
	for _, tt := range []struct {
		command, jump, sock, shell, want string
	}{
		{"", "", "/s", "", ""},
		{"none", "none", "/s", "", ""},
		{"nc %h %p", "", "/run/a b%'s", "", `env SSH_AUTH_SOCK='/run/a b%%'\''s' '/bin/sh' -c 'nc %h %p'`},
		{"nc ${(L)H} %p", "", "", "/usr/bin/zsh", `env -u SSH_AUTH_SOCK '/usr/bin/zsh' -c 'nc ${(L)H} %p'`},
		{"", "bastion", "/s", "", `env SSH_AUTH_SOCK='/s' ssh -o BatchMode=yes -W '[%h]:%p' -- 'bastion'`},
		{"", "a@b:22,ssh://c@d_1:23", "", "", `env -u SSH_AUTH_SOCK ssh -o BatchMode=yes -l 'c' -p 23 -J 'a@b:22' -W '[%h]:%p' -- 'd_1'`},
		{"", "my_b,ops@[2001:db8::1]:2222", "", "", `env -u SSH_AUTH_SOCK ssh -o BatchMode=yes -l 'ops' -p 2222 -J 'my_b' -W '[%h]:%p' -- '2001:db8::1'`},
		{"", "ops@[10.0.0.5]", "", "", `env -u SSH_AUTH_SOCK ssh -o BatchMode=yes -l 'ops' -W '[%h]:%p' -- '10.0.0.5'`},
	} {
		got, err := proxyCommand(tt.command, tt.jump, tt.sock, tt.shell)
		if err != nil || got != tt.want {
			t.Errorf("proxyCommand(%q, %q, %q, %q) = %q, %v, want %q", tt.command, tt.jump, tt.sock, tt.shell, got, err, tt.want)
		}
	}
	for _, jump := range []string{"b;touch x", "a%h", "'b'", "-oProxyCommand=x", "[a b]", "a@-b"} {
		if _, err := proxyCommand("", jump, "", ""); err == nil {
			t.Errorf("accepted ProxyJump %q", jump)
		}
	}
}

func TestDeployKeyOfAnotherRepositoryKeepsWaiting(t *testing.T) {
	setup(t, "denied\notherrepo\nwrite\n")
	code, stdout, stderr := runTool(t, baseArgs())
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, stdout, stderr)
	}
	want := "waiting:      Forgejo refused git-receive-pack: Forgejo: Public (Deploy) Key: 7:publisher is not authorized to write owner/site.\n"
	if !strings.Contains(stdout, want) || strings.Contains(stdout, "has no write access") {
		t.Errorf("stdout lacks %q or calls it read-only:\n%s", want, stdout)
	}
}
