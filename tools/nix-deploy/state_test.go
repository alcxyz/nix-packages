package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFailedActivationsSharedBetweenRuns(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	first := loadFailedActivations()
	second := loadFailedActivations()
	node := target{phase: "system", host: "node"}
	other := target{phase: "system", host: "other"}

	// Each run re-reads the record, so neither drops the other's entry.
	if !first.begin(node, "/nix/store/g") || !second.begin(other, "/nix/store/x") {
		t.Fatal("begin failed")
	}
	second.finish(other, true)
	if !first.failed(node, "/nix/store/g") || second.failed(other, "/nix/store/x") {
		t.Fatal("entries lost or kept across runs")
	}
	first.finish(node, false)

	// A later failure keeps an earlier unfinished generation.
	if !second.begin(node, "/nix/store/h") {
		t.Fatal("begin failed")
	}
	second.finish(node, false)
	if !first.failed(node, "/nix/store/g") || !first.failed(node, "/nix/store/h") {
		t.Fatal("an unfinished generation was dropped")
	}
	if !first.begin(node, "/nix/store/g") {
		t.Fatal("begin failed")
	}
	first.finish(node, true)
	if second.failed(node, "/nix/store/g") || second.failed(node, "/nix/store/h") {
		t.Fatal("a success did not clear the target")
	}
}

func TestFailedActivationsRefusesWithoutRecord(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	// The record's directory cannot be created.
	if err := os.WriteFile(filepath.Join(state, "nix-deploy"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if loadFailedActivations().begin(target{phase: "hm", host: "node"}, "/nix/store/g") {
		t.Fatal("begin reported success without a record")
	}
}

func TestFailedActivationsSerializeOneTarget(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	first := loadFailedActivations()
	second := loadFailedActivations()
	node := target{phase: "system", host: "node"}

	if !first.begin(node, "/nix/store/g") {
		t.Fatal("begin failed")
	}
	started := make(chan bool)
	go func() { started <- second.begin(node, "/nix/store/h") }()
	select {
	case <-started:
		t.Fatal("a second run activated the target while the first was in flight")
	case <-time.After(200 * time.Millisecond):
	}
	// The first run fails; the second may then proceed, and its success
	// clears both entries only after the first one ended.
	first.finish(node, false)
	if !<-started {
		t.Fatal("second begin failed")
	}
	if !second.failed(node, "/nix/store/g") || !second.failed(node, "/nix/store/h") {
		t.Fatal("an unfinished generation was dropped")
	}
	second.finish(node, true)
	if first.failed(node, "/nix/store/g") {
		t.Fatal("a success did not clear the target")
	}
}

func TestFailedActivationsUnreadableCountsAsUnfinished(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	if err := os.MkdirAll(filepath.Join(state, "nix-deploy"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "nix-deploy", "failed-activations.json"), []byte("{damaged"), 0o644); err != nil {
		t.Fatal(err)
	}
	record := loadFailedActivations()
	node := target{phase: "system", host: "node"}
	if !record.failed(node, "/nix/store/g") {
		t.Fatal("a damaged record passed for an empty one")
	}
	if record.begin(node, "/nix/store/g") {
		t.Fatal("begin succeeded on a damaged record")
	}
}

func TestFailedActivationsNullRecordIsUnreadable(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	if err := os.MkdirAll(filepath.Join(state, "nix-deploy"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "nix-deploy", "failed-activations.json"), []byte("null"), 0o644); err != nil {
		t.Fatal(err)
	}
	record := loadFailedActivations()
	node := target{phase: "system", host: "node"}
	if !record.failed(node, "/nix/store/g") || record.begin(node, "/nix/store/g") {
		t.Fatal("a null record passed for a usable one")
	}
}
