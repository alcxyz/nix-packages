package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const reservationsUsage = `Usage: nixfleet reservations [list] [--config FILE]
       nixfleet reservations release ID [--config FILE] [--confirm-stopped]

List durable serial deployment reservations, or release a stopped invocation.
Release checks the recorded controller process identity and every member for
activation processes and pending system jobs. Positive activity always refuses
release. Failed or unavailable inspection requires --confirm-stopped, an explicit
attestation that the controller and remote work have stopped. Nothing expires.
The authority records releases in its audit log. No deployment is performed.
`

type reservationInfo struct {
	Member             string            `json:"member"`
	Group              []string          `json:"group,omitempty"`
	Controller         string            `json:"controller"`
	ControllerEndpoint string            `json:"controllerEndpoint,omitempty"`
	PID                int               `json:"pid"`
	PIDNamespace       string            `json:"pidNamespace,omitempty"`
	Started            string            `json:"started,omitempty"`
	Created            time.Time         `json:"created"`
	Reason             string            `json:"reason,omitempty"`
	ActiveHost         string            `json:"activeHost,omitempty"`
	Endpoints          map[string]string `json:"endpoints,omitempty"`
}

type heldReservation struct {
	ID      string
	Owner   string
	Info    reservationInfo
	Problem string
}

var reservationID = regexp.MustCompile(`^[a-f0-9]{64}$`)

func (d *deployer) reservationCommand(id string) string {
	return "nixfleet reservations release " + id + " --config " + shellQuote(d.configFile)
}

func (d *deployer) authority(ctx context.Context, script string) (string, int) {
	// A successful SSH exit does not prove stdin reached the remote shell
	// (for example, StdinNull or -n discards it). Bind a fresh acknowledgement
	// to this exact operation, including the ownership token in its payload.
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", 1
	}
	ack := fmt.Sprintf("nixfleet-authority-ack:%x:%x", sha256.Sum256([]byte(script)), nonce)
	script = "(\n" + script + "\n)\nstatus=$?\n[ \"$status\" -eq 0 ] || exit \"$status\"\nprintf '\\n%s\\n' " + shellQuote(ack) + "\n"
	authority := d.inv.OperatorHost
	guard := "umask 077; mkdir -p " + coordinationDir + " && exec flock -w 10 " + coordinationDir + "/.operations.lock sh -s"
	remote := "sh -c " + shellQuote(guard)
	if d.inv.usesRemoteSudo(authority) {
		remote = "sudo -n " + remote
	}
	argv := append([]string{"ssh", "-o", "BatchMode=yes"}, d.sshArgs...)
	argv = append(argv, d.inv.systemEndpoint(authority), remote)
	var output bytes.Buffer
	status := command{ctx: ctx, argv: argv, stdin: strings.NewReader(script), stdout: &output, timeout: 30 * time.Second}.run()
	result := output.String()
	if status == 0 {
		var acknowledged bool
		result, acknowledged = strings.CutSuffix(result, "\n"+ack+"\n")
		if !acknowledged {
			return result, 1
		}
	}
	return result, status
}

func (d *deployer) listReservations() ([]heldReservation, error) {
	script := `# nixfleet reservation list
set -eu
base=` + coordinationDir + `
[ -d "$base" ] || exit 0
for dir in "$base"/*; do
 [ -d "$dir" ] || continue
 key=${dir##*/}
 case "$key" in *[!a-f0-9]*|'') continue;; esac
 [ "${#key}" -eq 64 ] || continue
 owner=-
 if [ -f "$dir/owner" ]; then owner=$(cat "$dir/owner"); fi
 printf '%s\t%s\t' "$key" "$owner"
 if [ -f "$dir/metadata" ]; then tr -d '\n' < "$dir/metadata"; else printf '{}'; fi
 printf '\n'
done
`
	out, status := d.authority(runContext, script)
	if status != 0 {
		return nil, fmt.Errorf("cannot inspect shared reservations (exit %d)", status)
	}
	var result []heldReservation
	scan := bufio.NewScanner(strings.NewReader(out))
	scan.Buffer(make([]byte, 4096), 1024*1024)
	for scan.Scan() {
		fields := strings.SplitN(scan.Text(), "\t", 3)
		if len(fields) != 3 || !reservationID.MatchString(fields[0]) {
			return nil, fmt.Errorf("invalid authority reservation listing")
		}
		item := heldReservation{ID: fields[0], Owner: fields[1]}
		if item.Owner == "-" {
			item.Owner = ""
		}
		if err := json.Unmarshal([]byte(fields[2]), &item.Info); err != nil {
			item.Problem = "unreadable metadata"
		}
		member := d.reservationMember(item.ID)
		if member == "" || item.Info.Member != member {
			item.Problem = "member identity is incomplete or inconsistent with this inventory"
		}
		result = append(result, item)
	}
	return result, scan.Err()
}

