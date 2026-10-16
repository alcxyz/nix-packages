# nixfleet (nix-deploy) changelog

`nixfleet --version` (or `deploy --version`) and the `version` field of the
`--json` reports name the running release. Bump `version` in `default.nix`
with every change to the tool and add an entry here:

- major: an incompatible change to the command line, the inventory schema or
  the JSON report;
- minor: a new option or a change in what a run does;
- patch: a fix that keeps the intended behaviour.

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
