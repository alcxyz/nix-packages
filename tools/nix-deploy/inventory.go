package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// inventory is schema version 1 of the deploy inventory (ADR-0006).
type inventory struct {
	OperatorHost          string
	HomeManagerUser       string
	HomeOutputPrefix      string
	KnownHosts            []string
	HomeManagerHosts      []string
	RemoteHosts           []string
	DeployAllHosts        []string
	Aliases               map[string]string
	SSHHosts              map[string]string
	SystemSSHUsers        map[string]string
	SystemRemoteSudoHosts []string
	SystemActivationModes map[string]string
	HostColors            map[string]string
	SerialSystemHosts     []string
	SerialReadyCommand    string
	SerialReadyTimeout    int
}

// Names, users and prefixes become command arguments and flake attribute
// names, and endpoints become SSH destinations, so both are restricted to the
// characters the command interface needs.
var (
	safeName     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	safeEndpoint = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]*$`)
	validColor   = regexp.MustCompile(`^[0-9]{1,3};[0-9]{1,3};[0-9]{1,3}$`)
)

var (
	errNoSchemaVersion = errors.New("inventory is not valid JSON with a schemaVersion")
	errInvalidSchema   = errors.New("inventory does not satisfy schemaVersion 1")
)

func parseInventory(data []byte) (*inventory, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return nil, errNoSchemaVersion
	}
	version, err := schemaVersion(fields["schemaVersion"])
	if err != nil {
		return nil, err
	}
	if version != "1" {
		return nil, fmt.Errorf("unsupported inventory schemaVersion '%s' (expected 1)", version)
	}
	inv, ok := validateInventory(fields)
	if !ok {
		return nil, errInvalidSchema
	}
	return inv, nil
}

// schemaVersion renders the version the way the reference implementation
// printed it, so a string "1" passes this step and then fails validation.
func schemaVersion(raw json.RawMessage) (string, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if len(raw) == 0 || decoder.Decode(&value) != nil {
		return "", errNoSchemaVersion
	}
	switch v := value.(type) {
	case nil:
		return "", errNoSchemaVersion
	case bool:
		if !v {
			return "", errNoSchemaVersion
		}
		return "true", nil
	case string:
		return v, nil
	case json.Number:
		return v.String(), nil
	default:
		return string(raw), nil
	}
}

func validateInventory(fields map[string]json.RawMessage) (*inventory, bool) {
	inv := &inventory{SerialReadyTimeout: 600}
	ok := true
	check := func(valid bool) {
		if !valid {
			ok = false
		}
	}

	var version json.Number
	check(decodeNumber(fields["schemaVersion"], &version) && version.String() == "1")

	known, valid := uniqueStrings(fields["knownHosts"])
	check(valid && len(known) > 0 && allMatch(known, safeName))
	inv.KnownHosts = known

	inv.OperatorHost, valid = safeString(fields["operatorHost"])
	check(valid && contains(known, inv.OperatorHost))
	inv.HomeManagerUser, valid = safeString(fields["homeManagerUser"])
	check(valid)
	inv.HomeOutputPrefix, valid = safeString(fields["homeOutputPrefix"])
	check(valid)

	inv.HomeManagerHosts, valid = knownList(fields["homeManagerHosts"], known)
	check(valid)
	inv.RemoteHosts, valid = knownList(fields["remoteHosts"], known)
	check(valid)
	inv.DeployAllHosts, valid = knownList(fields["deployAllHosts"], known)
	check(valid)
	inv.SystemRemoteSudoHosts, valid = knownList(fields["systemRemoteSudoHosts"], known)
	check(valid)

	inv.Aliases, valid = stringMap(fields["aliases"])
	for alias, host := range inv.Aliases {
		// An alias must not shadow a canonical name.
		valid = valid && safeName.MatchString(alias) && !contains(known, alias) && contains(known, host)
	}
	check(valid)

	inv.SSHHosts, valid = stringMap(fields["sshHosts"])
	for host, endpoint := range inv.SSHHosts {
		valid = valid && contains(known, host) && safeEndpoint.MatchString(endpoint)
	}
	check(valid)

	inv.SystemSSHUsers, valid = stringMap(fields["systemSshUsers"])
	for host, user := range inv.SystemSSHUsers {
		valid = valid && contains(known, host) && safeName.MatchString(user)
	}
	check(valid)

	inv.SystemActivationModes, valid = stringMap(fields["systemActivationModes"])
	for host, mode := range inv.SystemActivationModes {
		valid = valid && contains(known, host) && (mode == "switch" || mode == "boot")
	}
	check(valid)

	inv.HostColors = map[string]string{}
	if raw := fields["hostColors"]; !isNullOrFalse(raw) {
		inv.HostColors, valid = stringMap(raw)
		for host, color := range inv.HostColors {
			valid = valid && contains(known, host) && isColor(color)
		}
		check(valid)
	}

	if raw, present := fields["serialSystemHosts"]; present {
		inv.SerialSystemHosts, valid = knownList(raw, known)
		check(valid)
	}
	if raw, present := fields["serialReadyCommand"]; present {
		check(json.Unmarshal(raw, &inv.SerialReadyCommand) == nil && isString(raw))
	}
	if raw, present := fields["serialReadyTimeoutSeconds"]; present {
		var number json.Number
		valid = decodeNumber(raw, &number)
		if valid {
			seconds, err := number.Float64()
			valid = err == nil && seconds == math.Floor(seconds) && seconds > 0 && seconds <= math.MaxInt32
			inv.SerialReadyTimeout = int(seconds)
		}
		check(valid)
	}

	return inv, ok
}

func decodeNumber(raw json.RawMessage, number *json.Number) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if len(raw) == 0 || decoder.Decode(&value) != nil {
		return false
	}
	n, ok := value.(json.Number)
	if ok {
		*number = n
	}
	return ok
}

func isNullOrFalse(raw json.RawMessage) bool {
	text := strings.TrimSpace(string(raw))
	return text == "" || text == "null" || text == "false"
}

func isString(raw json.RawMessage) bool {
	return strings.HasPrefix(strings.TrimSpace(string(raw)), `"`)
}

