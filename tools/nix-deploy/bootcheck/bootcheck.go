// Package bootcheck tells whether a NixOS host needs a reboot: whether the
// kernel, initrd, kernel modules or kernel parameters it booted differ from
// those of the system it runs now or of the system its next boot loads.
package bootcheck

import (
	"strings"
)

// Reboot verdicts. Unknown is never a reason to skip a reboot.
const (
	Needed    = "yes"
	NotNeeded = "no"
	Unknown   = "unknown"
)

// Entries are the parts of a NixOS system that only a boot loads. The first
// three are store links; kernel-params is a file whose content is compared.
var Entries = []string{"kernel", "initrd", "kernel-modules", "kernel-params"}

// Script prints the host's kernel name (uname -s) and, on Linux, the store
// paths of the booted system, the current system and the system profile (what
// the next boot loads), each with its boot links resolved and its kernel
// parameters on one line after "p:", one key=value line each; "-" marks a
// path that does not resolve. It is POSIX shell, only reads, and contains no
// single quotes, so RemoteCommand can quote it.
const Script = `os=$(uname -s); echo "os=$os"; [ "$os" = Linux ] || exit 0; ` +
	`show() { echo "$1=$(readlink -e "$2" || echo -)"; for e in kernel initrd kernel-modules; do echo "$1-$e=$(readlink -e "$2/$e" || echo -)"; done; ` +
	`if p=$(cat "$2/kernel-params" 2>/dev/null); then echo "$1-kernel-params=p:$(printf "%s" "$p" | tr "\n" " ")"; else echo "$1-kernel-params=-"; fi; }; ` +
	`show booted /run/booted-system; show current /run/current-system; show profile /nix/var/nix/profiles/system`

// RemoteCommand runs script under sh, whatever the remote login shell is. The
// script must not contain single quotes.
func RemoteCommand(script string) string {
	return "sh -c '" + script + "'"
}

// State is what Script reported, and the verdict drawn from it.
type State struct {
	// OS is the kernel name, such as Linux or Darwin; empty when the output
	// did not say.
	OS string
	// Booted, Current and Profile are the system store paths; empty when
	// unknown.
	Booted  string
	Current string
	Profile string
	// Reboot is Needed, NotNeeded or Unknown.
	Reboot string
	// Differs lists the entries whose booted path differs from the current
	// or the profile's; Missing those that could not be compared because a
	// side did not resolve.
	Differs []string
	Missing []string
}

// Parse reads Script's output. A known difference is enough for Needed; any
// entry that cannot be compared otherwise makes the verdict Unknown, never
// NotNeeded. Lines it does not know are ignored.
func Parse(output string) State {
	values := map[string]string{}
	for _, line := range strings.Split(output, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok {
			values[key] = value
		}
	}
	s := State{
		OS:      values["os"],
		Booted:  path(values["booted"]),
		Current: path(values["current"]),
		Profile: path(values["profile"]),
	}
	if s.OS != "Linux" {
		s.Reboot = Unknown
		return s
	}
	for _, entry := range Entries {
		read := path
		if entry == "kernel-params" {
			read = params
		}
		booted := read(values["booted-"+entry])
		current := read(values["current-"+entry])
		profile := read(values["profile-"+entry])
		switch {
		case booted != "" && current != "" && booted != current,
			booted != "" && profile != "" && booted != profile:
			s.Differs = append(s.Differs, entry)
		case booted == "" || current == "" || profile == "":
			s.Missing = append(s.Missing, entry)
		}
	}
	switch {
	case len(s.Differs) > 0:
		s.Reboot = Needed
	case len(s.Missing) > 0:
		s.Reboot = Unknown
	default:
		s.Reboot = NotNeeded
	}
	return s
}

// path returns a store path as given, or "" for "-", an empty value or
// anything else that is not an absolute path.
func path(value string) string {
	if !strings.HasPrefix(value, "/") {
		return ""
	}
	return value
}

// params returns kernel parameters as Script printed them, with their "p:"
// marker so that empty parameters still compare, or "" when unknown.
func params(value string) string {
	if !strings.HasPrefix(value, "p:") {
		return ""
	}
	return value
}

// Reason explains the verdict in a few words.
func (s State) Reason() string {
	switch {
	case s.OS == "":
		return "the host did not report its systems"
	case s.OS != "Linux":
		return s.OS + " host, not NixOS"
	case s.Reboot == Needed:
		verb := " differ"
		if len(s.Differs) == 1 {
			verb = " differs"
		}
		return strings.Join(s.Differs, ", ") + verb + " between the booted system and the current or next-boot system"
	case s.Reboot == Unknown:
		return "cannot compare " + strings.Join(s.Missing, ", ") + ": missing from the booted, current or next-boot system"
	default:
		return "the current and next-boot systems use the booted kernel, initrd, kernel modules and kernel parameters"
	}
}
