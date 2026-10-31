package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Deployed roots keep, on this machine, the generation deploy last activated
// on each remote target (ADR-0008). Remote hosts' closures have no other root
// here, so a collection would otherwise remove them, and the next run would
// rebuild their unchanged derivations that no binary cache holds, such as
// packages built for another architecture or a cross-compiled kernel. This
// machine's own targets are rooted by its system and Home Manager profiles.

// deployedRootDir holds one link per remote target, named like its target.
func deployedRootDir() string {
	return filepath.Join(stateHome(), "nix-deploy", "roots")
}

// keepDeployed roots path as the target's deployed generation, replacing the
// root of the one it activated before. It runs inside the activation, which
// holds the target's lock across runs, so concurrent runs root a target in
// the order they activated it. A root that cannot be made is a warning, not a
// failure: the host already runs the generation.
func (d *deployer) keepDeployed(t target, path string, errOut io.Writer) {
	if t.host == d.self {
		return
	}
	dir := deployedRootDir()
	link := filepath.Join(dir, t.phase+"-"+t.host)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(errOut, "deploy: cannot keep %s rooted after its activation: %v\n", t.key(), err)
		return
	}
	// The path is valid here, since it was built or copied from this store,
	// so realising it only adds the root. Nix replaces an existing link.
	if output, status := capture(0, "nix-store", "--add-root", link, "--realise", path); status != 0 {
		fmt.Fprintf(errOut, "deploy: cannot keep %s rooted after its activation: %s\n", t.key(), oneLine(output))
	}
}

// pruneDeployedRoots removes the roots of targets the inventory no longer
// deploys from this machine, so a retired host does not pin its last closure
// forever. Other files in the directory are left alone. The directory is
// shared by every inventory this user deploys, so a second inventory's runs
// remove the first one's roots.
func (d *deployer) pruneDeployedRoots() {
	dir := deployedRootDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink == 0 || d.deploysRemotely(entry.Name()) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
			fmt.Fprintf(os.Stderr, "deploy: cannot remove the deployed root %s: %v\n", entry.Name(), err)
		}
	}
}

// deploysRemotely reports whether a deployed root's name is a remote target
// of the inventory.
func (d *deployer) deploysRemotely(name string) bool {
	phase, host, ok := strings.Cut(name, "-")
	if !ok || host == d.self || !contains(d.inv.KnownHosts, host) {
		return false
	}
	switch phase {
	case "system":
		return true
	case "hm":
		return d.inv.usesHomeManager(host)
	}
	return false
}