func safeString(raw json.RawMessage) (string, bool) {
	var value string
	if !isString(raw) || json.Unmarshal(raw, &value) != nil {
		return "", false
	}
	return value, safeName.MatchString(value)
}

func uniqueStrings(raw json.RawMessage) ([]string, bool) {
	if !strings.HasPrefix(strings.TrimSpace(string(raw)), "[") {
		return nil, false
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return nil, false
	}
	values := make([]string, 0, len(items))
	for _, item := range items {
		var value string
		if !isString(item) || json.Unmarshal(item, &value) != nil || contains(values, value) {
			return nil, false
		}
		values = append(values, value)
	}
	return values, true
}

func knownList(raw json.RawMessage, known []string) ([]string, bool) {
	values, ok := uniqueStrings(raw)
	for _, value := range values {
		ok = ok && contains(known, value)
	}
	return values, ok
}

func stringMap(raw json.RawMessage) (map[string]string, bool) {
	if !strings.HasPrefix(strings.TrimSpace(string(raw)), "{") {
		return nil, false
	}
	var items map[string]json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return nil, false
	}
	values := make(map[string]string, len(items))
	for key, item := range items {
		var value string
		if !isString(item) || json.Unmarshal(item, &value) != nil {
			return nil, false
		}
		values[key] = value
	}
	return values, true
}

func isColor(value string) bool {
	if !validColor.MatchString(value) {
		return false
	}
	for _, part := range strings.Split(value, ";") {
		n, err := strconv.Atoi(part)
		if err != nil || n > 255 {
			return false
		}
	}
	return true
}

func allMatch(values []string, pattern *regexp.Regexp) bool {
	for _, value := range values {
		if !pattern.MatchString(value) {
			return false
		}
	}
	return true
}

func contains(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}

func (inv *inventory) canonical(host string) string {
	if target, ok := inv.Aliases[host]; ok {
		return target
	}
	return host
}

func (inv *inventory) sshHost(host string) string {
	if endpoint, ok := inv.SSHHosts[host]; ok {
		return endpoint
	}
	return host
}

func (inv *inventory) systemUser(host string) string {
	if user, ok := inv.SystemSSHUsers[host]; ok {
		return user
	}
	return "root"
}

func (inv *inventory) systemEndpoint(host string) string {
	return inv.systemUser(host) + "@" + inv.sshHost(host)
}

func (inv *inventory) homeEndpoint(host string) string {
	return inv.HomeManagerUser + "@" + inv.sshHost(host)
}

func (inv *inventory) activationMode(host string) string {
	if mode, ok := inv.SystemActivationModes[host]; ok {
		return mode
	}
	return "switch"
}

func (inv *inventory) usesRemoteSudo(host string) bool {
	return contains(inv.SystemRemoteSudoHosts, host)
}

func (inv *inventory) usesHomeManager(host string) bool {
	return contains(inv.HomeManagerHosts, host)
}

func (inv *inventory) isSerial(host string) bool {
	return contains(inv.SerialSystemHosts, host)
}
