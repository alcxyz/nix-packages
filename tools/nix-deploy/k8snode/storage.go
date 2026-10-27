package k8snode

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	// minLonghornReplicas is the replica policy floor for attached volumes.
	minLonghornReplicas = 3
	// minSurvivingLonghornReplicas is the number of other nodes that must
	// keep a healthy replica of every attached volume.
	minSurvivingLonghornReplicas = 2
	// rebuildLimitSetting closes or opens Longhorn's automatic replica
	// rebuild admission.
	rebuildLimitSetting = "concurrent-replica-rebuild-per-node-limit"
)

// storageDetachScript runs as root on the target after the drain. It fails
// on any residual Longhorn mount or open iSCSI device and reconciles only
// validated, detached sessions, removing the Open-iSCSI field that newer
// releases reject. Its first input line sets cleanup_sessions.
//
//go:embed scripts/storage-detach.sh
var storageDetachScript string

type longhornVolume struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		NumberOfReplicas int    `json:"numberOfReplicas"`
		NodeID           string `json:"nodeID"`
	} `json:"spec"`
	Status struct {
		State            string `json:"state"`
		Robustness       string `json:"robustness"`
		CurrentNodeID    string `json:"currentNodeID"`
		KubernetesStatus struct {
			Namespace string `json:"namespace"`
			PVCName   string `json:"pvcName"`
		} `json:"kubernetesStatus"`
	} `json:"status"`
}

// claim names the volume's PersistentVolumeClaim as namespace/name.
func (v longhornVolume) claim() string {
	return v.Status.KubernetesStatus.Namespace + "/" + v.Status.KubernetesStatus.PVCName
}

func (v longhornVolume) attachedOrAttaching() bool {
	return v.Status.State == "attached" || v.Status.State == "attaching"
}

type longhornReplica struct {
	Spec struct {
		VolumeName string `json:"volumeName"`
		NodeID     string `json:"nodeID"`
		FailedAt   string `json:"failedAt"`
	} `json:"spec"`
	Status struct {
		CurrentState string `json:"currentState"`
	} `json:"status"`
}

func (o *Operation) longhornVolumesServed() bool {
	r := o.kubectl(Command{Argv: []string{"-n", "longhorn-system", "get", "volumes.longhorn.io"}, Quiet: true})
	return r.Status == 0
}

func (o *Operation) listLonghornVolumes() ([]longhornVolume, error) {
	var volumes struct {
		Items []longhornVolume `json:"items"`
	}
	err := o.getJSON(&volumes, "-n", "longhorn-system", "get", "volumes.longhorn.io", "-o", "json")
	return volumes.Items, err
}

// unhealthyAttachedVolumes lists attached or attaching volumes whose
// robustness is not healthy.
func unhealthyAttachedVolumes(volumes []longhornVolume) []string {
	var lines []string
	for _, v := range volumes {
		if v.attachedOrAttaching() && v.Status.Robustness != "healthy" {
			lines = append(lines, fmt.Sprintf("%s\t%s\t%s\t%s", v.Metadata.Name, v.Status.State, v.Status.Robustness, v.claim()))
		}
	}
	return lines
}

// longhornSetting reads a Longhorn setting's value, or "" when kubectl
// cannot read it.
func (o *Operation) longhornSetting(name string) (string, error) {
	var setting struct {
		Value json.RawMessage `json:"value"`
	}
	found, err := o.readQuiet(&setting, "-n", "longhorn-system", "get", "settings.longhorn.io", name, "-o", "json")
	if err != nil || !found {
		return "", err
	}
	var text string
	if err := json.Unmarshal(setting.Value, &text); err == nil {
		return text, nil
	}
	return string(setting.Value), nil
}

func (o *Operation) setRebuildLimit(value string) bool {
	patch, _ := json.Marshal(map[string]string{"value": value})
	r := o.kubectl(Command{Argv: []string{"-n", "longhorn-system", "patch", "settings.longhorn.io", rebuildLimitSetting,
		"--type=merge", "-p", string(patch)}})
	return r.Status == 0
}

// restoreRebuildLimit puts back a temporarily raised rebuild admission. It
// runs even after an interrupt.
func (o *Operation) restoreRebuildLimit() error {
	original := o.rebuildLimitOriginal
	if original == "" {
		return nil
	}
	o.log("restoring Longhorn replica rebuild admission to %s", original)
	patch, _ := json.Marshal(map[string]string{"value": original})
	argv := append(o.kubectlArgv(), "-n", "longhorn-system", "patch", "settings.longhorn.io", rebuildLimitSetting,
		"--type=merge", "-p", string(patch))
	r := o.runner.Run(context.Background(), Command{Argv: argv})
	if r.Status != 0 {
		return fmt.Errorf("failed to restore Longhorn replica rebuild admission to %s", original)
	}
	o.rebuildLimitOriginal = ""
	return nil
}