// probeHost consumes only process identity/state, never process environments.
// Work inspection prints no process arguments; unknown or incomplete inspection
// cannot establish idleness. Root visibility is required on activation targets.
func (d *deployer) probeHost(host, script string) (string, error) {
	if !contains(d.inv.KnownHosts, host) {
		return "unknown", fmt.Errorf("host is absent from this inventory")
	}
	remote := "sh -s"
	if d.inv.usesRemoteSudo(host) {
		remote = "sudo -n sh -s"
	}
	argv := append([]string{"ssh", "-o", "BatchMode=yes"}, d.sshArgs...)
	argv = append(argv, d.inv.systemEndpoint(host), remote)
	return runProbe(argv, script)
}

func runProbe(argv []string, script string) (string, error) {
	var output bytes.Buffer
	status := command{argv: argv, stdin: strings.NewReader(script), stdout: &output, timeout: 15 * time.Second}.run()
	state := strings.TrimSpace(output.String())
	// Positive evidence still blocks release if transport fails after the
	// probe reported it. Only complete, successful inspection proves idleness.
	if state == "busy" {
		return "busy", nil
	}
	if status != 0 {
		return "unknown", fmt.Errorf("inspection failed (exit %d)", status)
	}
	if state != "idle" && state != "busy" && state != "gone" && state != "unknown" {
		return "unknown", fmt.Errorf("incomplete inspection")
	}
	return state, nil
}

const activationProbe = `# nixfleet activation probe
set -eu
[ "$(id -u)" = 0 ] || { echo unknown; exit 0; }
ps_status=0
processes=$(ps -A -ww -o pid= -o comm= -o args=) || ps_status=$?
scan_status=0
printf '%s\n' "$processes" | awk '
 $1 !~ /^[0-9]+$/ || NF < 3 { invalid=1 }
 $2 !~ /(^|\/)(awk|gawk|mawk|ps)$/ &&
 $0 ~ /(nixos-rebuild|darwin-rebuild|switch-to-configuration|\/activate([[:space:]]|$))/ { busy=1 }
 END { if (busy) exit 0; if (!NR || invalid) exit 2; exit 1 }
' || scan_status=$?
case "$scan_status" in 0) echo busy; exit 0;; 1) ;; *) exit 1;; esac
[ "$ps_status" -eq 0 ] || exit 1
case "$(uname -s)" in
 Linux)
  jobs=$(systemctl list-jobs --no-legend --no-pager) || exit 1
  [ -z "$jobs" ] || { echo busy; exit 0; }
  ;;
 Darwin) ;;
 *) echo unknown; exit 0;;
esac
echo idle
`

func (d *deployer) controllerState(info reservationInfo) (string, error) {
	if info.PID <= 1 || !contains(d.inv.KnownHosts, info.Controller) || info.ControllerEndpoint != d.inv.systemEndpoint(info.Controller) {
		return "unknown", fmt.Errorf("controller identity is incomplete")
	}
	// ps distinguishes absence (successful empty output) from an inspection
	// failure by first obtaining a complete PID list, then reading the identity.
	script := `# nixfleet controller probe
set -eu
TZ=UTC; LC_ALL=C; export TZ LC_ALL
case "$(uname -s)" in
 Linux)
  [ "$(id -u)" = 0 ] || { echo unknown; exit 0; }
  namespace=$(readlink /proc/self/ns/pid) || exit 1
  [ "$namespace" = ` + shellQuote(info.PIDNamespace) + ` ] || { echo unknown; exit 0; }
  ;;
 Darwin) [ ` + shellQuote(info.PIDNamespace) + ` = darwin ] || { echo unknown; exit 0; };;
 *) echo unknown; exit 0;;
esac
ps_status=0
pids=$(ps -A -o pid=) || ps_status=$?
scan_status=0
printf '%s\n' "$pids" | awk '
 $1 == ` + strconv.Itoa(info.PID) + ` { found=1 }
 $1 !~ /^[0-9]+$/ || NF != 1 { invalid=1 }
 END { if (found) exit 0; if (!NR || invalid) exit 2; exit 1 }
' || scan_status=$?
case "$scan_status" in
 0) ;;
 1) [ "$ps_status" -eq 0 ] || exit 1; echo gone; exit 0;;
 *) exit 1;;
esac
started=$(ps -p ` + strconv.Itoa(info.PID) + ` -o lstart=) || :
[ -n "$started" ] || { echo unknown; exit 0; }
started=$(printf '%s\n' "$started" | awk '{$1=$1; print}')
`
	if info.Started == "" {
		script += "echo unknown\n"
	} else {
		// Wall-clock steps can change ps lstart for a live process. A mismatch
		// alone cannot prove PID reuse; require explicit confirmation instead.
		script += "if [ \"$started\" = " + shellQuote(info.Started) + " ]; then echo busy; else echo unknown; fi\n"
	}
	return d.probeHost(info.Controller, script)
}

