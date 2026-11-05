package main

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// /run is host-owned and survives controllers and SSH sessions. Its lifetime
// is exactly one boot: normal boot startup remains responsible for the units.
const runnerRebootDir = "/run/nixfleet-reboot-runners"
const hostStoppedRunnerReason = "stopped by hand or by a reboot run; nothing starts it again"
const unknownRunnerReboot = "stopped by a reboot run with an unknown outcome"

type runnerRebootStop struct {
	Owner      string          `json:"owner"`
	Controller reservationInfo `json:"controller"`
	Units      []string        `json:"units"`
}

const runnerRebootReadScript = `
if [ -d ` + runnerRebootDir + ` ]; then
 for field in owner controller units; do
  if [ -r ` + runnerRebootDir + `/$field ]; then
   while IFS= read -r line; do printf "runner-reboot.%s=%s\n" "$field" "$line"; done < ` + runnerRebootDir + `/$field
  else echo runner-reboot.error=unreadable; fi
 done
fi`

func addRunnerRebootStop(r *runnerStatus, output string) *runnerStatus {
	var record runnerRebootStop
	present, problem := false, ""
	for _, line := range strings.Split(output, "\n") {
		field, value, ok := strings.Cut(line, "=")
		if !ok || !strings.HasPrefix(field, "runner-reboot.") {
			continue
		}
		present = true
		switch field {
		case "runner-reboot.owner":
			record.Owner = value
		case "runner-reboot.controller":
			if json.Unmarshal([]byte(value), &record.Controller) != nil {
				problem = "invalid controller identity"
			}
		case "runner-reboot.units":
			if !safeUnit.MatchString(value) {
				problem = "invalid recorded unit"
			} else {
				record.Units = append(record.Units, value)
			}
		case "runner-reboot.error":
			problem = "unreadable record"
		}
	}
	if !present {
		return r
	}
	if r == nil {
		r = &runnerStatus{}
	}
	if len(record.Owner) != 32 || strings.Trim(record.Owner, "0123456789abcdef") != "" || record.Controller.PID <= 1 {
		problem = "incomplete record"
	}
	if problem != "" {
		r.Error = "reboot runner stop: " + problem
		return r
	}
	r.RebootStop = &record
	for i := range r.Units {
		u := &r.Units[i]
		if contains(record.Units, u.Unit) && u.State != "active" {
			if u.Reason != "" && u.Reason != hostStoppedRunnerReason {
				u.Reason = unknownRunnerReboot + "; " + u.Reason
			} else {
				u.Reason = unknownRunnerReboot
			}
		}
	}
	return r
}

func runnerResumeCommand(host, config string) string {
	command := "nixfleet runners resume " + shellQuote(host)
	if config != "" {
		command += " --config " + shellQuote(config)
	}
	return command
}

// All host mutations share a bounded kernel lock. Losing a connection never
// removes the stop record, and a second reboot cannot overwrite its ownership.
func runnerRebootLocked(script string) string {
	return "flock -w 10 " + runnerRebootDir + ".lock sh -c " + shellQuote("set -eu\n"+script)
}

func runnerStopScript(owner string, info reservationInfo, units []string) string {
	data, _ := json.Marshal(info)
	return runnerRebootLocked(`
base=` + runnerRebootDir + `
[ ! -e "$base" ] || { echo 'runner stop refused: a previous reboot run owns a stop record; use nixfleet runners resume' >&2; exit 1; }
# Reject unsupported hosts before publishing a record or stopping any unit.
report=$(` + runnerStatusCommand + `) || { echo 'runner stop refused: a working nixfleet-runner-status report is required before draining runners' >&2; exit 1; }
printf '%s\n' "$report" | awk -F '\t' '$1=="version" && $2=="1" { version=1 } $1=="hold" && $2=="active" && $3!="" && $3!="unknown" && $3!="-" { hold=1 } END { exit !(version && hold) }' || { echo 'runner stop refused: runner hold inspection is unavailable' >&2; exit 1; }
umask 022
tmp=$(mktemp -d "$base.XXXXXX")
trap 'rm -rf "$tmp"' 0
printf '%s\n' ` + shellQuote(owner) + ` > "$tmp/owner"
printf '%s\n' ` + shellQuote(string(data)) + ` > "$tmp/controller"
: > "$tmp/units"
for unit in ` + quoteUnits(units) + `; do
 state=$(systemctl show --property=ActiveState --value -- "$unit")
 case "$state" in
  active) printf '%s\n' "$unit" >> "$tmp/units";;
  inactive|failed) ;;
  *) echo "runner stop refused: $unit is $state" >&2; exit 1;;
 esac
done
chmod 0755 "$tmp"
mv "$tmp" "$base"
# Publish the ownership and exact active subset before any stop can happen.
units=$(cat "$base/units")
[ -z "$units" ] || systemctl stop -- $units
`)
}