var digitsPattern = regexp.MustCompile(`^[0-9]+$`)

// waitForLonghornHealth requires the replica policy floor on every attached
// volume, then waits for them to be healthy. With automatic rebuild
// admission closed, it temporarily admits two rebuilds per node while
// degraded volumes recover, and restores the setting afterwards.
func (o *Operation) waitForLonghornHealth() error {
	if r := o.kubectl(Command{Argv: []string{"get", "namespace", "longhorn-system"}, Quiet: true}); r.Status != 0 {
		o.log("longhorn-system namespace not found; skipping Longhorn checks")
		return nil
	}
	if !o.longhornVolumesServed() {
		o.log("Longhorn volume CRDs not found; skipping Longhorn checks")
		return nil
	}

	o.log("checking attached Longhorn replica floor (%d)", minLonghornReplicas)
	volumes, err := o.listLonghornVolumes()
	if err != nil {
		return err
	}
	var under []string
	for _, v := range volumes {
		if v.attachedOrAttaching() && v.Spec.NumberOfReplicas < minLonghornReplicas {
			under = append(under, fmt.Sprintf("%s\tdesired=%d\t%s\t%s", v.Metadata.Name, v.Spec.NumberOfReplicas, v.Status.State, v.claim()))
		}
	}
	if len(under) > 0 {
		o.report(under)
		return fmt.Errorf("attached Longhorn volumes are below the replica policy floor")
	}

	o.log("waiting for attached Longhorn volumes to be healthy")
	settle := duration(o.opts.SettleTimeout)
	reuse, err := o.longhornSetting("replica-replenishment-wait-interval")
	if err != nil {
		return err
	}
	if digitsPattern.MatchString(reuse) {
		o.log("Longhorn failed-replica reuse window is %ss; health timeout is %ds", reuse, int(settle.Seconds()))
	}
	limit, err := o.longhornSetting(rebuildLimitSetting)
	if err != nil {
		return err
	}
	if limit == "0" {
		o.log("Longhorn automatic rebuild admission is disabled; degraded volumes require the guarded sequential recovery procedure")
	}
	deadline := o.now().Add(settle)

	volumes, err = o.listLonghornVolumes()
	if err != nil {
		return err
	}
	if len(unhealthyAttachedVolumes(volumes)) > 0 && limit == "0" && !o.opts.CheckOnly {
		// Recorded first, so a failed patch is still restored on exit.
		o.rebuildLimitOriginal = limit
		o.log("temporarily enabling two guarded Longhorn replica rebuilds per node")
		if !o.setRebuildLimit("2") {
			return fmt.Errorf("could not enable guarded Longhorn replica rebuilds")
		}
	}

	var bad []string
	var lastReport time.Time
	for {
		volumes, err := o.listLonghornVolumes()
		if err != nil {
			return err
		}
		bad = unhealthyAttachedVolumes(volumes)
		if len(bad) == 0 {
			break
		}
		// Rate-limit progress; the full list is printed only if the deadline
		// expires.
		if lastReport.IsZero() || o.now().Sub(lastReport) >= 5*time.Minute {
			o.log("%d attached Longhorn volume(s) are still reconciling", len(bad))
			lastReport = o.now()
		}
		if o.expired(deadline) {
			break
		}
		if err := o.pause(); err != nil {
			return err
		}
	}
	if len(bad) > 0 {
		o.report(bad)
		return fmt.Errorf("Longhorn volumes are not healthy")
	}

	if err := o.restoreRebuildLimit(); err != nil {
		return err
	}

	var target struct {
		Status struct {
			Available  bool        `json:"available"`
			Conditions []condition `json:"conditions"`
		} `json:"status"`
	}
	// No backup target is not checked; one that does not decode fails, so
	// --require-longhorn-backup-target cannot pass on unread data.
	found, err := o.readQuiet(&target, "-n", "longhorn-system", "get", "backuptargets.longhorn.io", "default", "-o", "json")
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	o.log("checking Longhorn backup target")
	if target.Status.Available {
		return nil
	}
	reason := "unavailable"
	for _, c := range target.Status.Conditions {
		if c.Type == "Unavailable" && c.Status == "True" {
			if c.Reason != "" {
				reason = c.Reason
			}
			break
		}
	}
	if o.opts.RequireLonghornBackupTarget {
		return fmt.Errorf("Longhorn backup target is not available (%s)", reason)
	}
	o.log("Longhorn backup target is not available (%s); continuing", reason)
	return nil
}

