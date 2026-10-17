package k8snode

import "fmt"

// The types below decode the fields of `kubectl get -o json` output that the
// gates read. Missing fields decode to zero values, which the gates treat
// like jq's `// default` fallbacks in the Bash tool.

type objectMeta struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace"`
	Labels            map[string]string `json:"labels"`
	Annotations       map[string]string `json:"annotations"`
	OwnerReferences   []ownerReference  `json:"ownerReferences"`
	DeletionTimestamp *string           `json:"deletionTimestamp"`
}

type ownerReference struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// hasOwner reports whether any owner reference is of the kind.
func (m objectMeta) hasOwner(kind string) bool {
	for _, owner := range m.OwnerReferences {
		if owner.Kind == kind {
			return true
		}
	}
	return false
}

// firstOwner is the first owner reference, which names a Pod's controller.
func (m objectMeta) firstOwner() ownerReference {
	if len(m.OwnerReferences) == 0 {
		return ownerReference{}
	}
	return m.OwnerReferences[0]
}

type condition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

type taint struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Effect string `json:"effect"`
}

// RebootPendingTaint marks the nodes a fleet reboot (nixfleet reboot) has
// still to reboot. Its PreferNoSchedule effect makes evicted Pods prefer
// nodes that already rebooted, and a primary switchover prefers a replica
// on a node without it.
const RebootPendingTaint = "nixfleet/reboot-pending"

type node struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		Unschedulable bool    `json:"unschedulable"`
		Taints        []taint `json:"taints"`
	} `json:"spec"`
	Status struct {
		Conditions []condition `json:"conditions"`
		Addresses  []struct {
			Type    string `json:"type"`
			Address string `json:"address"`
		} `json:"addresses"`
		Allocatable map[string]string `json:"allocatable"`
	} `json:"status"`
}

type nodeList struct {
	Items []node `json:"items"`
}

// internalIPs lists the node's InternalIP addresses.
func (n node) internalIPs() []string {
	var addresses []string
	for _, address := range n.Status.Addresses {
		if address.Type == "InternalIP" {
			addresses = append(addresses, address.Address)
		}
	}
	return addresses
}

func nodeIsReady(n node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == "Ready" && c.Status == "True" {
			return true
		}
	}
	return false
}

type requirement struct {
	Key      string   `json:"key"`
	Operator string   `json:"operator"`
	Values   []string `json:"values"`
}

type labelSelector struct {
	MatchLabels      map[string]string `json:"matchLabels"`
	MatchExpressions []requirement     `json:"matchExpressions"`
}

type nodeSelectorTerm struct {
	MatchExpressions []requirement `json:"matchExpressions"`
	MatchFields      []requirement `json:"matchFields"`
}

type podAffinityTerm struct {
	LabelSelector *labelSelector `json:"labelSelector"`
	// NamespaceSelector is a pointer because an empty selector, which
	// selects every namespace, differs from none.
	NamespaceSelector *labelSelector `json:"namespaceSelector"`
	Namespaces        []string       `json:"namespaces"`
	TopologyKey       string         `json:"topologyKey"`
	MatchLabelKeys    []string       `json:"matchLabelKeys"`
	MismatchLabelKeys []string       `json:"mismatchLabelKeys"`
}

type affinity struct {
	NodeAffinity *struct {
		Required *struct {
			NodeSelectorTerms []nodeSelectorTerm `json:"nodeSelectorTerms"`
		} `json:"requiredDuringSchedulingIgnoredDuringExecution"`
	} `json:"nodeAffinity"`
	PodAntiAffinity *struct {
		Required []podAffinityTerm `json:"requiredDuringSchedulingIgnoredDuringExecution"`
	} `json:"podAntiAffinity"`
}

type toleration struct {
	Key      string `json:"key"`
	Operator string `json:"operator"`
	Value    string `json:"value"`
	Effect   string `json:"effect"`
}

type spreadConstraint struct {
	MaxSkew            int            `json:"maxSkew"`
	TopologyKey        string         `json:"topologyKey"`
	WhenUnsatisfiable  string         `json:"whenUnsatisfiable"`
	LabelSelector      *labelSelector `json:"labelSelector"`
	MinDomains         *int           `json:"minDomains"`
	NodeAffinityPolicy string         `json:"nodeAffinityPolicy"`
	NodeTaintsPolicy   string         `json:"nodeTaintsPolicy"`
	MatchLabelKeys     []string       `json:"matchLabelKeys"`
}

// container is the part of a container the capacity check reads.
type container struct {
	Resources struct {
		Requests map[string]string `json:"requests"`
	} `json:"resources"`
	// RestartPolicy Always marks a sidecar init container, which keeps
	// running beside the app containers.
	RestartPolicy string `json:"restartPolicy"`
}

