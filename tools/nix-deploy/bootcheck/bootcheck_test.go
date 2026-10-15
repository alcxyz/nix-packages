package bootcheck

import (
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// report renders Script output for a host whose systems use the given
// kernel, initrd, modules and parameters; "-" marks an entry that does not
// resolve.
func report(booted, current, profile [4]string) string {
	lines := []string{"os=Linux"}
	for _, side := range []struct {
		name    string
		entries [4]string
	}{{"booted", booted}, {"current", current}, {"profile", profile}} {
		lines = append(lines, side.name+"=/nix/store/"+side.name+"-system")
		for i, entry := range Entries {
			value := side.entries[i]
			switch {
			case value == "-":
			case entry == "kernel-params":
				value = "p:" + value
			default:
				value = "/nix/store/" + value + "-" + entry
			}
			lines = append(lines, side.name+"-"+entry+"="+value)
		}
	}
	return strings.Join(lines, "\n") + "\n"
}

func TestParse(t *testing.T) {
	same := [4]string{"a", "a", "a", "a"}
	cases := []struct {
		name    string
		output  string
		reboot  string
		differs []string
		missing []string
	}{
		{"unchanged", report(same, same, same), NotNeeded, nil, nil},
		{"new kernel running", report(same, [4]string{"b", "a", "b", "a"}, [4]string{"b", "a", "b", "a"}), Needed, []string{"kernel", "kernel-modules"}, nil},
		{"new initrd staged for boot", report(same, same, [4]string{"a", "b", "a", "a"}), Needed, []string{"initrd"}, nil},
		{"missing initrd", report(same, [4]string{"a", "-", "a", "a"}, same), Unknown, nil, []string{"initrd"}},
		{"known difference beats a missing entry", report([4]string{"a", "-", "a", "a"}, [4]string{"b", "a", "a", "a"}, [4]string{"b", "a", "a", "a"}), Needed, []string{"kernel"}, []string{"initrd"}},
		{"new kernel parameters running", report(same, [4]string{"a", "a", "a", "b"}, [4]string{"a", "a", "a", "b"}), Needed, []string{"kernel-params"}, nil},
		{"missing kernel parameters", report(same, same, [4]string{"a", "a", "a", "-"}), Unknown, nil, []string{"kernel-params"}},
		{"empty kernel parameters compare", report([4]string{"a", "a", "a", ""}, [4]string{"a", "a", "a", ""}, [4]string{"a", "a", "a", ""}), NotNeeded, nil, nil},
		{"no booted system", "os=Linux\nbooted=-\n", Unknown, nil, Entries},
		{"empty output", "", Unknown, nil, nil},
		{"darwin", "os=Darwin\n", Unknown, nil, nil},
	}
	for _, c := range cases {
		s := Parse(c.output)
		if s.Reboot != c.reboot || !slices.Equal(s.Differs, c.differs) || !slices.Equal(s.Missing, c.missing) {
			t.Errorf("%s: got %s differs %v missing %v (%s), want %s differs %v missing %v",
				c.name, s.Reboot, s.Differs, s.Missing, s.Reason(), c.reboot, c.differs, c.missing)
		}
	}
}

func TestParseKeepsSystemsAndIgnoresNoise(t *testing.T) {
	s := Parse("Warning: something\n" + report([4]string{"a", "a", "a", "a"}, [4]string{"a", "a", "a", "a"}, [4]string{"a", "a", "a", "a"}) + "runner=x active\n")
	if s.OS != "Linux" || s.Booted != "/nix/store/booted-system" || s.Current != "/nix/store/current-system" || s.Profile != "/nix/store/profile-system" {
		t.Errorf("got %+v", s)
	}
}

func TestReason(t *testing.T) {
	if got := Parse("os=Darwin\n").Reason(); got != "Darwin host, not NixOS" {
		t.Errorf("darwin: %q", got)
	}
	if got := Parse("").Reason(); got != "the host did not report its systems" {
		t.Errorf("empty: %q", got)
	}
	if got := Parse(report([4]string{"a", "a", "a", "a"}, [4]string{"a", "-", "a", "a"}, [4]string{"a", "a", "a", "a"})).Reason(); !strings.Contains(got, "cannot compare initrd") {
		t.Errorf("unknown: %q", got)
	}
}

// The script is quoted for the remote shell, and runs on this machine.
func TestScriptRuns(t *testing.T) {
	if strings.Contains(Script, "'") {
		t.Fatal("the script contains a single quote")
	}
	output, err := exec.Command("sh", "-c", Script).Output()
	if err != nil {
		t.Fatalf("script failed: %v", err)
	}
	t.Logf("script output:\n%s", output)
	s := Parse(string(output))
	if runtime.GOOS == "linux" {
		if s.OS != "Linux" || strings.Count(string(output), "\n") != 16 {
			t.Errorf("unexpected output:\n%s", output)
		}
	}
	if s.Reboot != Needed && s.Reboot != NotNeeded && s.Reboot != Unknown {
		t.Errorf("verdict %q", s.Reboot)
	}
}