// Shutdown inspection must be complete, including scheduled logind shutdowns
// which need not have systemd jobs yet. No override permits an unknown result.
const runnerShutdownCheck = `
[ ! -e /run/systemd/shutdown/scheduled ] || { echo 'resume refused: shutdown is pending' >&2; exit 1; }
jobs=$(systemctl list-jobs --no-legend --no-pager) || { echo 'resume refused: system jobs cannot be inspected' >&2; exit 1; }
[ -z "$jobs" ] || { echo 'resume refused: system jobs are pending (possibly shutdown)' >&2; exit 1; }
state=$(systemctl show --property=ActiveState --value shutdown.target) || { echo 'resume refused: shutdown state cannot be inspected' >&2; exit 1; }
[ "$state" = inactive ] || { echo "resume refused: shutdown.target is $state" >&2; exit 1; }
processes=$(ps -A -ww -o comm= -o args=) || { echo 'resume refused: shutdown processes cannot be inspected' >&2; exit 1; }
printf '%s\n' "$processes" | awk 'NF<2 { invalid=1 } END { exit (invalid || !NR) }' || { echo 'resume refused: shutdown process inspection is incomplete' >&2; exit 1; }
if printf '%s\n' "$processes" | awk '
 $1 !~ /(^|\/)(awk|gawk|mawk|ps)$/ &&
 ($0 ~ /systemctl[[:space:]]+(reboot|poweroff|halt|kexec)/ || $1 ~ /(^|\/)(shutdown|reboot|poweroff|halt)$/) { found=1 }
 END { exit !found }'; then
 echo 'resume refused: a shutdown command is still active' >&2; exit 1
fi
`

// Starting through systemd retains every ExecCondition/start gate. Before
// attempting any starts, require the host's report to confirm no runner hold,
// gate refusal, reset or restart. A failed or skipped start keeps the record.
func runnerStartScript(owner string) string {
	return runnerRebootLocked(`
base=` + runnerRebootDir + `
[ -d "$base" ] || { echo 'no reboot-stopped runners'; exit 0; }
[ "$(cat "$base/owner")" = ` + shellQuote(owner) + ` ] || { echo 'resume refused: stop record changed' >&2; exit 1; }
` + runnerShutdownCheck + `
report=$(` + runnerStatusCommand + `) || { echo 'resume refused: runner status unavailable' >&2; exit 1; }
printf '%s\n' "$report" | awk -F '\t' '
 $1=="version" && $2=="1" { version=1 }
 $1=="hold" && $2=="active" && $3=="none" { hold=1 }
 $1=="restart" && ($2=="activating" || $2=="deactivating" || $2=="unknown") { busy=1 }
 END { exit !(version && hold && !busy) }' || { echo 'resume refused: runner hold or restart is active or unknown' >&2; exit 1; }
restart=$(printf '%s\n' "$report" | awk -F '\t' '$1=="restart-unit" { print $2 }')
case "$restart" in
 ''|-) ;;
 *)
  restart_state=$(systemctl show --property=ActiveState --value -- "$restart") || { echo 'resume refused: runner restart state is unreadable' >&2; exit 1; }
  case "$restart_state" in inactive|failed) ;; *) echo "resume refused: runner restart is $restart_state" >&2; exit 1;; esac
  ;;
esac
units=$(cat "$base/units")
for unit in $units; do
 printf '%s\n' "$unit" | LC_ALL=C awk '$0 ~ /^[A-Za-z0-9@._:\\-]+[.]service$/ { ok=1 } END { exit !ok }' || { echo 'resume refused: invalid recorded unit' >&2; exit 1; }
 load=$(systemctl show --property=LoadState --value -- "$unit")
 [ "$load" != not-found ] || continue
 printf '%s\n' "$report" | NIXFLEET_RECORDED_UNIT="$unit" awk -F '\t' '
  BEGIN { unit=ENVIRON["NIXFLEET_RECORDED_UNIT"] }
  $1=="unit" && $2=="runner" && $3==unit {
   found=1
   if ($4=="active") ok=1
   if ($4=="inactive" && $5=="dead" && $9=="` + hostStoppedRunnerReason + `") ok=1
  }
  END { exit !(found && ok) }' || { echo "resume refused: $unit is held, gated, disabled, busy or unreadable" >&2; exit 1; }
done
for unit in $units; do
 # Removed unit definitions have nothing to resume; never start them.
 load=$(systemctl show --property=LoadState --value -- "$unit")
 [ "$load" != not-found ] || continue
 # Do not restart a unit already started independently.
 state=$(systemctl show --property=ActiveState --value -- "$unit")
 case "$state" in
  active) continue;;
  inactive) ;;
  *) echo "resume refused: $unit changed state to $state" >&2; exit 1;;
 esac
` + runnerShutdownCheck + `
 systemctl start -- "$unit"
 state=$(systemctl show --property=ActiveState --value -- "$unit")
 [ "$state" = active ] || { echo "resume refused: $unit did not start (check its hold or start gate)" >&2; exit 1; }
done
# Retire atomically. Interruption during deletion cannot leave a blocking record.
retired="$base.resumed-"` + shellQuote(owner) + `
mv "$base" "$retired"
rm -rf "$retired"
echo 'resumed recorded reboot-stopped runners'
`)
}