// warnForLonghornHealth reports unhealthy attached volumes without waiting.
func (o *Operation) warnForLonghornHealth() error {
	if r := o.kubectl(Command{Argv: []string{"get", "namespace", "longhorn-system"}, Quiet: true}); r.Status != 0 {
		return nil
	}
	if !o.longhornVolumesServed() {
		return nil
	}
	volumes, err := o.listLonghornVolumes()
	if err != nil {
		return err
	}
	if bad := unhealthyAttachedVolumes(volumes); len(bad) > 0 {
		o.log("attached Longhorn volumes are not healthy after drain; continuing for poweroff")
		o.report(bad)
	}
	return nil
}

// survivingReplicaNodes maps each volume to the other nodes that keep a
// running, unfailed replica of it with the target down.
func survivingReplicaNodes(target string, replicas []longhornReplica) map[string][]string {
	surviving := map[string][]string{}
	for _, r := range replicas {
		if r.Spec.NodeID != target && r.Status.CurrentState == "running" && r.Spec.FailedAt == "" &&
			!slices.Contains(surviving[r.Spec.VolumeName], r.Spec.NodeID) {
			surviving[r.Spec.VolumeName] = append(surviving[r.Spec.VolumeName], r.Spec.NodeID)
		}
	}
	for _, nodes := range surviving {
		slices.Sort(nodes)
	}
	return surviving
}

// survivabilityFailures lists attached volumes that would not stay
// available with the node down: attached to it, faulted, below the replica
// floor, or without healthy replicas on enough other nodes.
func survivabilityFailures(target string, volumes []longhornVolume, replicas []longhornReplica) []string {
	surviving := survivingReplicaNodes(target, replicas)
	var lines []string
	for _, v := range volumes {
		if !v.attachedOrAttaching() {
			continue
		}
		nodes := surviving[v.Metadata.Name]
		if v.Spec.NumberOfReplicas < minLonghornReplicas ||
			v.Status.State != "attached" ||
			v.Status.Robustness == "faulted" ||
			v.Status.CurrentNodeID == target ||
			v.Spec.NodeID == target ||
			len(nodes) < minSurvivingLonghornReplicas {
			lines = append(lines, survivabilityLine(v, nodes))
		}
	}
	return lines
}

// spreadFailures lists attached volumes whose healthy replicas sit on too
// few nodes besides the target. Unlike survivabilityFailures it ignores the
// attachment, which the drain moves, so it can run before the cordon. A
// volume still attaching may not run its replicas yet, so it is left to the
// gate after the drain, which waits.
func spreadFailures(target string, volumes []longhornVolume, replicas []longhornReplica) []string {
	surviving := survivingReplicaNodes(target, replicas)
	var lines []string
	for _, v := range volumes {
		if v.Status.State == "attached" && len(surviving[v.Metadata.Name]) < minSurvivingLonghornReplicas {
			lines = append(lines, survivabilityLine(v, surviving[v.Metadata.Name]))
		}
	}
	return lines
}

func survivabilityLine(v longhornVolume, nodes []string) string {
	return fmt.Sprintf("%s\t%s\tstate=%s\trobustness=%s\tattachment=%s\tsurvivingNodes=%s",
		v.Metadata.Name, v.claim(), v.Status.State, v.Status.Robustness, v.Status.CurrentNodeID, strings.Join(nodes, ","))
}

func (o *Operation) listLonghornReplicas() ([]longhornReplica, error) {
	var replicas struct {
		Items []longhornReplica `json:"items"`
	}
	err := o.getJSON(&replicas, "-n", "longhorn-system", "get", "replicas.longhorn.io", "-o", "json")
	return replicas.Items, err
}

