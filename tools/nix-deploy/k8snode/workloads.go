package k8snode

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// workloadForPod returns the controller that owns a Pod, resolving a
// ReplicaSet to its Deployment. ok is false for Pods no controller will
// recreate elsewhere: DaemonSet, Job, static and unowned Pods.
func (o *Operation) workloadForPod(p pod) (workload, bool, error) {
	namespace := p.Metadata.Namespace
	owner := p.Metadata.firstOwner()
	switch owner.Kind {
	case "DaemonSet", "Job", "Node", "":
		return workload{}, false, nil
	case "StatefulSet":
		return workload{Namespace: namespace, Kind: "statefulset", Name: owner.Name}, true, nil
	case "ReplicaSet":
		var rs controller
		found, err := o.readQuiet(&rs, "-n", namespace, "get", "replicaset", owner.Name, "-o", "json")
		if err != nil {
			return workload{}, false, err
		}
		if found {
			rsOwner := rs.Metadata.firstOwner()
			if rsOwner.Kind == "Deployment" && rsOwner.Name != "" {
				return workload{Namespace: namespace, Kind: "deployment", Name: rsOwner.Name}, true, nil
			}
		}
		return workload{Namespace: namespace, Kind: "replicaset", Name: owner.Name}, true, nil
	}
	return workload{Namespace: namespace, Kind: strings.ToLower(owner.Kind), Name: owner.Name}, true, nil
}

// uniqueWorkloads sorts the workloads and drops duplicates.
func uniqueWorkloads(workloads []workload) []workload {
	sort.Slice(workloads, func(i, j int) bool {
		return workloads[i].String() < workloads[j].String()
	})
	var unique []workload
	for i, w := range workloads {
		if i == 0 || w != workloads[i-1] {
			unique = append(unique, w)
		}
	}
	return unique
}

// collectDisplacedWorkloads lists the controllers of the Pods on the node
// before the drain.
func (o *Operation) collectDisplacedWorkloads() ([]workload, error) {
	pods, err := o.listNodePods()
	if err != nil {
		return nil, err
	}
	var workloads []workload
	for _, p := range pods {
		if p.Metadata.Namespace == "" || p.Metadata.Name == "" {
			continue
		}
		w, ok, err := o.workloadForPod(p)
		if err != nil {
			return nil, err
		}
		if ok {
			workloads = append(workloads, w)
		}
	}
	return uniqueWorkloads(workloads), nil
}

// collectTargetPinnedPendingWorkloads reconstructs, on a resumed reboot or
// a power-on, the deferred controllers that can run only on the node: their
// Pods wait Pending until it returns.
func (o *Operation) collectTargetPinnedPendingWorkloads() ([]workload, error) {
	var pods podList
	if err := o.getJSON(&pods, "get", "pods", "--all-namespaces", "--field-selector", "status.phase=Pending", "-o", "json"); err != nil {
		return nil, err
	}
	var workloads []workload
	for _, p := range pods.Items {
		if p.Metadata.Namespace == "" || p.Metadata.Name == "" {
			continue
		}
		w, ok, err := o.workloadForPod(p)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		pinned, err := o.workloadRequiresTargetNode(w)
		if err != nil {
			return nil, err
		}
		if pinned {
			workloads = append(workloads, w)
		}
	}
	return uniqueWorkloads(workloads), nil
}

// waitForWorkloads waits, after the node returned to service, for every
// recorded controller to settle.
func (o *Operation) waitForWorkloads() error {
	if len(o.workloads) == 0 {
		o.log("no controller-owned workloads to wait for")
		return nil
	}
	o.log("waiting for displaced workload controllers to settle")
	failed := false
	for _, w := range o.workloads {
		switch w.Kind {
		case "deployment", "statefulset":
			o.log("waiting for %s", w)
			r := o.kubectl(Command{Argv: []string{"-n", w.Namespace, "rollout", "status", w.ref(), "--timeout=" + o.opts.SettleTimeout}, Passthrough: true})
			if r.Status != 0 {
				failed = true
			}
		case "replicaset":
			o.log("waiting for %s pods to be Ready", w)
			var rs controller
			if err := o.getJSON(&rs, "-n", w.Namespace, "get", w.ref(), "-o", "json"); err != nil {
				return err
			}
			var terms []string
			if rs.Spec.Selector != nil {
				for key, value := range rs.Spec.Selector.MatchLabels {
					terms = append(terms, key+"="+value)
				}
			}
			slices.Sort(terms)
			r := o.kubectl(Command{Argv: []string{"-n", w.Namespace, "wait", "--for=condition=Ready", "pod",
				"--selector=" + strings.Join(terms, ","), "--timeout=" + o.opts.SettleTimeout}, Passthrough: true})
			if r.Status != 0 {
				failed = true
			}
		default:
			o.log("cannot generically wait for %s; skipping", w)
		}
	}
	if failed {
		return fmt.Errorf("one or more displaced workloads did not settle")
	}
	return nil
}

// controllerReplicas reads a controller's spec.replicas, or one when kubectl
// cannot read it.
func (o *Operation) controllerReplicas(w workload) (int, error) {
	var c controller
	found, err := o.readQuiet(&c, "-n", w.Namespace, "get", w.ref(), "-o", "json")
	if err != nil || !found {
		return 1, err
	}
	return c.desiredReplicas(), nil
}

