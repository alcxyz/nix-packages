package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

const offerQuestion = "deploy: start nixfleet triage on this run? [y/N] "

// failedRun records a finished failed run and returns a deployer holding it,
// with an inventory whose triage object is triage (nil leaves it out).
func failedRun(t *testing.T, triage any) *deployer {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	d := &deployer{configFile: writeInventory(t, triage)}
	captureStderr(t, func() {
		d.record = startRunRecord(nil, flakeWithLock(t, "lock"), false, false, false, 30)
		r := newReport()
		r.record("system", "node", "failed")
		d.finishRecord([]string{"node"}, r, true)
	})
	return d
}

// unsetGuard removes NIXFLEET_TRIAGE for the test and restores it after.
func unsetGuard(t *testing.T) {
	t.Setenv(triageGuard, "")
	os.Unsetenv(triageGuard)
}

var agentTriage = map[string]any{"agentCommand": []string{"/bin/agent"}}

func TestTriageOfferAnswers(t *testing.T) {
	unsetGuard(t)
	for _, tc := range []struct {
		answer string
		start  bool
	}{
		{"y\n", true}, {"yes\n", true}, {"Y\n", true}, {" YES \n", true},
		{"\n", false}, {"n\n", false}, {"no\n", false}, {"yep\n", false},
		{"", false}, {"y", false},
	} {
		d := failedRun(t, agentTriage)
		var calls [][]string
		triage := func(args []string) int {
			calls = append(calls, args)
			// Triage refuses a run that still holds its run.lock.
			if _, _, err := readFinishedRun(args[2]); err != nil {
				t.Errorf("%q: triage cannot read the run: %v", tc.answer, err)
			}
			return 9
		}
		var out bytes.Buffer
		if status := d.offerTriage(1, true, strings.NewReader(tc.answer), &out, triage); status != 1 {
			t.Errorf("%q: status %d, want deploy's 1", tc.answer, status)
		}
		if !strings.HasPrefix(out.String(), offerQuestion) {
			t.Errorf("%q: output %q", tc.answer, out.String())
		}
		if !tc.start {
			if len(calls) != 0 {
				t.Errorf("%q started triage", tc.answer)
			}
			continue
		}
		if len(calls) != 1 || strings.Join(calls[0], " ") != "--config "+d.configFile+" "+d.record.dir {
			t.Errorf("%q: triage calls %q", tc.answer, calls)
		}
	}
}

func TestTriageOfferReadErrorDeclines(t *testing.T) {
	unsetGuard(t)
	d := failedRun(t, agentTriage)
	started := false
	var out bytes.Buffer
	status := d.offerTriage(4, true, iotest.ErrReader(os.ErrClosed), &out, func([]string) int { started = true; return 0 })
	if status != 4 || started || out.String() != offerQuestion+"\n" {
		t.Fatalf("status %d, started %v, output %q", status, started, out.String())
	}
}

// zeros yields zero bytes without end, as /dev/zero does.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func TestTriageOfferBoundsTheAnswer(t *testing.T) {
	unsetGuard(t)
	d := failedRun(t, agentTriage)
	done := make(chan int, 1)
	started := false
	var out bytes.Buffer
	go func() {
		done <- d.offerTriage(3, true, zeros{}, &out, func([]string) int { started = true; return 0 })
	}()
	select {
	case status := <-done:
		if status != 3 || started || out.String() != offerQuestion+"\n" {
			t.Fatalf("status %d, started %v, output %q", status, started, out.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the answer read did not end on endless zero bytes")
	}
	// A yes padded past the limit without a newline declines too.
	d = failedRun(t, agentTriage)
	out.Reset()
	answer := "yes" + strings.Repeat(" ", maxAnswerBytes) + "\n"
	d.offerTriage(1, true, strings.NewReader(answer), &out, func([]string) int { started = true; return 0 })
	if started {
		t.Fatal("an answer past the limit started triage")
	}
}

func TestTriageOfferConditions(t *testing.T) {
	unsetGuard(t)
	cases := map[string]func(t *testing.T) (*deployer, int, bool){
		"succeeded":   func(t *testing.T) (*deployer, int, bool) { return failedRun(t, agentTriage), 0, true },
		"no terminal": func(t *testing.T) (*deployer, int, bool) { return failedRun(t, agentTriage), 1, false },
		"no triage":   func(t *testing.T) (*deployer, int, bool) { return failedRun(t, nil), 1, true },
		"no agent":    func(t *testing.T) (*deployer, int, bool) { return failedRun(t, map[string]any{}), 1, true },
		"no record": func(t *testing.T) (*deployer, int, bool) {
			return &deployer{configFile: writeInventory(t, agentTriage)}, 1, true
		},
		"record unwritten": func(t *testing.T) (*deployer, int, bool) {
			return &deployer{configFile: writeInventory(t, agentTriage), record: &runRecord{}}, 1, true
		},
		"no inventory": func(t *testing.T) (*deployer, int, bool) {
			d := failedRun(t, agentTriage)
			d.configFile = filepath.Join(t.TempDir(), "missing.json")
			return d, 1, true
		},
		"guard set": func(t *testing.T) (*deployer, int, bool) {
			t.Setenv(triageGuard, "1")
			return failedRun(t, agentTriage), 1, true
		},
		"guard empty": func(t *testing.T) (*deployer, int, bool) {
			t.Setenv(triageGuard, "")
			return failedRun(t, agentTriage), 1, true
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			d, status, terminals := setup(t)
			started := false
			var out bytes.Buffer
			got := d.offerTriage(status, terminals, strings.NewReader("y\n"), &out, func([]string) int { started = true; return 0 })
			if got != status || started || out.Len() != 0 {
				t.Fatalf("status %d, started %v, output %q", got, started, out.String())
			}
		})
	}
	// All conditions hold: the same setup offers.
	d := failedRun(t, agentTriage)
	var out bytes.Buffer
	d.offerTriage(1, true, strings.NewReader("n\n"), &out, func([]string) int { return 0 })
	if out.String() != offerQuestion {
		t.Fatalf("output %q", out.String())
	}
}

func TestFinishedRecordReleasesItsLock(t *testing.T) {
	d := failedRun(t, nil)
	if held, err := runHeld(d.record.dir); err != nil || held {
		t.Fatalf("held %v, err %v after the record finished", held, err)
	}
}

func TestTriageLaunchSetsGuard(t *testing.T) {
	t.Setenv(triageGuard, "")
	scripts := t.TempDir()
	record := filepath.Join(scripts, "env")
	agent := filepath.Join(scripts, "agent")
	script := "#!/bin/sh\nenv > " + record + "\n"
	if err := os.WriteFile(agent, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := &triageConfig{AgentCommand: []string{agent}}
	if status := launchTriage(cfg, "/c", t.TempDir(), "/p", false); status != 0 {
		t.Fatalf("exit status %d", status)
	}
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(data), "\n")
	count := 0
	for _, line := range lines {
		if strings.HasPrefix(line, triageGuard+"=") {
			count++
			if line != triageGuard+"=1" {
				t.Errorf("agent environment has %q", line)
			}
		}
	}
	if count != 1 {
		t.Fatalf("agent environment has %d %s entries", count, triageGuard)
	}
}
