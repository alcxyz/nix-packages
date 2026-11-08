package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestOnFailureOption(t *testing.T) {
	d := &deployer{}
	if _, err := d.parseOptions([]string{"--all", "--on-failure", "stop"}); err != nil || d.onFailure != "stop" || d.onFailureSource != "--on-failure" {
		t.Errorf("--on-failure stop: %v, %+v", err, d)
	}
	d = &deployer{}
	if _, err := d.parseOptions([]string{"--on-failure=continue"}); err != nil || d.onFailure != "continue" {
		t.Errorf("--on-failure=continue: %v, %+v", err, d)
	}
	for _, args := range [][]string{{"--on-failure"}, {"--on-failure", "maybe"}, {"--on-failure="}} {
		if _, err := (&deployer{}).parseOptions(args); err == nil || !strings.HasPrefix(err.Error(), "--on-failure") {
			t.Errorf("%v: got %v", args, err)
		}
	}

	t.Setenv("DEPLOY_ON_FAILURE", "sometimes")
	if _, err := loadSettings(); err == nil || err.Error() != "DEPLOY_ON_FAILURE must be ask, continue or stop" {
		t.Errorf("invalid DEPLOY_ON_FAILURE: %v", err)
	}
	t.Setenv("DEPLOY_ON_FAILURE", "stop")
	if values, err := loadSettings(); err != nil || values.onFailure != "stop" {
		t.Errorf("DEPLOY_ON_FAILURE=stop: %+v, %v", values, err)
	}
}

func TestResolveOnFailure(t *testing.T) {
	cases := []struct {
		name              string
		flag, env         string
		all, terminals    bool
		dryRun, buildOnly bool
		want, wantErr     string
	}{
		{name: "terminal default", terminals: true, want: "ask"},
		{name: "terminal default, fleet", all: true, terminals: true, want: "ask"},
		{name: "no terminal, fleet", all: true, want: "continue"},
		{name: "no terminal, single host", want: "stop"},
		{name: "environment", env: "stop", all: true, terminals: true, want: "stop"},
		{name: "flag wins", flag: "continue", env: "stop", terminals: true, want: "continue"},
		{name: "ask without terminal", flag: "ask", all: true, wantErr: "--on-failure=ask needs standard input and output on a terminal"},
		{name: "environment ask without terminal", env: "ask", wantErr: "DEPLOY_ON_FAILURE=ask needs standard input and output on a terminal"},
		{name: "ask in a build-only run", flag: "ask", buildOnly: true, want: "ask"},
		{name: "ask in a dry run", flag: "ask", dryRun: true, want: "ask"},
	}
	for _, c := range cases {
		d := &deployer{settings: settings{onFailure: c.env}, dryRun: c.dryRun, buildOnly: c.buildOnly}
		if c.flag != "" {
			d.onFailure, d.onFailureSource = c.flag, "--on-failure"
		}
		err := d.resolveOnFailure(c.all, c.terminals)
		switch {
		case c.wantErr != "":
			if err == nil || err.Error() != c.wantErr {
				t.Errorf("%s: got %v, want %q", c.name, err, c.wantErr)
			}
		case err != nil || d.onFailure != c.want:
			t.Errorf("%s: got %q, %v, want %q", c.name, d.onFailure, err, c.want)
		}
	}
}

func TestDecideAfterFailures(t *testing.T) {
	failed := []target{{phase: "system", host: "node"}}
	ready := []target{{phase: "hm", host: "node"}, {phase: "system", host: "solo"}}
	cases := []struct {
		name, choice, answer string
		activate             bool
	}{
		{"continue", "continue", "", true},
		{"stop", "stop", "", false},
		{"ask, yes", "ask", "yes\n", true},
		{"ask, y", "ask", " Y \n", true},
		{"ask, no", "ask", "n\n", false},
		{"ask, empty", "ask", "\n", false},
		{"ask, end of input", "ask", "", false},
		{"ask, no newline", "ask", "y", false},
		{"ask, unbounded", "ask", strings.Repeat("y", 2*maxAnswerBytes), false},
	}
	for _, c := range cases {
		d := &deployer{ui: &ui{}, onFailure: c.choice}
		r := newReport()
		r.record("system", "node", "failed")
		var out bytes.Buffer
		got := d.decideAfterFailures(failed, ready, r, strings.NewReader(c.answer), &out)
		if got != c.activate {
			t.Errorf("%s: activate %v, want %v", c.name, got, c.activate)
		}
		want := "-"
		if !c.activate {
			want = "skipped"
		}
		if r.result("hm", "node") != want || r.result("system", "solo") != want || r.result("system", "node") != "failed" || !r.failed {
			t.Errorf("%s: results %v", c.name, r.results)
		}
		asked := strings.Contains(out.String(), "activate the 2 targets that built? [y/N]")
		if asked != (c.choice == "ask") {
			t.Errorf("%s: prompt %q", c.name, out.String())
		}
		if asked && !strings.Contains(out.String(), "1 target failed to evaluate or build:\n  [system] [node]\n") {
			t.Errorf("%s: failed targets not listed: %q", c.name, out.String())
		}
	}

	// Nothing failed, or nothing built: no question and nothing skipped.
	for _, c := range []struct{ failed, ready []target }{{nil, ready}, {failed, nil}} {
		d := &deployer{ui: &ui{}, onFailure: "ask"}
		r := newReport()
		var out bytes.Buffer
		if !d.decideAfterFailures(c.failed, c.ready, r, strings.NewReader("n\n"), &out) || out.Len() != 0 || len(r.results) != 0 {
			t.Errorf("%v / %v: asked %q, results %v", c.failed, c.ready, out.String(), r.results)
		}
	}
}
