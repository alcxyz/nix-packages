package main

import (
	"reflect"
	"testing"
)

func TestParsePendingBuilds(t *testing.T) {
	output := `warning: Nix search path entry '/nix/var/nix/profiles/per-user/root/channels' does not exist, ignoring
these 2 derivations will be built:
  /nix/store/aaaa-hyprland-0.56.2.drv
  /nix/store/bbbb-etc.drv
these 2 paths will be fetched (1.0 MiB download, 4.0 MiB unpacked):
  /nix/store/cccc-hello-2.12.3
  /nix/store/dddd-glibc-2.42.drv
`
	want := []string{"/nix/store/aaaa-hyprland-0.56.2.drv", "/nix/store/bbbb-etc.drv"}
	if got, err := parsePendingBuilds(output); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("parsePendingBuilds = %v, %v; want %v", got, err, want)
	}

	single := "this path will be fetched (1.0 KiB download, 1.0 KiB unpacked):\n  /nix/store/eeee-x\nthis derivation will be built:\n  /nix/store/ffff-y.drv\n"
	if got, err := parsePendingBuilds(single); err != nil || !reflect.DeepEqual(got, []string{"/nix/store/ffff-y.drv"}) {
		t.Fatalf("single derivation: %v, %v", got, err)
	}
	for _, empty := range []string{"", "these 3 paths will be fetched (1 KiB download):\n  /nix/store/a\n", "warning: something\n"} {
		if got, err := parsePendingBuilds(empty); err != nil || len(got) != 0 {
			t.Fatalf("%q: %v, %v", empty, got, err)
		}
	}
	// A list Nix labels some other way is not taken for nothing to build.
	if _, err := parsePendingBuilds("these 2 derivations are queued:\n  /nix/store/a.drv\n  /nix/store/b.drv\n"); err == nil {
		t.Fatal("unknown heading accepted")
	}
	if _, err := parsePendingBuilds("  /nix/store/a.drv\n"); err == nil {
		t.Fatal("list without a heading accepted")
	}
	if _, err := parsePendingBuilds("these 2 derivations will be built:\n\t/nix/store/a.drv\n"); err == nil {
		t.Fatal("announced builds without a readable list accepted")
	}

	// Under systemd, Nix starts each line with its log level.
	systemd := "<4>warning: Nix search path entry does not exist, ignoring\n<3>this derivation will be built:\n<3>  /nix/store/gggg-z.drv\n<3>this path will be fetched (74.2 KiB download, 273.0 KiB unpacked):\n<3>  /nix/store/hhhh-hello\n"
	if got, err := parsePendingBuilds(systemd); err != nil || !reflect.DeepEqual(got, []string{"/nix/store/gggg-z.drv"}) {
		t.Fatalf("systemd log levels: %v, %v", got, err)
	}
}

func TestCapLimits(t *testing.T) {
	cases := []struct {
		host        hostLimits
		cpus        int
		jobs, cores int
	}{
		{hostLimits{jobs: 32, cores: 0}, 32, 6, 8},   // a large workstation keeps the heavy values
		{hostLimits{jobs: 4, cores: 0}, 4, 4, 4},     // a small host is never loaded beyond its defaults
		{hostLimits{jobs: 2, cores: 3}, 16, 2, 3},    // explicit low settings win
		{hostLimits{jobs: 0, cores: 0}, 16, 0, 8},    // remote builders only stays remote only
		{hostLimits{jobs: -1, cores: -1}, 2, -1, -1}, // unknown settings are left to the host
		{hostLimits{jobs: 64, cores: 16}, 64, 6, 8},  // higher settings are lowered
	}
	for _, c := range cases {
		jobs, cores := capLimits(6, 8, c.host, c.cpus)
		if jobs != c.jobs || cores != c.cores {
			t.Errorf("capLimits(6, 8, %+v, %d) = %d, %d; want %d, %d", c.host, c.cpus, jobs, cores, c.jobs, c.cores)
		}
	}
}
