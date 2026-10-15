package k8snode

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

var addressPattern = regexp.MustCompile(`^[0-9A-Fa-f:.]+$`)

// waitForReturnedNodeNetwork waits for the returned node's Flannel
// interface, which can appear after the node reports Ready, then runs the
// strict network checks. Neither a timeout nor a failed audit advances
// recovery.
func (o *Operation) waitForReturnedNodeNetwork() error {
	node := o.opts.Node
	deadline := o.now().Add(duration(o.opts.ReadyTimeout))
	o.log("waiting for %s Flannel interface before the network audit", node)
	for {
		r := o.ssh(Command{Argv: []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=5", o.opts.SSHTarget,
			"ip link show dev flannel.1 >/dev/null 2>&1"}, Passthrough: true})
		if r.Status == 0 {
			break
		}
		if o.expired(deadline) {
			return fmt.Errorf("%s Flannel interface did not appear within %s; node remains cordoned", node, o.opts.ReadyTimeout)
		}
		if err := o.pause(); err != nil {
			return err
		}
	}
	return o.verifyReturnedNodeNetwork()
}

// verifyReturnedNodeNetwork requires the returned node's Flannel public IP
// to match its InternalIP before scheduling resumes, then audits every
// node's network path.
func (o *Operation) verifyReturnedNodeNetwork() error {
	node := o.opts.Node
	n, err := o.getNode()
	if err != nil {
		return err
	}
	// kubectl's jsonpath joins several addresses with spaces; so does this,
	// so that more than one never matches the annotation.
	internalIP := strings.Join(n.internalIPs(), " ")
	flannelIP := n.Metadata.Annotations["flannel.alpha.coreos.com/public-ip"]
	if internalIP == "" {
		return fmt.Errorf("%s has no Kubernetes InternalIP", node)
	}
	if flannelIP != internalIP {
		shown := flannelIP
		if shown == "" {
			shown = "missing"
		}
		return fmt.Errorf("%s Flannel public IP (%s) does not match InternalIP (%s)", node, shown, internalIP)
	}
	return o.verifyAllNodeNetworkPaths("")
}

// verifyNetworkPathOnNode runs the network audit helper as root on a node:
// its InternalIP, Flannel underlay and routes to every peer must use the
// expected address and never the overlay VPN interface.
func (o *Operation) verifyNetworkPathOnNode(auditNode, expectedIP, sshTarget string) error {
	if expectedIP == "" {
		return fmt.Errorf("%s has no Kubernetes InternalIP", auditNode)
	}
	if !safeName.MatchString(auditNode) {
		return fmt.Errorf("unsafe Kubernetes node name: %s", auditNode)
	}
	if !addressPattern.MatchString(expectedIP) {
		return fmt.Errorf("unsafe Kubernetes node address: %s", expectedIP)
	}
	if o.opts.NetworkAuditScript == "" {
		return fmt.Errorf("Kubernetes node network audit helper is unavailable")
	}
	script, err := os.ReadFile(o.opts.NetworkAuditScript)
	if err != nil {
		return fmt.Errorf("Kubernetes node network audit helper is unavailable")
	}

	remoteCommand := "bash -s -- --node " + auditNode + " --expected-node-ip " + expectedIP + " --disallowed-interface wt0"
	o.log("checking %s Kubernetes peer routes and Flannel underlay", auditNode)
	r := o.ssh(Command{Argv: []string{"-o", "BatchMode=yes", sshTarget, `
    if [[ $(id -u) -eq 0 ]]; then
      exec ` + remoteCommand + `
    fi
    exec sudo -n ` + remoteCommand + `
  `}, Stdin: string(script), Passthrough: true})
	if r.Status != 0 {
		return fmt.Errorf("%s Kubernetes network path audit failed", auditNode)
	}
	return nil
}

// verifyAllNodeNetworkPaths audits every Ready node but the excluded one.
// The target is reached through its SSH target, the others as
// NodeSSHUser@node. A node that is not Ready fails the check.
func (o *Operation) verifyAllNodeNetworkPaths(excluded string) error {
	nodes, err := o.listNodes()
	if err != nil {
		return err
	}
	for _, n := range nodes {
		name := n.Metadata.Name
		if name == "" || name == excluded {
			continue
		}
		expectedIP := ""
		if addresses := n.internalIPs(); len(addresses) == 1 {
			expectedIP = addresses[0]
		}
		var ready []string
		for _, c := range n.Status.Conditions {
			if c.Type == "Ready" {
				ready = append(ready, c.Status)
			}
		}
		if len(ready) != 1 || ready[0] != "True" {
			return fmt.Errorf("survivor node %s is not Ready", name)
		}
		sshTarget := o.opts.NodeSSHUser + "@" + name
		if name == o.opts.Node {
			sshTarget = o.opts.SSHTarget
		}
		if err := o.verifyNetworkPathOnNode(name, expectedIP, sshTarget); err != nil {
			return err
		}
	}
	return nil
}

func (o *Operation) verifySurvivorNodeNetworkPaths() error {
	o.log("checking survivor-node networking after %s left service", o.opts.Node)
	return o.verifyAllNodeNetworkPaths(o.opts.Node)
}
