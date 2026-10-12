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
	if inv.SerialReadyTimeout != 30 || !inv.isSerial("node") {
		t.Errorf("serial settings not loaded: %+v", inv)
	}
}

func TestParseInventoryDefaultsSerialTimeout(t *testing.T) {
	inv, err := parseInventory(encode(t, validFields()))
	if err != nil {
		t.Fatal(err)
	}
	if inv.SerialReadyTimeout != 600 || len(inv.SerialSystemHosts) != 0 || inv.SerialReadyCommand != "" {
		t.Errorf("unexpected serial defaults: %+v", inv)
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
