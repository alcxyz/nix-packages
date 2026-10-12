package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func validFields() map[string]any {
	return map[string]any{
		"schemaVersion":         1,
		"operatorHost":          "xyz",
		"homeManagerUser":       "alc",
		"homeOutputPrefix":      "alc-",
		"knownHosts":            []any{"xyz", "node"},
		"homeManagerHosts":      []any{"xyz"},
		"remoteHosts":           []any{"node"},
		"deployAllHosts":        []any{"xyz", "node"},
		"aliases":               map[string]any{"edge": "node"},
		"sshHosts":              map[string]any{"node": "node.invalid"},
		"systemSshUsers":        map[string]any{},
		"systemRemoteSudoHosts": []any{},
		"systemActivationModes": map[string]any{"node": "boot"},
	}
}

func encode(t *testing.T, fields map[string]any) []byte {
	t.Helper()
	data, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestParseInventoryAcceptsValidDocument(t *testing.T) {
	fields := validFields()
	fields["hostColors"] = nil
	fields["serialSystemHosts"] = []any{"node"}
	fields["serialReadyCommand"] = "true"
	fields["serialReadyTimeoutSeconds"] = 30.0
	inv, err := parseInventory(encode(t, fields))
	if err != nil {
		t.Fatal(err)
	}
	if inv.canonical("edge") != "node" || inv.canonical("xyz") != "xyz" {
		t.Errorf("aliases not resolved: %v", inv.Aliases)
	}
	if inv.systemEndpoint("node") != "root@node.invalid" || inv.homeEndpoint("xyz") != "alc@xyz" {
		t.Errorf("unexpected endpoints")
	}
	if inv.activationMode("node") != "boot" || inv.activationMode("xyz") != "switch" {
		t.Errorf("unexpected activation modes")
	}
	group := inv.group("node")
	if group == nil || group.ReadyTimeout != 30 || group.ReadyCommand != "true" || inv.group("xyz") != nil {
		t.Errorf("serial shorthand not loaded: %+v", inv.SerialGroups)
	}
}

func TestParseInventorySerialGroups(t *testing.T) {
	fields := validFields()
	fields["serialSystemHosts"] = []any{"node"}
	fields["serialGroups"] = []any{
		map[string]any{"hosts": []any{"xyz"}, "readyCommand": "dig @{host}", "readyTimeoutSeconds": 45},
	}
	inv, err := parseInventory(encode(t, fields))
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.SerialGroups) != 2 || inv.group("node") != inv.SerialGroups[0] {
		t.Fatalf("shorthand must come first: %+v", inv.SerialGroups)
	}
	dns := inv.group("xyz")
	if dns == nil || dns.ReadyTimeout != 45 || dns.readyCommand("xyz") != "dig @xyz" {
		t.Errorf("group not loaded: %+v", dns)
	}
	if inv.SerialGroups[0].ReadyCommand != "" || inv.SerialGroups[0].ReadyTimeout != 600 {
		t.Errorf("shorthand defaults: %+v", inv.SerialGroups[0])
	}
}

func TestParseInventoryDefaultsSerialTimeout(t *testing.T) {
	inv, err := parseInventory(encode(t, validFields()))
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.SerialGroups) != 0 {
		t.Errorf("unexpected serial groups: %+v", inv.SerialGroups)
	}
}

func TestParseInventoryRejects(t *testing.T) {
	cases := []struct {
		name   string
		change func(map[string]any)
		want   string
	}{
		{"missing version", func(f map[string]any) { delete(f, "schemaVersion") }, "not valid JSON"},
		{"false version", func(f map[string]any) { f["schemaVersion"] = false }, "not valid JSON"},
		{"string version", func(f map[string]any) { f["schemaVersion"] = "1" }, "does not satisfy"},
		{"newer version", func(f map[string]any) { f["schemaVersion"] = 2 }, "unsupported inventory schemaVersion '2'"},
		{"no known hosts", func(f map[string]any) { f["knownHosts"] = []any{} }, "does not satisfy"},
		{"duplicate host", func(f map[string]any) { f["deployAllHosts"] = []any{"xyz", "xyz"} }, "does not satisfy"},
		{"unknown host", func(f map[string]any) { f["remoteHosts"] = []any{"missing"} }, "does not satisfy"},
		{"operator unknown", func(f map[string]any) { f["operatorHost"] = "missing" }, "does not satisfy"},
		{"alias shadows host", func(f map[string]any) { f["aliases"] = map[string]any{"node": "xyz"} }, "does not satisfy"},
		{"shell in endpoint", func(f map[string]any) { f["sshHosts"] = map[string]any{"node": "node;touch"} }, "does not satisfy"},
		{"trailing newline in name", func(f map[string]any) { f["homeManagerUser"] = "alc\n" }, "does not satisfy"},
		{"unknown mode", func(f map[string]any) { f["systemActivationModes"] = map[string]any{"node": "test"} }, "does not satisfy"},
		{"color out of range", func(f map[string]any) { f["hostColors"] = map[string]any{"node": "1;2;256"} }, "does not satisfy"},
		{"null serial hosts", func(f map[string]any) { f["serialSystemHosts"] = nil }, "does not satisfy"},
		{"list ready command", func(f map[string]any) { f["serialReadyCommand"] = []any{"true"} }, "does not satisfy"},
		{"fractional timeout", func(f map[string]any) { f["serialReadyTimeoutSeconds"] = 1.5 }, "does not satisfy"},
		{"zero timeout", func(f map[string]any) { f["serialReadyTimeoutSeconds"] = 0 }, "does not satisfy"},
		{"missing map", func(f map[string]any) { delete(f, "sshHosts") }, "does not satisfy"},
		{"group without hosts", func(f map[string]any) { f["serialGroups"] = []any{map[string]any{"readyCommand": "true"}} }, "does not satisfy"},
		{"group with empty hosts", func(f map[string]any) { f["serialGroups"] = []any{map[string]any{"hosts": []any{}}} }, "does not satisfy"},
		{"group with unknown key", func(f map[string]any) {
			f["serialGroups"] = []any{map[string]any{"hosts": []any{"node"}, "ready": "true"}}
		}, "does not satisfy"},
		{"group with unknown host", func(f map[string]any) { f["serialGroups"] = []any{map[string]any{"hosts": []any{"missing"}}} }, "does not satisfy"},
		{"host in two groups", func(f map[string]any) {
			f["serialSystemHosts"] = []any{"node"}
			f["serialGroups"] = []any{map[string]any{"hosts": []any{"node"}}}
		}, "does not satisfy"},
		{"group with fractional timeout", func(f map[string]any) {
			f["serialGroups"] = []any{map[string]any{"hosts": []any{"node"}, "readyTimeoutSeconds": 0.5}}
		}, "does not satisfy"},
		{"groups not a list", func(f map[string]any) { f["serialGroups"] = map[string]any{} }, "does not satisfy"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fields := validFields()
			c.change(fields)
			_, err := parseInventory(encode(t, fields))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error containing %q", err, c.want)
			}
		})
	}
	if _, err := parseInventory([]byte("not-json")); err != errNoSchemaVersion {
		t.Errorf("got %v for invalid JSON", err)
	}
	if _, err := parseInventory([]byte("[]")); err != errNoSchemaVersion {
		t.Errorf("got %v for a JSON array", err)
	}
}
