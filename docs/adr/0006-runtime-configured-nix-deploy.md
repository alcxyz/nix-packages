# ADR-0006: Runtime-configured nix-deploy

**Status:** Accepted (implementation amended by [ADR-0008](0008-nix-deploy-in-go.md))
**Date:** 2026-09-09
**Applies to:** `tools/nix-deploy/`
**History:** [replaced text](archive/0006-runtime-configured-nix-deploy-history.md)

## Context

The reusable deploy command previously contained a stale built-in host list,
while its actively maintained copy lived in a consumer repository because it
needed that repository's inventory. Keeping two command implementations caused
their behavior to diverge.

## Decision

`nix-deploy` is the canonical generic command. It requires a versioned JSON
inventory selected with `--config FILE` or `NIX_DEPLOY_CONFIG`. Version 1
contains the operator host, Home Manager user and output prefix, host sets,
aliases, SSH endpoints and users, remote-sudo hosts, activation modes, and
optional display colors.

The command parses the document, validates types and cross-references,
and rejects values that could become shell command fragments. It does not ship
personal host defaults. Consumer repositories generate their own inventory and
may wrap the command with a default configuration path. Child commands
receive the selected path in `NIX_DEPLOY_CONFIG`.

The implementation and its runtime dependencies are described in
[ADR-0008](0008-nix-deploy-in-go.md), which replaced the Bash script with a Go
program and keeps this interface. Deployment front ends remain runtime
requirements: `nix`, `nixos-rebuild` (or the existing `nix run` fallback),
`home-manager`, `sudo`, and nix-darwin's system rebuild command.

### JSON interface version 1

The required fields are:

| Field | Type | Meaning |
| --- | --- | --- |
| `schemaVersion` | integer | Must be `1` |
| `operatorHost` | string | Host allowed to run the canonical `--all` workflow |
| `homeManagerUser` | string | SSH user for remote Home Manager activation |
| `homeOutputPrefix` | string | Prefix joined with a host to select a Home Manager flake output |
| `knownHosts` | unique string array | Canonical host names |
| `homeManagerHosts` | unique string array | Hosts eligible for explicit Home Manager deployment |
| `remoteHosts` | unique string array | Fleet targets used by `--here` away from the operator host |
| `deployAllHosts` | unique string array | Ordered canonical fleet targets |
| `aliases` | string map | User-facing alias to canonical host |
| `sshHosts` | string map | Canonical host to SSH hostname or IPv4 address |
| `systemSshUsers` | string map | Canonical host to non-default system SSH user |
| `systemRemoteSudoHosts` | unique string array | Hosts whose rebuild requires `--ask-sudo-password` |
| `systemActivationModes` | string map | Canonical host to `switch` or `boot` |
| `hostColors` | string map, optional | Canonical host to an `R;G;B` display color |
| `serialSystemHosts` | unique string array, optional | Hosts whose system switch runs one at a time, in this order; default `[]` |
| `serialReadyCommand` | string, optional | Operator-side shell command run after each serial host's switch; `{host}` is replaced by the host name; default empty (no gate) |
| `serialReadyTimeoutSeconds` | positive integer, optional | Deadline for the post-switch ready gate, including attempts; default `600` |
| `serialGroups` | list, optional | Further serial groups, each `{ hosts, readyCommand?, readyTimeoutSeconds? }` with the meanings above; `hosts` is a non-empty unique list of known hosts, and a host belongs to at most one group, including the `serialSystemHosts` group (ADR-0008) |
| `kubernetesNodes` | unique string array, optional | Known hosts that are Kubernetes nodes, each node named like its host; `nixfleet reboot` reboots them behind the node gates and refuses a cluster node missing here (ADR-0010); default `[]` |
| `kubernetesContext` | string, optional | kubeconfig context of the cluster of `kubernetesNodes`, used for every cluster read and node step of `nixfleet status`, `reboot`, `off` and `on`; default empty (the current context) |
| `kubernetesCorePriorityClass` | string, optional | PriorityClass of the cluster's core services; `nixfleet reboot` reboots nodes holding fewer of its single-Pod workloads first (ADR-0010); a lowercase DNS subdomain; default empty (none count) |
| `interactiveHosts` | unique string array, optional | Known hosts people use at the screen; `nixfleet reboot` runs `rebootGuardCommand` on them before their reboot (ADR-0010); default `[]` |
| `rebootGuardCommand` | string, optional | Read-only command, one line, run on interactive hosts over SSH (locally on this machine); prints `busy`, `block` and `note` findings; required when `interactiveHosts` is not empty |
| `rebootHookCommand` | string, optional | Command, one line and not blank, run on this machine through `sh` at the events of a `nixfleet reboot` that reboots hosts, to quiet monitoring and announce progress (ADR-0010); leave it out for none |
| `bootCheck` | boolean, optional | Hosts check themselves after each boot and leave the result in `/run/nixfleet-boot-check/result`; `nixfleet status` shows it and `nixfleet reboot` waits for it; default `false` |
| `triage` | object, optional | Read by `nixfleet triage` (ADR-0016), which validates only this object, so an error elsewhere in the inventory does not stop it, and an inventory it cannot read only skips the launch; deploy validates the same object after a failed run in a terminal and offers triage only when it is valid and has an `agentCommand`; an invalid object means no offer. `agentCommand`: non-empty array of non-empty strings, the agent CLI's argv, to which triage appends one prompt argument; `inputCheckouts`: object of flake input name to the local checkout of that operator-owned input; `playbookAddendum`: path of a private Markdown playbook addendum. All three are optional; paths may start with `~/` and must be absolute after that; other fields are rejected. Missing checkouts or addendum only warn; leave the object out to write the context without launching an agent |

