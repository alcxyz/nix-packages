package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSharedReservationsAcrossControllers(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	os.Mkdir(bin, 0700)
	// SSH is a local subprocess stub; no network or privileges are used.
	script := `#!/bin/sh
if [ "$FAIL_COORDINATION" = 1 ]; then exit 255; fi
mkdir -p "$COORDINATION_FIXTURE"
sed "s|/var/lib/nix-deploy/coordination|$COORDINATION_FIXTURE|g" | sh
status=$?
if [ "$FAIL_COORDINATION" = after ]; then exit 255; fi
exit "$status"
`
	os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0700)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("COORDINATION_FIXTURE", filepath.Join(dir, "shared"))
	d := &deployer{inv: &inventory{OperatorHost: "controller", SystemSSHUsers: map[string]string{}}}
	group := &serialGroup{Hosts: []string{"a", "b"}}
	first, err := d.reserveGroup(group)
	if err != nil {
		t.Fatal(err)
	}
	// Separate local state and reordered membership cannot bypass the authority.
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "other-controller"))
	if _, err := d.reserveGroup(&serialGroup{Hosts: []string{"b", "a"}}); err == nil {
		t.Fatal("overlapping controller admitted")
	}
	if !first.release() {
		t.Fatal("release failed")
	}
	second, err := d.reserveGroup(group)
	if err != nil {
		t.Fatal(err)
	}
	if !second.release() {
		t.Fatal("second release failed")
	}
	// Simultaneous controllers must admit exactly one protected rollout.
	type result struct {
		reservation *reservation
		err         error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for i := 0; i < 2; i++ {
		go func() { <-start; r, err := d.reserveGroup(group); results <- result{r, err} }()
	}
	close(start)
	var winner *reservation
	admitted := 0
	for i := 0; i < 2; i++ {
		result := <-results
		if result.err == nil {
			admitted++
			winner = result.reservation
		}
	}
	if admitted != 1 {
		t.Fatalf("admitted %d simultaneous controllers", admitted)
	}
	if !winner.release() {
		t.Fatal("concurrent winner release failed")
	}
	t.Setenv("FAIL_COORDINATION", "1")
	if _, err := d.reserveGroup(group); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatal("unavailable authority admitted")
	}
}

func TestReservationRetainedAfterAmbiguousAcquisition(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "ssh"), []byte(`#!/bin/sh
mkdir -p "$COORDINATION_FIXTURE"
sed "s|/var/lib/nix-deploy/coordination|$COORDINATION_FIXTURE|g" | sh
exit 255
`), 0700)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	shared := filepath.Join(dir, "shared")
	t.Setenv("COORDINATION_FIXTURE", shared)
	d := &deployer{inv: &inventory{OperatorHost: "controller"}}
	if _, err := d.reserveGroup(&serialGroup{Hosts: []string{"a"}}); err == nil {
		t.Fatal("ambiguous acquisition succeeded")
	}
	entries, err := reservationEntries(shared)
	if err != nil || len(entries) != 1 {
		t.Fatalf("reservation lost on disconnect: %v, %v", entries, err)
	}
}

func TestKnownContentionRollsBackPartialAcquisition(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "ssh"), []byte(`#!/bin/sh
mkdir -p "$COORDINATION_FIXTURE"
sed "s|/var/lib/nix-deploy/coordination|$COORDINATION_FIXTURE|g" | sh
`), 0700)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	shared := filepath.Join(dir, "shared")
	t.Setenv("COORDINATION_FIXTURE", shared)
	d := &deployer{inv: &inventory{OperatorHost: "controller"}, self: "controller-a"}
	// 'b' hashes before 'a', so the second controller acquires b before
	// encountering the existing reservation on a.
	held, err := d.reserveGroup(&serialGroup{Hosts: []string{"a"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.reserveGroup(&serialGroup{Hosts: []string{"a", "b"}}); err == nil {
		t.Fatal("contention accepted")
	}
	entries, err := reservationEntries(shared)
	if err != nil || len(entries) != 1 {
		t.Fatalf("partial reservation stranded: %v, %v", entries, err)
	}
	metadata, err := os.ReadFile(filepath.Join(shared, entries[0].Name(), "metadata"))
	if err != nil || !strings.Contains(string(metadata), "controller-a") {
		t.Fatalf("missing invocation metadata: %s, %v", metadata, err)
	}
	if !held.release() {
		t.Fatal("held reservation lost")
	}
}

func reservationEntries(dir string) ([]os.DirEntry, error) {
	entries, err := os.ReadDir(dir)
	var held []os.DirEntry
	for _, entry := range entries {
		if entry.IsDir() && reservationID.MatchString(entry.Name()) {
			held = append(held, entry)
		}
	}
	return held, err
}