func (o *Operation) waitForControllerReadyFloor(w workload, minimumReady int) error {
	deadline := o.now().Add(duration(o.opts.SettleTimeout))
	for {
		ready := 0
		var c controller
		found, err := o.readQuiet(&c, "-n", w.Namespace, "get", w.ref(), "-o", "json")
		if err != nil {
			return err
		}
		if found {
			ready = c.Status.ReadyReplicas
		}
		if ready >= minimumReady {
			return nil
		}
		if o.expired(deadline) {
			return fmt.Errorf("%s has %d Ready replicas; maintenance requires %d", w, ready, minimumReady)
		}
		if err := o.pause(); err != nil {
			return err
		}
	}
}

// waitForDisplacedWorkloadSurvivability waits, after the drain, for every
// displaced controller to keep the replicas it can while the node is out.
// Controllers that can run only on the node are deferred until it returns.
func (o *Operation) waitForDisplacedWorkloadSurvivability() error {
	if len(o.workloads) == 0 {
		o.log("no controller-owned workloads need maintenance checks")
		return nil
	}
	o.log("checking displaced workload availability")
	for _, w := range o.workloads {
		pinned, err := o.workloadRequiresTargetNode(w)
		if err != nil {
			return err
		}
		if pinned {
			o.log("deferring target-pinned %s until %s returns", w, o.opts.Node)
			continue
		}
		switch w.Kind {
		case "deployment":
			desired, err := o.controllerReplicas(w)
			if err != nil {
				return err
			}
			minimumReady, ok, err := o.deploymentReadyFloor(w)
			if err != nil {
				return err
			}
			if !ok {
				minimumReady = desired
			}
			if minimumReady < desired {
				o.log("%s cannot place every replica while %s is out; requiring %d of %d Ready", w, o.opts.Node, minimumReady, desired)
			}
			if err := o.waitForControllerReadyFloor(w, minimumReady); err != nil {
				return err
			}
		case "replicaset":
			desired, err := o.controllerReplicas(w)
			if err != nil {
				return err
			}
			if err := o.waitForControllerReadyFloor(w, desired); err != nil {
				return err
			}
		case "statefulset":
			desired, err := o.controllerReplicas(w)
			if err != nil {
				return err
			}
			minimumReady := 1
			if desired > 1 {
				minimumReady = desired - 1
			}
			if err := o.waitForControllerReadyFloor(w, minimumReady); err != nil {
				return err
			}
		default:
			o.log("using a dedicated availability gate for %s", w)
		}
	}
	return nil
}

// badPodReasons are container waiting reasons that will not resolve without
// intervention.
var badPodReasons = []string{"CrashLoopBackOff", "ImagePullBackOff", "ErrImagePull", "CreateContainerConfigError", "CreateContainerError"}

// unhealthyPods lists failed or stuck Pods that Jobs do not own.
func unhealthyPods(pods []pod) []string {
	var lines []string
	for _, p := range pods {
		if p.Metadata.hasOwner("Job") || p.Status.Phase == "Succeeded" {
			continue
		}
		bad := p.Status.Phase == "Failed"
		for _, status := range p.Status.ContainerStatuses {
			if status.State.Waiting != nil && slices.Contains(badPodReasons, status.State.Waiting.Reason) {
				bad = true
			}
		}
		if bad {
			lines = append(lines, fmt.Sprintf("%s/%s: %s", p.Metadata.Namespace, p.Metadata.Name, p.Status.Phase))
		}
	}
	return lines
}

func (o *Operation) waitForNoBadPods() error {
	o.log("checking for unhealthy non-Job pods")
	deadline := o.now().Add(duration(o.opts.SettleTimeout))
	for {
		pods, err := o.listAllPods()
		if err != nil {
			return err
		}
		bad := unhealthyPods(pods)
		if len(bad) == 0 {
			return nil
		}
		o.report(bad)
		if o.expired(deadline) {
			return fmt.Errorf("unhealthy pods remained after waiting")
		}
		if err := o.pause(); err != nil {
			return err
		}
	}
}

func (o *Operation) verifyNodeIsCordoned() error {
	cordoned, err := o.nodeIsCordoned()
	if err != nil {
		return err
	}
	if !cordoned {
		return fmt.Errorf("%s is not cordoned; --resume-maintenance requires an existing maintenance cordon", o.opts.Node)
	}
	return nil
}

// verifyNodeHasOnlyMaintenancePods allows only DaemonSet Pods and the
// Longhorn instance-manager on a node in maintenance.
func (o *Operation) verifyNodeHasOnlyMaintenancePods() error {
	pods, err := o.listNodePods()
	if err != nil {
		return err
	}
	var bad []string
	for _, p := range pods {
		if !p.active() {
			continue
		}
		owner := p.Metadata.firstOwner().Kind
		if owner == "DaemonSet" {
			continue
		}
		if p.Metadata.Namespace == "longhorn-system" && p.Metadata.Labels["longhorn.io/component"] == "instance-manager" {
			continue
		}
		if owner == "" {
			owner = "none"
		}
		bad = append(bad, fmt.Sprintf("%s/%s\towner=%s\tphase=%s", p.Metadata.Namespace, p.Metadata.Name, owner, p.Status.Phase))
	}
	if len(bad) > 0 {
		o.report(bad)
		return fmt.Errorf("non-maintenance Pods remain on %s", o.opts.Node)
	}
	return nil
}
