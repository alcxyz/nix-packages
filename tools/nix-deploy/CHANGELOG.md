# nixfleet (nix-deploy) changelog

`nixfleet --version` (or `deploy --version`) and the `version` field of the
`--json` reports name the running release. Bump `version` in `default.nix`
with every change to the tool and add an entry here:

- major: an incompatible change to the command line, the inventory schema or
  the JSON report;
- minor: a new option or a change in what a run does;
- patch: a fix that keeps the intended behaviour.

## 0.18.2 (2026-10-22)

- Atomic report publication refuses existing access ACLs and ACLs inherited
  by temporary files, preserving the prior report rather than potentially
  granting access through a changed ACL mask or owning-group permissions.
  ACL inspection failures also prevent publication.

## 0.18.1 (2026-10-22)

- Deployment JSON reports are published atomically, so interruption cannot
  leave an empty or partially written report when the signal handler and normal
  exit race. Existing regular-file ownership and permissions are preserved;
  new reports use mode 0600.
  Symbolic links and other non-regular report destinations are rejected.

## 0.18.0 (2026-10-17)

- `nixfleet reboot` runs the inventory's new `rebootHookCommand`, when set,
  at the start and end of a run and of each host (#578, ADR-0010
  amendment), with the event in `NIXFLEET_*` environment variables, so the
  operator's setup can quiet monitoring and announce progress. A host's
  finish follows its start even after a failure or an interrupt; a failing
  hook is only a warning; `--check-only` and `--dry-run` call none.

## 0.17.0 (2026-10-17)

