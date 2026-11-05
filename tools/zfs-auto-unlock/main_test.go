package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConfigValidatedBeforeAnySubprocess(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for _, cfg := range []Config{
		{IdentityFiles: []string{"/synthetic/identity"}, Pools: []PoolConfig{{Name: "pool-a", EncryptedKeyFile: "/synthetic/key"}, {Name: ""}}},
		{IdentityFiles: []string{""}, Pools: []PoolConfig{{Name: "pool-a", EncryptedKeyFile: "/synthetic/key"}}},
		{IdentityFiles: []string{"/synthetic/identity"}, Pools: []PoolConfig{{Name: "pool-a", EncryptedKeyFile: "/synthetic/key"}, {Name: "pool-a", EncryptedKeyFile: "/synthetic/key2"}}},
	} {
		err := runConfig(&cfg, time.Second)
		if err == nil || strings.Contains(err.Error(), "executable") {
			t.Fatalf("invalid configuration reached subprocess: %v", err)
		}
	}
}

func TestUnlockAndMountFailures(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "synthetic.age")
	os.WriteFile(key, []byte("synthetic encrypted fixture"), 0600)
	cfg := &Config{IdentityFiles: []string{filepath.Join(dir, "identity")}, Pools: []PoolConfig{{Name: "pool-a", EncryptedKeyFile: key}}}
	bin := filepath.Join(dir, "bin")
	os.Mkdir(bin, 0700)
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$CALLS"
case "$*" in
 'list -H -o name') echo pool-a ;;
 'get -H -o value mounted pool-a/keystore') if [ "$FAIL" = inspect ]; then exit 1; fi; if [ "$FAIL" = keystore ]; then echo no; else echo yes; fi ;;
 'mount pool-a/keystore') exit 1 ;;
 'get -H -o value keystatus pool-a') if [ "$FAIL" = unlock ]; then echo unavailable; else echo available; fi ;;
 'mount -a') if [ "$FAIL" = mount ]; then exit 1; fi ;;
 *) exit 99 ;;
esac
`
	for _, cmd := range []string{"zfs", "zpool"} {
		os.WriteFile(filepath.Join(bin, cmd), []byte(script), 0700)
	}
	t.Setenv("PATH", bin)
	t.Setenv("CALLS", filepath.Join(dir, "calls"))
	for _, failure := range []string{"inspect", "keystore", "unlock", "mount", "none"} {
		t.Setenv("FAIL", failure)
		err := runConfig(cfg, time.Second)
		if (err != nil) != (failure != "none") {
			t.Fatalf("%s: %v", failure, err)
		}
	}
}
