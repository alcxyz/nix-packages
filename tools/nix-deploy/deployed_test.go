package main

import "testing"

func TestDeploysRemotely(t *testing.T) {
	d := &deployer{
		self: "xyz",
		inv: &inventory{
			KnownHosts:       []string{"xyz", "node", "solo", "pi-one"},
			HomeManagerHosts: []string{"xyz", "node"},
		},
	}
	for name, want := range map[string]bool{
		"system-node":   true,
		"hm-node":       true,
		"system-solo":   true,
		"system-pi-one": true,
		"hm-solo":       false, // solo has no Home Manager configuration
		"system-xyz":    false, // this machine's profiles root its own
		"hm-xyz":        false,
		"system-gone":   false,
		"other-node":    false,
		"node":          false,
	} {
		if got := d.deploysRemotely(name); got != want {
			t.Errorf("deploysRemotely(%q) = %v, want %v", name, got, want)
		}
	}
}