// inspectWork is deliberately conservative: all configured group members are
// required and pending system jobs also count as potentially active work.
func (d *deployer) inspectWork(members []string, endpoints map[string]string) (string, string) {
	unknown := ""
	if len(members) == 0 {
		return "unknown", "group membership is unavailable"
	}
	for _, host := range members {
		var state string
		var err error
		if host != "" && host == d.self {
			// This invocation knows its own controller. Inspect locally instead
			// of requiring inbound root SSH, including outside-inventory clients.
			argv := []string{"bash", "-s"}
			if os.Geteuid() != 0 {
				argv = append([]string{"sudo", "-n"}, argv...)
			}
			state, err = runProbe(argv, activationProbe)
		} else {
			if !contains(d.inv.KnownHosts, host) {
				unknown = "inventory endpoint unavailable for " + host
				continue
			}
			if endpoints[host] != d.inv.systemEndpoint(host) {
				unknown = "inventory endpoint changed or was not recorded for " + host
			}
			// Confirmation never overrides positive work at a known endpoint.
			state, err = d.probeHost(host, activationProbe)
		}
		if state == "busy" {
			return "busy", "activation processes or system jobs may still be running on " + host
		}
		if err != nil || state != "idle" {
			unknown = "cannot establish that activation work stopped on " + host
		}
	}
	if unknown != "" {
		return "unknown", unknown
	}
	return "idle", "all member inspections report idle"
}

func (d *deployer) releaseHeld(records []heldReservation, id string, confirmed bool, out io.Writer) error {
	var selected *heldReservation
	for i := range records {
		if records[i].ID == id {
			selected = &records[i]
			break
		}
	}
	if selected == nil {
		return fmt.Errorf("reservation %s was not found; list with nixfleet reservations --config %s", id, shellQuote(d.configFile))
	}
	var held []heldReservation
	for _, item := range records {
		if item.ID == id || (selected.Owner != "" && item.Owner == selected.Owner) {
			held = append(held, item)
		}
	}
	unknown := ""
	members := []string{}
	endpoints := map[string]string{}
	for _, item := range held {
		info := item.Info
		if item.Problem != "" {
			unknown = item.Problem
		}
		state, err := d.controllerState(info)
		if state == "busy" {
			return fmt.Errorf("release refused: recorded controller process %s/%d is still running", info.Controller, info.PID)
		}
		if err != nil || state != "gone" {
			unknown = "cannot establish that the recorded controller process stopped"
		}
		actualMember := d.reservationMember(item.ID)
		if actualMember != "" {
			if !contains(members, actualMember) {
				members = append(members, actualMember)
			}
			if group := d.inv.group(actualMember); group != nil {
				for _, member := range group.Hosts {
					if !contains(members, member) {
						members = append(members, member)
					}
				}
			}
		} else {
			unknown = "reservation member is absent from this inventory"
		}
		for _, member := range info.Group {
			if !contains(members, member) {
				members = append(members, member)
			}
		}
		if len(info.Group) == 0 && info.Member != "" && !contains(members, info.Member) {
			members = append(members, info.Member)
		}
		// A dead controller can leave its rebuild frontend alive. Check that
		// host too, before treating idle targets as proof that no work can resume.
		if !contains(members, info.Controller) {
			members = append(members, info.Controller)
		}
		if info.ControllerEndpoint != "" {
			endpoints[info.Controller] = info.ControllerEndpoint
		}
		for host, endpoint := range info.Endpoints {
			if prior, exists := endpoints[host]; exists && prior != endpoint {
				unknown = "inconsistent recorded endpoints"
			}
			endpoints[host] = endpoint
		}
	}
	state, reason := d.inspectWork(members, endpoints)
	if state == "busy" {
		return fmt.Errorf("release refused: %s", reason)
	}
	if state != "idle" {
		unknown = reason
	}
	if unknown != "" && !confirmed {
		return fmt.Errorf("release refused: %s; after independently confirming the controller and remote work stopped, repeat with --confirm-stopped", unknown)
	}
	for _, item := range held {
		if err := d.removeReservation(item, "operator release", confirmed); err != nil {
			return err
		}
		fmt.Fprintf(out, "Released reservation %s (host %s, controller %s, process %d, confirmed=%t)\n", item.ID, item.Info.Member, item.Info.Controller, item.Info.PID, confirmed)
	}
	return nil
}

