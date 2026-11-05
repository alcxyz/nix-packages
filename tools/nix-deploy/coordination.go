package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"
)

const coordinationDir = "/var/lib/nix-deploy/coordination"

type reservation struct {
	d         *deployer
	token     string
	keys      []string
	info      reservationInfo
	attempted bool
	ambiguous bool
	reason    string
}

func (d *deployer) coordinationCommand(script string) int {
	_, status := d.authority(runContext, script)
	return status
}

func (d *deployer) reserveGroup(g *serialGroup) (*reservation, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	info, err := controllerIdentity(d.inv, d.self)
	if err != nil {
		return nil, err
	}
	r := &reservation{d: d, token: fmt.Sprintf("%x", nonce)}
	r.info = info
	r.info.Group = append([]string(nil), g.Hosts...)
	r.info.Reason = "reserved; no activation started"
	for _, host := range g.Hosts {
		r.keys = append(r.keys, reservationKey(host))
		r.info.Endpoints[host] = d.inv.systemEndpoint(host)
	}
	sort.Strings(r.keys)
	// Authority operations run under a kernel lock. A failed acquisition rolls
	// back its known partial changes; a lost success acknowledgement retains it.
	script := "# nixfleet reservation acquire\nset -eu\numask 077\nowned=''\n"
	script += "trap 'status=$?; if [ \"$status\" -ne 0 ]; then for dir in $owned; do rm \"$dir/metadata\" \"$dir/owner\" && rmdir \"$dir\"; done; fi; exit \"$status\"' 0\n"
	for _, key := range r.keys {
		info := r.info
		info.Member = r.member(key)
		metadata, _ := json.Marshal(info)
		dir := coordinationDir + "/" + key
		script += "if ! mkdir " + dir + "; then printf 'blocked\\t%s\\n' " + key + "; exit 73; fi\n"
		script += "printf '%s\\n' " + r.token + " > " + dir + "/owner\nprintf '%s\\n' " + shellQuote(string(metadata)) + " > " + dir + "/metadata\nowned=\"$owned " + dir + "\"\n"
	}
	out, status := d.authority(runContext, script)
	if status != 0 {
		for _, line := range strings.Split(out, "\n") {
			if key, ok := strings.CutPrefix(line, "blocked\t"); ok && reservationID.MatchString(key) {
				return nil, fmt.Errorf("shared deployment blocked by reservation %s (host %s); next: %s", key, r.member(key), d.reservationCommand(key))
			}
		}
		return nil, fmt.Errorf("shared deployment coordination unavailable; inspect with nixfleet reservations --config %s; attempted reservations: %s", shellQuote(d.configFile), strings.Join(r.keys, ","))
	}
	return r, nil
}

func (r *reservation) member(key string) string {
	for _, host := range r.info.Group {
		if reservationKey(host) == key {
			return host
		}
	}
	return "unknown"
}

func (r *reservation) persist(reason string) bool {
	r.info.Reason = reason
	script := "# nixfleet reservation update\nset -eu\n"
	for _, key := range r.keys {
		info := r.info
		info.Member = r.member(key)
		data, _ := json.Marshal(info)
		dir := coordinationDir + "/" + key
		script += "test \"$(cat " + dir + "/owner)\" = " + r.token + "\nprintf '%s\\n' " + shellQuote(string(data)) + " > " + dir + "/metadata.tmp\nmv " + dir + "/metadata.tmp " + dir + "/metadata\n"
	}
	_, status := r.d.authority(context.Background(), script)
	if status != 0 {
		return false
	}
	return true
}

func (r *reservation) start(host string) bool {
	if runContext.Err() != nil {
		r.reason = "controller interrupted before activation"
		return false
	}
	r.info.ActiveHost = host
	if !r.persist("activation starting on " + host) {
		r.reason = "authority update unavailable before activation"
		return false
	}
	r.attempted = true
	r.ambiguous = true
	return true
}

func definiteActivationStatus(status int) bool {
	return status == 0 || (status > 0 && status < 128 && status != exitTimedOut)
}

func (r *reservation) ended(status int) bool {
	r.ambiguous = !definiteActivationStatus(status) || runContext.Err() != nil
	r.reason = fmt.Sprintf("activation on %s returned status %d", r.info.ActiveHost, status)
	if r.ambiguous {
		r.reason += "; outcome ambiguous"
	} else {
		r.reason += "; command finished"
	}
	// The failed rollout finalizer records retention once.
	if status != 0 {
		return true
	}
	return r.persist(r.reason)
}

// finish keeps only genuinely uncertain work. A definite failure releases
// automatically once every member is observably idle, permitting a fixing deploy.
func (r *reservation) finish(failed bool) bool {
	if !r.attempted {
		return r.release()
	}
	if !failed && runContext.Err() == nil {
		return r.release()
	}
	reason := r.reason
	if reason == "" {
		reason = "admission failed before activation"
	}
	if !r.ambiguous && runContext.Err() == nil {
		members := append([]string(nil), r.info.Group...)
		if !contains(members, r.info.Controller) {
			members = append(members, r.info.Controller)
		}
		state, inspection := r.d.inspectWork(members, r.info.Endpoints)
		if state == "idle" {
			return r.release()
		}
		reason += "; " + inspection
	}
	if runContext.Err() != nil {
		reason += "; controller interrupted"
	}
	r.persist(reason)
	fmt.Fprintf(os.Stderr, "deploy: retained reservation %s: %s; next: %s\n", r.keys[0], reason, r.d.reservationCommand(r.keys[0]))
	return false
}

func (r *reservation) release() bool {
	for _, key := range r.keys {
		info := r.info
		info.Member = r.member(key)
		if err := r.d.removeReservation(heldReservation{ID: key, Owner: r.token, Info: info}, r.releaseReason(), false); err != nil {
			fmt.Fprintln(os.Stderr, "deploy:", err)
			return false
		}
	}
	return true
}

func (r *reservation) releaseReason() string {
	if r.reason != "" {
		return r.reason + "; protected work completed or inspected idle"
	}
	return "rollout completed or stopped before activation"
}

func reservationKey(host string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(host))) }

func controllerIdentity(inv *inventory, self string) (reservationInfo, error) {
	// The process start identity prevents a reused controller PID from looking
	// like the original deployment. Use the same locale on every controller.
	started, status := capture(5*time.Second, "bash", "-c", fmt.Sprintf("TZ=UTC LC_ALL=C ps -p %d -o lstart=", os.Getpid()))
	if status != 0 || strings.TrimSpace(started) == "" {
		return reservationInfo{}, fmt.Errorf("cannot establish controller process identity")
	}
	namespace := "darwin"
	if runtime.GOOS == "linux" {
		value, code := capture(5*time.Second, "readlink", "/proc/self/ns/pid")
		if code != 0 || strings.TrimSpace(value) == "" {
			return reservationInfo{}, fmt.Errorf("cannot establish controller PID namespace")
		}
		namespace = strings.TrimSpace(value)
	}

	info := reservationInfo{Controller: self, PID: os.Getpid(), Started: strings.Join(strings.Fields(started), " "), PIDNamespace: namespace, Created: time.Now().UTC(), Endpoints: map[string]string{}}
	if contains(inv.KnownHosts, self) {
		info.ControllerEndpoint = inv.systemEndpoint(self)
		info.Endpoints[self] = info.ControllerEndpoint
	}
	return info, nil
}
