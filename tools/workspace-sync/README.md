# workspace-sync

`workspace-sync ROOT MANIFEST [MODE]` creates missing workspace directories,
clones missing repositories in manifest order, and creates relative links to
existing canonical targets. Existing paths and local edits are preserved.

Modes are `--status`, `--directories`, `--help` and `-h`. Omitted or empty mode
means sync. Only the first mode argument is examined; trailing arguments are
ignored. Status creates nothing, including a missing root. Directory mode only
creates the declared directories and their parents.

The manifest has `directories` (relative paths), `repositories` (objects with
`path`, `url`, and optional or null `branch`), and `links` (objects with `path`,
`target`, and a string array `profiles`). Extra fields are accepted. Profiles
are metadata; the consumer filters them before calling the executable. There
are no built-in roots, inventories, profile defaults, or credential settings.

The package export is `packages.${system}.workspace-sync` on `x86_64-linux` and
`aarch64-darwin`. The executable is `bin/workspace-sync`. Consumers supply Git
through `PATH`; Git keeps its own transports, credentials, progress and errors.
The package supplies jq and GNU coreutils at the front of `PATH`, so GNU path
semantics also apply on Darwin. Bash is available as a fallback for trusted
executable wrappers without a shebang, preserving the existing PATH contract.
It does not activate a workspace or install a consumer wrapper.

The port follows the accepted
[nix-config ADR-0095](https://git.alc.xyz/alcxyz/nix-config/src/branch/dev/docs/adr/0095-bounded-workspace-sync-go-pilot.md)
and [producer issue](https://git.alc.xyz/alcxyz/nix-packages/issues/542).
Reconciliation state, typed entries, directory checks, ordering and result
accounting live in Go. jq remains the manifest compatibility boundary: replacing
its validation and conversions would change accepted stream, null and string
quirks. GNU coreutils retains canonical-path semantics and fatal link/path
command statuses on both supported platforms. Subprocess arguments remain separate; no command strings are constructed.

Invalid manifests or unknown modes return 2. Missing raw arguments, rejected
roots, failed clones, and directory-mode conflicts return 1. Sync and status
can succeed with reportable conflicts or missing link targets. Fatal canonical
path or link creation failures retain the command's status and stop immediately
without a final summary. This preserves the baseline's policy, including its
quirks; it adds no concurrent-writer or filesystem-race guarantee. The single
approved exit exception keeps 127 for an executable whose interpreter is
missing, matching Bash 5.2; Bash 5.3 uses 126 for that command-start error.

Run `go test ./...` with Bash, jq, GNU coreutils, Git and ripgrep available. The suite
compares the Go executable with the pinned shell reference using local synthetic
repositories and fake commands, including filesystem snapshots and fatal aborts.
`WORKSPACE_SYNC_TEST_BINARY` selects an existing executable; the package uses it
to check the installed Go binary. An additional package fixture checks the
wrapper with a minimal `PATH`, consumer Git and incompatible path utilities.
`checks.${system}.workspace-sync-contract`
is the package build, including its Go tests and installed executable checks.
Producer evidence does not qualify the consumer lock or installed workspace:
that work belongs to
[nix-config #674](https://git.alc.xyz/alcxyz/nix-config/issues/674).
