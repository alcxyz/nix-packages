package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testClock is "now" for the fixtures: two hours after the last change of
// most fixture threads.
var testClock = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func fakeBin(t *testing.T, name string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("testdata", "bin", name))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func testConfig(t *testing.T) Config {
	t.Helper()
	cfg := Config{
		Listen:       "127.0.0.1:0",
		PublicOrigin: "https://deck.example.com",
		Threads: ThreadsConfig{Instances: []InstanceConfig{{
			Name: "main", Label: "Main", DB: "/var/lib/example/state.sqlite", WebURL: "https://t3.example.com/",
		}}},
		Commands: CommandsConfig{
			Inventory: fakeBin(t, "t3-thread-inventory"),
			Status:    fakeBin(t, "t3-thread-status"),
			Messages:  fakeBin(t, "t3-thread-messages"),
			Group:     fakeBin(t, "t3-thread-overview-group"),
			Ack:       fakeBin(t, "t3-thread-overview-ack"),
		},
	}
	if err := cfg.normalize(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// newTestDeck builds a deck on the fake commands with a fixed clock and a
// fresh cache, and records the commands' arguments in the returned log.
func newTestDeck(t *testing.T) (*Deck, string) {
	t.Helper()
	argsLog := filepath.Join(t.TempDir(), "args.log")
	t.Setenv("DECK_ARGS_LOG", argsLog)
	cache, err := openSummaryCache(filepath.Join(t.TempDir(), "summaries.json"))
	if err != nil {
		t.Fatal(err)
	}
	models := []ModelConfig{{Provider: "anthropic", Model: "light-model", Transport: "cli"}}
	d := newDeck(testConfig(t), cache, models, nil, quietLog())
	d.now = func() time.Time { return testClock }
	d.runModel = func(ctx context.Context, m ModelConfig, prompt string) ([]byte, error) {
		t.Fatalf("unexpected model call")
		return nil, nil
	}
	return d, argsLog
}

func readArgs(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// refreshAndProbe refreshes an instance and waits for its probes.
func refreshAndProbe(d *Deck, in InstanceConfig) {
	d.refreshInstance(context.Background(), in)
	d.probeWG.Wait()
}
