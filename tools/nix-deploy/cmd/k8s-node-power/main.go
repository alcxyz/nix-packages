// Command k8s-node-power reboots, powers off or powers on a Kubernetes node
// behind the cluster, storage, database and self-fence gates of nix-config
// ADR-0068 and ADR-0082 (package k8snode).
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"nix-deploy/k8snode"
)

// version is set by the package build (-X main.version); see CHANGELOG.md.
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, k8snode.Usage)
		return 2
	}
	switch args[0] {
	case "-h", "--help":
		fmt.Print(k8snode.Usage)
		return 0
	case "--version":
		fmt.Println("k8s-node-power " + version)
		return 0
	}

	opts, err := k8snode.ParseArgs(args[0], args[1:])
	switch {
	case errors.Is(err, k8snode.ErrHelp):
		fmt.Print(k8snode.Usage)
		return 0
	case errors.Is(err, k8snode.ErrUsage):
		fmt.Fprint(os.Stderr, k8snode.Usage)
		return 2
	case err != nil:
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	runner := &k8snode.Runner{Stdout: os.Stdout, Stderr: os.Stderr}
	for _, name := range []string{"kubectl", "ssh"} {
		if !runner.Has(name) {
			fmt.Fprintf(os.Stderr, "error: missing required command: %s\n", name)
			return 1
		}
	}

	// An interrupt stops new commands and waits; the operation then cleans
	// up: it restores cluster settings it changed and reports a stopped
	// self-fence agent.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	received := make(chan os.Signal, 1)
	go func() {
		sig := <-signals
		received <- sig
		cancel()
	}()

	status := k8snode.New(ctx, opts, runner).Run()
	select {
	case sig := <-received:
		if sig == syscall.SIGTERM {
			return 143
		}
		return 130
	default:
		return status
	}
}