func (d *deployer) reservationMember(id string) string {
	for _, host := range d.inv.KnownHosts {
		if reservationKey(host) == id {
			return host
		}
	}
	return ""
}

// The authority serializes every metadata operation under a kernel flock.
// Recheck the listed owner under that lock, then rename before deletion.
func (d *deployer) removeReservation(item heldReservation, reason string, confirmed bool) error {
	if !reservationID.MatchString(item.ID) || (item.Owner != "" && !regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(item.Owner)) {
		return fmt.Errorf("invalid reservation identity")
	}
	dir := coordinationDir + "/" + item.ID
	retired := dir + ".released-" + item.Owner
	if item.Owner == "" {
		retired = dir + ".released-incomplete"
	}
	event := struct {
		ID         string    `json:"reservation"`
		Owner      string    `json:"invocation"`
		Member     string    `json:"member"`
		Controller string    `json:"controller"`
		PID        int       `json:"pid"`
		Reason     string    `json:"reason"`
		Phase      string    `json:"phase"`
		Confirmed  bool      `json:"confirmed"`
		At         time.Time `json:"at"`
	}{ID: item.ID, Owner: item.Owner, Member: item.Info.Member, Controller: item.Info.Controller, PID: item.Info.PID, Reason: reason, Phase: "release-request", Confirmed: confirmed, At: time.Now().UTC()}
	request, _ := json.Marshal(event)
	event.Phase = "released"
	released, _ := json.Marshal(event)
	script := `# nixfleet reservation release
set -eu
umask 077
dir=` + shellQuote(dir) + `
retired=` + shellQuote(retired) + `
owner=''
if [ -f "$dir/owner" ]; then owner=$(cat "$dir/owner"); fi
if [ "$owner" != ` + shellQuote(item.Owner) + ` ]; then exit 75; fi
if [ -e "$retired" ]; then exit 76; fi
if ! printf '%s\n' ` + shellQuote(string(request)) + ` >> ` + coordinationDir + `/releases.jsonl; then exit 77; fi
mv "$dir" "$retired"
printf '%s\n' ` + shellQuote(string(released)) + ` >> ` + coordinationDir + `/releases.jsonl
# Only this invocation's renamed directory is touched below.
for file in "$retired"/*; do [ ! -f "$file" ] || rm "$file"; done
rmdir "$retired"
`
	_, status := d.authority(context.Background(), script)
	if status != 0 {
		return fmt.Errorf("reservation %s release incomplete (exit %d); inspect with nixfleet reservations --config %s", item.ID, status, shellQuote(d.configFile))
	}
	return nil
}

func runReservations(args []string) int {
	config := os.Getenv("NIX_DEPLOY_CONFIG")
	action, id := "list", ""
	confirmed := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--help", "-h":
			fmt.Print(reservationsUsage)
			return 0
		case "--config":
			if i+1 >= len(args) {
				fmt.Fprint(os.Stderr, reservationsUsage)
				return 2
			}
			i++
			config = args[i]
		case "list":
			action = "list"
		case "release":
			action = "release"
		case "--confirm-stopped":
			confirmed = true
		default:
			if id != "" || !reservationID.MatchString(args[i]) {
				fmt.Fprint(os.Stderr, reservationsUsage)
				return 2
			}
			id = args[i]
		}
	}
	if config == "" || (action == "release" && id == "") || (action == "list" && (id != "" || confirmed)) {
		fmt.Fprint(os.Stderr, reservationsUsage)
		return 2
	}
	inv, err := loadInventory(config)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	options := os.Getenv("NIX_SSHOPTS")
	if options == "" {
		options = defaultSSHOptions
	}
	d := &deployer{inv: inv, configFile: config, sshArgs: strings.Fields(options)}
	held, err := d.listReservations()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if action == "release" {
		if err := d.releaseHeld(held, id, confirmed, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
	if len(held) == 0 {
		fmt.Println("No held reservations.")
		return 0
	}
	for _, item := range held {
		age := "unknown"
		if !item.Info.Created.IsZero() {
			age = time.Since(item.Info.Created).Round(time.Second).String()
		}
		reason := item.Info.Reason
		if reason == "" {
			reason = "legacy or interrupted reservation; reason unavailable"
		}
		if item.Problem != "" {
			reason = item.Problem
		}
		fmt.Printf("%s host=%s group=%s controller=%s process=%d age=%s reason=%s\n  %s\n", item.ID, item.Info.Member, strings.Join(item.Info.Group, ","), item.Info.Controller, item.Info.PID, age, reason, d.reservationCommand(item.ID))
	}
	return 0
}
