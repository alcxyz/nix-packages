package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestFormatWait(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0:                                   "0s",
		45 * time.Second:                    "45s",
		30 * time.Minute:                    "30m",
		time.Hour:                           "1h",
		90 * time.Minute:                    "1h30m",
		2*time.Hour + 5*time.Second:         "2h5s",
		3*time.Hour + 1500*time.Millisecond: "3h2s",
	} {
		if got := formatWait(d); got != want {
			t.Errorf("formatWait(%s) = %q, want %q", d, got, want)
		}
	}
}

// Status paints each host's name in its deploy color, in the table and on
// its notes, and pads the column before painting so the table stays aligned.
func TestStatusPaintsHosts(t *testing.T) {
	u := &ui{color: true, hostColors: map[string]string{"xyz": "1;2;3"}}
	report := statusReport{Hosts: []hostStatus{
		{Host: "xyz", Error: "timed out"},
		{Host: "plain", Error: "timed out"},
	}}
	var out bytes.Buffer
	printStatus(&out, u, report)
	text := out.String()
	for _, want := range []string{
		"\033[38;2;1;2;3mxyz         \033[0m unreachable ",
		"\033[38;2;1;2;3m[xyz]\033[0m unreachable: timed out\n",
		"\033[38;2;" + colorText + "m[plain]\033[0m unreachable: timed out\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%q", want, text)
		}
	}
	if (*ui)(nil).host("xyz") != "[xyz]" || (*ui)(nil).hostColumn("xyz", 5) != "xyz  " {
		t.Errorf("a nil ui paints")
	}
}

// One unit that needs a restart is named; several are counted.
func TestStatusNamesASingleRestart(t *testing.T) {
	r := parseRunners(runnerReport([]string{primaryOld}, ""))
	notes := runnerNotes(nil, "xyz", r)
	want := "[xyz] forgejo-actions-runner.service needs a restart (started from an older definition): nixfleet runners restart xyz"
	if len(notes) != 1 || notes[0] != want {
		t.Errorf("notes: %q", notes)
	}
}