func runRunnerResume(s *statusCheck, states []hostStatus, config string, out io.Writer) int {
	d := &deployer{inv: s.inv, self: s.self, configFile: config, sshArgs: s.sshArgs}
	exit := 0
	for _, state := range states {
		if err := resumeHostRunners(d, s, state); err != nil {
			fmt.Fprintf(out, "[%s] %v\n", state.Host, err)
			exit = 1
		} else {
			fmt.Fprintf(out, "[%s] runner resume complete (only recorded units)\n", state.Host)
		}
	}
	return exit
}

func resumeHostRunners(d *deployer, s *statusCheck, state hostStatus) (result error) {
	if !state.Reachable {
		return fmt.Errorf("resume refused: host unreachable: %s", state.Error)
	}
	r := state.Runners
	if r == nil {
		return nil
	}
	if r.Error != "" {
		return fmt.Errorf("resume refused: %s", r.Error)
	}
	if r.RebootStop == nil {
		return nil
	}
	// Acquire a real reservation, rather than listing then racing a deployment.
	reservation, err := d.reserveGroup(&serialGroup{Hosts: []string{state.Host}})
	if err != nil {
		return fmt.Errorf("resume refused: %v", err)
	}
	reservation.reason = "runner resume completed or refused"
	release := true
	defer func() {
		if release {
			if !reservation.release() && result == nil {
				result = fmt.Errorf("runner resume finished but reservation release failed; next: %s", d.reservationCommand(reservation.keys[0]))
			}
		} else {
			reservation.persist("runner resume outcome unknown on " + state.Host)
			result = fmt.Errorf("%v; protection retained; next: %s", result, d.reservationCommand(reservation.keys[0]))
		}
	}()
	controller, err := d.controllerStateAt(r.RebootStop.Controller, true)
	if controller == "busy" {
		return fmt.Errorf("resume refused: reboot controller %s/%d is still active", r.RebootStop.Controller.Controller, r.RebootStop.Controller.PID)
	}
	if err != nil {
		return fmt.Errorf("resume refused: cannot inspect reboot controller: %v", err)
	}
	if controller != "gone" {
		return fmt.Errorf("resume refused: reboot controller exit cannot be established")
	}
	// A dead controller can leave a detached SSH child that has not sent its
	// reboot command yet. Target-only inspection cannot see that pending work.
	info := r.RebootStop.Controller
	if work, reason := d.inspectWork([]string{info.Controller}, map[string]string{info.Controller: info.ControllerEndpoint}); work != "idle" {
		return fmt.Errorf("resume refused: reboot controller work is not idle: %s", reason)
	}
	nonce, err := newRunnerStopOwner()
	if err != nil {
		return err
	}
	ack := "nixfleet-runner-resume-ack:" + nonce
	command := runnerStartScript(r.RebootStop.Owner) + " && printf '\\n%s\\n' " + shellQuote(ack)
	argv := []string{"sh", "-c", "sudo -n " + command}
	if state.Host != s.self {
		if s.inv.usesRemoteSudo(state.Host) {
			command = "sudo -n " + command
		}
		argv = append([]string{"ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=" + s.connectTimeout}, s.sshArgs...)
		argv = append(argv, s.inv.systemEndpoint(state.Host), command)
	}
	if !reservation.persist("runner resume starting on " + state.Host) {
		return fmt.Errorf("resume refused: authority update unavailable")
	}
	release = false
	stdout, stderr, status := s.run(runnerStartTimeout, argv...)
	if status != 0 && definiteActivationStatus(status) && runContext.Err() == nil {
		release = true
	}
	if status != 0 {
		return fmt.Errorf("resume failed (exit %d): %s %s", status, oneLine(stdout), oneLine(stderr))
	}
	// Empty SSH success is insufficient evidence that the operation executed.
	if !strings.HasSuffix(stdout, "\n"+ack+"\n") {
		return fmt.Errorf("resume result unavailable; read runners status before retrying")
	}
	release = true
	return nil
}

func newRunnerStopOwner() (string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", nonce), nil
}

func resumeUsesLocalSudo(self string, states []hostStatus) bool {
	for _, state := range states {
		if state.Runners != nil && state.Runners.RebootStop != nil && (state.Host == self || state.Runners.RebootStop.Controller.Controller == self) {
			return true
		}
	}
	return false
}
