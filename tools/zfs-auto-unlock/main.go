package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type PoolConfig struct {
	Name             string `json:"name"`
	EncryptedKeyFile string `json:"encryptedKeyFile"`
}

type Config struct {
	IdentityFiles []string     `json:"identityFiles"`
	Pools         []PoolConfig `json:"pools"`
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func runCmd(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func runCmdOutput(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func ensurePoolImported(ctx context.Context, pool string) error {
	out, err := runCmdOutput(ctx, "zpool", "list", "-H", "-o", "name")
	if err != nil {
		return fmt.Errorf("zpool list failed: %w", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(line) == pool {
			return nil
		}
	}
	log.Printf("Pool %s not imported; attempting import -N", pool)
	if err := runCmd(ctx, "zpool", "import", "-N", pool); err != nil {
		return fmt.Errorf("zpool import -N %s failed: %w", pool, err)
	}
	return nil
}

func ensureKeystoreMounted(ctx context.Context, pool string, keyFile string) error {
	ds := pool + "/keystore"
	log.Printf("Ensuring keystore dataset %s is mounted", ds)
	// Ignore error if already mounted; we'll detect missing key file later anyway.
	if err := runCmd(ctx, "zfs", "mount", ds); err != nil {
		log.Printf("Warning: zfs mount %s failed: %v", ds, err)
	}
	dir := filepath.Dir(keyFile)
	if !fileExists(dir) {
		return fmt.Errorf("keystore mountpoint %s does not exist after zfs mount", dir)
	}
	return nil
}

func getKeyStatus(ctx context.Context, pool string) (string, error) {
	out, err := runCmdOutput(ctx, "zfs", "get", "-H", "-o", "value", "keystatus", pool)
	if err != nil {
		return "unavailable", err
	}
	return strings.TrimSpace(out), nil
}

func tryIdentity(ctx context.Context, pool PoolConfig, identity string) error {
	tmp, err := os.CreateTemp("/run", "zfs-key-"+pool.Name+"-")
	if err != nil {
		return fmt.Errorf("create temp key file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		tmp.Close()
		_ = os.Remove(tmpPath)
	}()

	log.Printf("Decrypting %s with identity %s", pool.EncryptedKeyFile, identity)

	cmd := exec.CommandContext(ctx,
		"age",
		"--decrypt",
		"-i", identity,
		pool.EncryptedKeyFile,
	)
	cmd.Stdout = tmp
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("age decrypt failed: %w", err)
	}

	log.Printf("Loading ZFS key for pool %s from %s", pool.Name, tmpPath)

	if err := runCmd(ctx, "zfs", "load-key", "-L", "file://"+tmpPath, pool.Name); err != nil {
		return fmt.Errorf("zfs load-key failed: %w", err)
	}

	return nil
}

func unlockPoolOnce(ctx context.Context, cfg *Config, pool PoolConfig) error {
	log.Printf("=== Pool %s ===", pool.Name)

	if err := ensurePoolImported(ctx, pool.Name); err != nil {
		return err
	}

	if err := ensureKeystoreMounted(ctx, pool.Name, pool.EncryptedKeyFile); err != nil {
		return err
	}

	status, err := getKeyStatus(ctx, pool.Name)
	if err == nil && status == "available" {
		log.Printf("Pool %s already unlocked", pool.Name)
		return nil
	}

	if !fileExists(pool.EncryptedKeyFile) {
		return fmt.Errorf("encrypted key file %s not found", pool.EncryptedKeyFile)
	}

	for _, id := range cfg.IdentityFiles {
		if id == "" {
			continue
		}
		if !fileExists(id) {
			continue
		}
		log.Printf("Trying identity %s for pool %s", id, pool.Name)
		if err := tryIdentity(ctx, pool, id); err != nil {
			log.Printf("Identity %s failed for %s: %v", id, pool.Name, err)
			continue
		}
		// Double-check keystatus
		status, err = getKeyStatus(ctx, pool.Name)
		if err == nil && status == "available" {
			log.Printf("Pool %s successfully unlocked with %s", pool.Name, id)
			return nil
		}
		log.Printf("zfs reports keystatus=%s after load-key for %s", status, pool.Name)
	}

	return fmt.Errorf("no identity could unlock pool %s", pool.Name)
}

func unlockPoolWithTimeout(cfg *Config, pool PoolConfig, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return unlockPoolOnce(ctx, cfg, pool)
}

func main() {
	configPath := flag.String("config", "/etc/zfs-auto-unlock.json", "Path to JSON config file")
	timeout := flag.Duration("timeout", 60*time.Second, "Per-pool timeout")
	flag.Parse()

	f, err := os.Open(*configPath)
	if err != nil {
		log.Fatalf("open config %s: %v", *configPath, err)
	}
	defer f.Close()

	var cfg Config
	if err := json.NewDecoder(f).Decode(&cfg); err != nil {
		log.Fatalf("parse config: %v", err)
	}

	if len(cfg.Pools) == 0 {
		log.Fatalf("config has no pools")
	}
	if len(cfg.IdentityFiles) == 0 {
		log.Fatalf("config has no identityFiles")
	}

	var hadErr bool
	for _, p := range cfg.Pools {
		if p.Name == "" || p.EncryptedKeyFile == "" {
			log.Printf("Skipping pool with incomplete config: %+v", p)
			continue
		}
		if err := unlockPoolWithTimeout(&cfg, p, *timeout); err != nil {
			log.Printf("ERROR: unlocking pool %s failed: %v", p.Name, err)
			hadErr = true
		}
	}

	if hadErr {
		os.Exit(1)
	}

	// All pools unlocked successfully; ensure all datasets are mounted.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := runCmd(ctx, "zfs", "mount", "-a"); err != nil {
		log.Printf("WARNING: 'zfs mount -a' failed: %v", err)
	} else {
		log.Printf("'zfs mount -a' completed successfully")
	}
}