Every host reference must resolve through `knownHosts`; aliases must not shadow
canonical names. Validation and loading use one in-memory snapshot of the file. Names, users, prefixes,
and endpoints accept only the characters needed by the current command
interface; whitespace and shell metacharacters are rejected before planning.

## Alternatives Considered

- **Keep the consumer-specific implementation** — rejected because fixes and
  tests would continue to bypass the reusable package.
- **Generate shell declarations and evaluate them** — rejected because it
  would turn inventory data into executable input.
- **Add generic policy abstractions** — deferred. The schema represents the
  existing command inputs and does not define broader deployment policy.

## Consequences

- One package owns CLI behavior and black-box contract tests. The contract lives
  next to the executable under `tools/nix-deploy`, so a tool-specific test
  change selects its owning package in CI. Shared CI/flake changes still
  select the full native matrix, and all exported platforms are always evaluated.
- Missing, malformed, or unsupported inventory fails before deployment work.
- Schema changes require a new version or backward-compatible optional fields.
- Fleet system and Home Manager phases derive independent candidates from the
  selected fleet. The Home Manager phase considers only `homeManagerHosts`,
  including an eligible local host, so a system endpoint failure does not
  suppress a separately reachable Home Manager endpoint. `--fail-unreachable`
  still makes an unreachable candidate fail its phase instead of being
  skipped.
- A failed job does not stop other hosts or later phases. Fleet mode records
  each host's result per phase, prints a summary that also names known hosts
  outside the selected fleet, and exits non-zero if any job failed.
  Preflight-skipped hosts are reported as skipped.
- Hosts that must not switch together, such as members of a quorum cluster,
  are listed in `serialSystemHosts`. Fleet mode leaves them out of the
  parallel system batch and switches the selected ones afterwards, one at a
  time in inventory order. After each switch, `serialReadyCommand` (if set)
  is retried every `DEPLOY_SERIAL_READY_INTERVAL` seconds (default 10) until
  it succeeds within `serialReadyTimeoutSeconds`. Each attempt is killed at
  the remaining deadline, and a success reported after it does not count; the
  gate gives up rather than pause when no budget would remain for another
  attempt. The
  first failed switch or gate marks that host failed, records the remaining
  serial hosts as skipped without touching them, and fails the run.
- The serial group must be healthy before any member switches. If a selected
  serial host fails preflight, or any serial host (selected for this run or
  not) fails a single ready-command attempt (bounded by the smaller of 60
  seconds and `serialReadyTimeoutSeconds`) run before the first serial
  switch, no serial host is switched in that run: a member is already
  degraded, so switching another could break quorum. All selected serial
  hosts are recorded as skipped and the run fails, while non-serial hosts
  proceed. Before each later serial switch the other serial hosts are checked
  again; a failure there skips the remaining selected serial hosts and fails
  the run. A single-host system deploy of a serial host first checks the
  other serial hosts the same way and aborts before switching if one is not
  ready; after the switch it runs the gate. The ready command is trusted,
  consumer-authored, non-interactive shell (for example with
  `ssh -o BatchMode=yes`) run in the foreground on the operator machine so
  interrupts reach it; only validated host names are substituted into it.
  Present serial keys must have the documented type; `null` is not treated as
  absent. The Home Manager phase is unaffected. Each group in
  `serialGroups` behaves the same way on its own, and only members whose
  system changed are switched and gated (ADR-0008).
- Fleet mode starts the local sudo keepalive only when the selected system
  phase contains the local host, and does so before preflight so the password
  prompt appears when the command starts. Remote-only orchestration does not
  prompt for or refresh local sudo credentials.
- When a root-accessed target with `nix-gc.timer` is below the free-space
  threshold, preflight starts its `nix-gc.service` once, waits in the
  foreground up to `DEPLOY_REMOTE_GC_TIMEOUT` per host, and checks again, so
  deployment reclaims space under the host's own GC policy instead of skipping
  the host. The wait does not stop the remote unit, and the per-host waits add
  up in the sequential preflight.
  `DEPLOY_REMOTE_GC=0` disables this.