// checkLonghornReplicaSpread fails before the database switchover and the
// cordon when an attached volume keeps healthy replicas on too few nodes
// besides the target. Longhorn
// can report such a volume healthy, say with several replicas on one node
// after another node's outage, and the one-node-down gate after the drain
// would then wait out its whole timeout with the node cordoned and drained.
func (o *Operation) checkLonghornReplicaSpread() error {
	if !o.longhornVolumesServed() {
		return nil
	}
	o.log("checking Longhorn replica spread without %s (%d other nodes)", o.opts.Node, minSurvivingLonghornReplicas)
	volumes, err := o.listLonghornVolumes()
	if err != nil {
		return err
	}
	replicas, err := o.listLonghornReplicas()
	if err != nil {
		return err
	}
	if bad := spreadFailures(o.opts.Node, volumes, replicas); len(bad) > 0 {
		o.report(bad)
		return fmt.Errorf("attached Longhorn volumes keep healthy replicas on fewer than %d nodes besides %s; restore the replica spread before maintenance",
			minSurvivingLonghornReplicas, o.opts.Node)
	}
	return nil
}

func (o *Operation) waitForLonghornSurvivability() error {
	if !o.longhornVolumesServed() {
		return nil
	}
	o.log("checking Longhorn one-node-down replica floor (%d)", minSurvivingLonghornReplicas)
	deadline := o.now().Add(duration(o.opts.SettleTimeout))
	for {
		volumes, err := o.listLonghornVolumes()
		if err != nil {
			return err
		}
		replicas, err := o.listLonghornReplicas()
		if err != nil {
			return err
		}
		bad := survivabilityFailures(o.opts.Node, volumes, replicas)
		if len(bad) == 0 {
			return nil
		}
		o.report(bad)
		if o.expired(deadline) {
			return fmt.Errorf("Longhorn one-node-down availability gate did not pass")
		}
		if err := o.pause(); err != nil {
			return err
		}
	}
}

type volumeAttachment struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		NodeName string `json:"nodeName"`
		Attacher string `json:"attacher"`
		Source   struct {
			PersistentVolumeName string `json:"persistentVolumeName"`
		} `json:"source"`
	} `json:"spec"`
	Status struct {
		Attached bool `json:"attached"`
	} `json:"status"`
}

// waitForNodeStorageDetach waits for every Longhorn volume and
// VolumeAttachment to leave the node, then checks the host for residual
// CSI mounts and iSCSI sessions. Only a real run reconciles sessions.
func (o *Operation) waitForNodeStorageDetach() error {
	node := o.opts.Node
	timeout := duration(o.opts.DrainTimeout)

	if o.longhornVolumesServed() {
		o.log("waiting for Longhorn volumes to detach from %s", node)
		deadline := o.now().Add(timeout)
		for {
			volumes, err := o.listLonghornVolumes()
			if err != nil {
				return err
			}
			var attached []string
			for _, v := range volumes {
				if (v.Status.CurrentNodeID == node || v.Spec.NodeID == node) && v.Status.State != "detached" {
					attached = append(attached, fmt.Sprintf("%s\t%s\t%s", v.Metadata.Name, v.Status.State, v.claim()))
				}
			}
			if len(attached) == 0 {
				break
			}
			o.report(attached)
			if o.expired(deadline) {
				return fmt.Errorf("Longhorn volumes remain attached to %s", node)
			}
			if err := o.pause(); err != nil {
				return err
			}
		}
	}

	o.log("waiting for Longhorn VolumeAttachments to leave %s", node)
	deadline := o.now().Add(timeout)
	for {
		var attachments struct {
			Items []volumeAttachment `json:"items"`
		}
		if err := o.getJSON(&attachments, "get", "volumeattachments.storage.k8s.io", "-o", "json"); err != nil {
			return err
		}
		var remaining []string
		for _, a := range attachments.Items {
			if a.Spec.NodeName == node && a.Spec.Attacher == "driver.longhorn.io" {
				remaining = append(remaining, fmt.Sprintf("%s\t%s\tattached=%t", a.Metadata.Name, a.Spec.Source.PersistentVolumeName, a.Status.Attached))
			}
		}
		if len(remaining) == 0 {
			break
		}
		o.report(remaining)
		if o.expired(deadline) {
			return fmt.Errorf("Longhorn VolumeAttachments remain on %s", node)
		}
		if err := o.pause(); err != nil {
			return err
		}
	}

	o.log("checking %s for residual Longhorn mounts and iSCSI sessions", o.opts.SSHTarget)
	script := fmt.Sprintf("cleanup_sessions=%t\n", !o.opts.CheckOnly) + storageDetachScript
	if r := o.remoteRootShell(script, true); r.Status != 0 {
		return fmt.Errorf("residual Longhorn storage is still active on %s", o.opts.SSHTarget)
	}
	return nil
}