type podSpec struct {
	NodeName                  string             `json:"nodeName"`
	NodeSelector              map[string]string  `json:"nodeSelector"`
	Affinity                  *affinity          `json:"affinity"`
	Tolerations               []toleration       `json:"tolerations"`
	TopologySpreadConstraints []spreadConstraint `json:"topologySpreadConstraints"`
	Containers                []container        `json:"containers"`
	InitContainers            []container        `json:"initContainers"`
	Overhead                  map[string]string  `json:"overhead"`
	// Resources are Pod-level requests, which replace the containers' sum
	// for each resource they name.
	Resources struct {
		Requests map[string]string `json:"requests"`
	} `json:"resources"`
	// TerminationGracePeriodSeconds defaults to 30 when nil.
	TerminationGracePeriodSeconds *int64 `json:"terminationGracePeriodSeconds"`
}

type pod struct {
	Metadata objectMeta `json:"metadata"`
	Spec     podSpec    `json:"spec"`
	Status   struct {
		Phase      string      `json:"phase"`
		Conditions []condition `json:"conditions"`
		// NominatedNodeName is set while a preemptor waits for its
		// victims to stop.
		NominatedNodeName string `json:"nominatedNodeName"`
		ContainerStatuses []struct {
			Ready bool `json:"ready"`
			// AllocatedResources and Resources show what the node holds
			// for the container while an in-place resize is under way.
			AllocatedResources map[string]string `json:"allocatedResources"`
			Resources          struct {
				Requests map[string]string `json:"requests"`
			} `json:"resources"`
			State struct {
				Waiting *struct {
					Reason string `json:"reason"`
				} `json:"waiting"`
			} `json:"state"`
		} `json:"containerStatuses"`
	} `json:"status"`
}

type podList struct {
	Items []pod `json:"items"`
}

// active reports whether the Pod has not finished.
func (p pod) active() bool {
	return p.Status.Phase != "Succeeded" && p.Status.Phase != "Failed"
}

// podReady reports whether the Pod's Ready condition is true, which also
// takes its readiness gates into account.
func (p pod) podReady() bool {
	for _, c := range p.Status.Conditions {
		if c.Type == "Ready" {
			return c.Status == "True"
		}
	}
	return false
}

// runningAndReady reports whether the Pod runs with every container Ready.
func (p pod) runningAndReady() bool {
	if p.Status.Phase != "Running" || len(p.Status.ContainerStatuses) == 0 {
		return false
	}
	for _, status := range p.Status.ContainerStatuses {
		if !status.Ready {
			return false
		}
	}
	return true
}

// controller is a Deployment, StatefulSet, ReplicaSet or other Pod owner.
type controller struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		Replicas *int           `json:"replicas"`
		Selector *labelSelector `json:"selector"`
		Template struct {
			Metadata objectMeta `json:"metadata"`
			Spec     podSpec    `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
	Status struct {
		ReadyReplicas int `json:"readyReplicas"`
	} `json:"status"`
}

// desiredReplicas is spec.replicas, which defaults to one.
func (c controller) desiredReplicas() int {
	if c.Spec.Replicas == nil {
		return 1
	}
	return *c.Spec.Replicas
}

// workload is a controller that owns Pods on the node, such as
// deployment/web in namespace default.
type workload struct {
	Namespace string
	// Kind is lower case: deployment, statefulset, replicaset or another
	// owner kind.
	Kind string
	Name string
}

// ref is the kubectl resource reference, such as deployment/web.
func (w workload) ref() string {
	return w.Kind + "/" + w.Name
}

func (w workload) String() string {
	return w.Namespace + "/" + w.ref()
}

// getNode reads the target node.
func (o *Operation) getNode() (node, error) {
	var n node
	if err := o.getJSON(&n, "get", "node", o.opts.Node, "-o", "json"); err != nil {
		return n, fmt.Errorf("could not read Kubernetes node %s: %v", o.opts.Node, err)
	}
	return n, nil
}

func (o *Operation) nodeIsCordoned() (bool, error) {
	n, err := o.getNode()
	if err != nil {
		return false, err
	}
	return n.Spec.Unschedulable, nil
}

func (o *Operation) listNodes() ([]node, error) {
	var nodes nodeList
	err := o.getJSON(&nodes, "get", "nodes", "-o", "json")
	return nodes.Items, err
}

func (o *Operation) listAllPods() ([]pod, error) {
	var pods podList
	err := o.getJSON(&pods, "get", "pods", "--all-namespaces", "-o", "json")
	return pods.Items, err
}

// listNodePods lists the Pods bound to the target node.
func (o *Operation) listNodePods() ([]pod, error) {
	var pods podList
	err := o.getJSON(&pods, "get", "pods", "--all-namespaces", "--field-selector", "spec.nodeName="+o.opts.Node, "-o", "json")
	return pods.Items, err
}

// rebootPending reports whether the node waits for its turn in a fleet
// reboot.
func rebootPending(n node) bool {
	for _, t := range n.Spec.Taints {
		if t.Key == RebootPendingTaint {
			return true
		}
	}
	return false
}