- A node's maintenance checks, before anything changes, that the Pods the
  availability gate needs after its drain fit on the remaining nodes by
  their CPU and memory requests (#548, ADR-0010 amendment). It honours
  node selectors, required node affinity, taints and required hostname
  anti-affinity both ways, searches for a placement when spreading leaves one out
  (warning when the search cannot settle it), and warns when the drain's
  other Pods lack room after them; `--check-only` runs it too. It reads
  ReplicaSets, StatefulSets, ControllerRevisions and, for a template that
  relies on namespace defaults, LimitRanges.
- After the drain, the availability gate fails once a controller's Pods
  that have not each been Unschedulable for five minutes can no longer
  reach its minimum, instead of waiting out the settle timeout.
- A drain or availability gate that fails before the storage detach now
  uncordons the node, which still runs, instead of leaving it cordoned and
  short of room; later failures, an interrupt, `--resume-maintenance` and
  `--leave-cordoned` keep the cordon.
- Without `--drain-timeout`, the drain timeout grows to the longest grace
  period of the node's evictable Pods plus two minutes; a chosen timeout
  that is shorter is kept with a warning.

## 0.16.0 (2026-10-17)

- Database switchovers and health gates ask each CloudNativePG primary which
  replicas stream (#548, ADR-0010 amendment). A switchover takes only a
  replica that streams and is at most one WAL segment behind; one that is
  Ready but stopped streaming would roll the database back. Full health and
  the one-node-down gate require the replicas to stream, so such a replica
  stops the run before it changes anything, or after the node returns, with
  a hint to rebuild it. A replica trailing under load is waited for up to
  the settle timeout, and a primary that cannot be asked is reported with
  the reason, without that hint. The operator's kubeconfig now needs
  `pods/exec` on the database primaries.

## 0.15.0 (2026-10-17)

- `nixfleet reboot` asks every question before anything starts (#548,
  ADR-0010 amendment): each busy interactive host, then this machine with
  `--include-self` (even with `--yes`), then the run's confirmation, so a
  question is no longer buried under the output of concurrent groups. A
  host the guard blocks is skipped in the plan. At its turn the guard runs
  again and skips the host for a block or a busy reason that was not
  confirmed, without asking. With `--include-self`, sudo takes this
  machine's password before the start and is renewed until its reboot.

## 0.14.0 (2026-10-16)

- Interactive hosts (#548, phase 4, ADR-0010 amendment): new optional
  inventory fields `interactiveHosts` and `rebootGuardCommand`. `nixfleet
  reboot` runs the guard on those hosts for the plan and again at their
  turn; a busy host is asked about on a terminal and skipped without one or
  with `--yes`, a blocked one is skipped, and a failed guard counts as busy.
- `nixfleet reboot --include-self` reboots this machine last, only on a
  terminal, only when no other host failed, after its guard (a block
  refuses) and its own confirmation even with `--yes`; the summary comes
  first. A Kubernetes node or a member of a gated serial group is refused.
  Without the flag the message says how to include it.
- New optional inventory field `bootCheck`: `nixfleet status` gains a `boot`
  column with each host's check after its current boot (and a note for a
  failed one, and `bootCheck` in `--json`), and `nixfleet reboot` waits up to
  12 minutes for a rebooted host's check to pass.

## 0.13.0 (2026-10-16)

- New optional inventory field `kubernetesCorePriorityClass`: the
  PriorityClass of the cluster's core services. `nixfleet reboot` counts the
  Pods of that class that are the only Pod of their workload (core
  singletons; not those of a DaemonSet, a Job or a node) on each node, lists them in the plan, and reboots nodes holding
  fewer primaries and core singletons first (ADR-0010 amendment). It reads
  the cluster's Pods for this; a group with nodes fails when they cannot be
  read.
- With two or more nodes to reboot, each waits for its turn with the
  `nixfleet/reboot-pending:PreferNoSchedule` taint, so Pods a drain evicts
  prefer nodes that already rebooted. The taint comes off as the node's turn
  ends, or at the end of the run when that failed, and a run that reboots a
  node removes every mark it does not set. A run that cannot set it warns
  and goes on.
- A primary switchover (also of `k8s-node-power`) prefers a replica on a node
  without that taint.

## 0.12.0 (2026-10-16)

- New optional inventory field `kubernetesContext`: the kubeconfig context of
  the Kubernetes nodes' cluster. `nixfleet status` reads the cluster there,
  `nixfleet reboot` reads it and reboots every node there, and `nixfleet off`
  and `on` default to it, so the current context no longer picks the
  cluster. Without the field, the current context is used as before.
- `k8s-node-power`, `nixfleet off` and `nixfleet on` accept
  `--context <name>`; `nixfleet reboot` rejects it, since its ready gates and
  status reads follow the inventory. `nixfleet off` and `on` stop when the
  inventory in `NIX_DEPLOY_CONFIG` cannot be read. The kubectl cnpg plugin takes
  the context after its name, since kubectl finds a plugin only before the
  first flag.

## 0.11.0 (2026-10-16)

- `nixfleet reboot [--all | <host>...]` reboots the fleet by policy and
  constraint group (#548, phase 3, ADR-0010). Kubernetes nodes, listed in the
  new optional inventory field `kubernetesNodes`, reboot behind every gate of
  `k8s-node-power reboot`, with its node options; members of a serial group
  wait for the others' ready command before they go down and pass it
  themselves after they return; active Forgejo or Gitea runner units are
  stopped first, so admitted jobs finish, and must be active again after the
  reboot. Every rebooted host must come back with a new boot ID and run the
  system it was meant to boot.
- Each serial group is a constraint group and every other host a group of
  its own. Groups run concurrently and members one at a time, nodes with fewer
  database primaries first; node reboots never overlap, even across groups.
  A failure stops its own group and leaves the host as it is; a summary of
  every host ends the run, which exits 1 when one failed.
- The run prints its plan first. `--dry-run` stops there; otherwise a
  terminal confirms, or `--yes`. `--needed` skips hosts whose verdict is a
  clear "no" (a node only when it is not cordoned). The operator host and this
  machine are not rebooted, hosts that are not NixOS are reported, and a host
  the cluster lists as a node but `kubernetesNodes` does not is refused, as is
  every host when the cluster's nodes cannot be read.
- `k8s-node-power reboot` stays the single-node reboot; `nixfleet off` and
  `nixfleet on` are unchanged.

## 0.10.0 (2026-10-16)

- One program, `nixfleet` (#548, phase 2), with the subcommands `deploy`,
  `reboot`, `off`, `on` and `status`. Installed as `deploy` and
  `k8s-node-power` it is those commands with their command lines unchanged,
  and `nix run .#nix-deploy` still runs `deploy`. The package is also exported
  as `.#nixfleet`, whose main program is `nixfleet`. `kreboot`, `koff` and
  `kon` stay with the k8s-node-reboot package.
- `nixfleet status [--all | <host>...] [--json FILE]`: reads, over SSH with
  BatchMode or locally for this machine, each host's booted, current and
  next-boot systems, whether it needs a reboot, its Kubernetes node's cordon
  state (one `kubectl get nodes`) and its Forgejo or Gitea runner units.
  Unreachable hosts and clusters are reported, and the exit status is 0 unless
  the arguments, the inventory or the report fail.
- `reboot --needed` (also `k8s-node-power reboot --needed`): skips the reboot,
  before any cluster or host change, when the booted kernel, initrd, kernel
  modules and kernel parameters match those of the current and next-boot
  systems and the node is not cordoned. A host that cannot be read or compared
  is rebooted, and a cordoned node goes through the normal gates.
- `k8s-node-power` now uses the same search path as `deploy` and `nixfleet`,
  so all three run the same ssh.

## 0.9.0 (2026-10-16)

- `k8s-node-power reboot|off|on` (#548, phase 1): a Go port of the
  k8s-node-reboot Bash tool (`kreboot`, `koff`, `kon`) with the same options,
  gates and messages (nix-config ADR-0068 and ADR-0082). It ships beside
  `deploy` for qualification runs; the Bash tool stays the supported command
  until then. kubectl JSON is decoded in Go instead of jq, and output that
  does not decode fails the gate rather than taking a fallback. Durations are
  validated before any cluster change and bare seconds reach kubectl as `Ns`.
  An interrupt stops new commands, restores a raised Longhorn rebuild limit
  and reports a stopped self-fence agent.

## 0.8.0 (2026-10-15)

- Build profiles (#548): before the shared build, count the derivations it
  would build (`nix build --dry-run`). From `DEPLOY_HEAVY_BUILDS` (default 50)
  the run is heavy and builds with `max-jobs` `DEPLOY_HEAVY_MAX_JOBS` (default
  6) and `cores` `DEPLOY_HEAVY_CORES` (default 8), each capped at this
  machine's own setting (one that cannot be read is left alone); below it, light keeps this machine's Nix settings. A
  count that fails or cannot be read picks heavy.
- `--heavy` / `--light`, or `DEPLOY_BUILD_PROFILE=auto|light|heavy`, choose
  the profile without counting.
- `--dry-run` prints the profile a run would use, and the JSON report gains
  `buildProfile` and `pendingBuilds`.

## 0.7.1 (2026-10-14)

- Drop Nix's "waiting for another Nix process to finish fetching input" lines
  from the evaluate phase. Nix takes a lock to fetch the flake for every
  evaluation, even one already in the store, so parallel evaluations always
  queue briefly and report it; the wait is expected and short.

## 0.7.0 (2026-10-13)

- Plan before building: each target's store path is compared with what its
  host runs straight after evaluation, and only changed targets are built. An
  unchanged host is no longer rebuilt on this machine, which recompiled its
  packages (under emulation for other architectures) whenever they had left
  the local store (ADR-0008).
- `--dry-run` stops after the plan and skips the build phase.

## 0.6.0 (2026-10-13)

- `deploy --version` prints the release, and the JSON report gains a
  `version` field.

## 0.5.0 (2026-10-12)

- Build every selected target once, compare it with what each host runs, and
  activate only changed targets (#526).
- `--dry-run`, `--force` and `--json FILE`; per-host timings in the summary.
- `serialGroups`: independent groups of hosts that switch one at a time behind
  a ready command.
- Failed or interrupted activations are remembered and retried.

## 0.4.0 (2026-10-12)

- Rewritten in Go at parity with the Bash tool (ADR-0008, #524).

## 0.3.0 (2026-10-01)

- Switch quorum hosts serially behind a ready gate (#456).

## 0.2.0 (2026-09-09)

- Configured at runtime by a versioned JSON inventory (ADR-0006).

## 0.1.0 (2026-04-26)

- First release: one command for NixOS, nix-darwin and Home Manager hosts.
