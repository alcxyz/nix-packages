# workspace-sync compatibility contract

The reusable executable accepts `ROOT MANIFEST [MODE]`. Modes are an omitted or
empty argument for sync, `--status`, `--directories`, and `--help` or `-h`.
Arguments after the mode are ignored. Missing either raw positional argument
exits 1, including when a help flag occupies the root position. Help with both
positional arguments performs no manifest validation or filesystem writes.

The pinned reference is
[nix-config commit 463ec8fa37129ccdd91b20e9d9c29fa3869c92d8](https://git.alc.xyz/alcxyz/nix-config/commit/463ec8fa37129ccdd91b20e9d9c29fa3869c92d8).
The verbatim implementation and
existing fixture script live under `testdata/reference/`, retaining their
original relative paths. The decision is
[nix-config ADR-0095](https://git.alc.xyz/alcxyz/nix-config/src/commit/463ec8fa37129ccdd91b20e9d9c29fa3869c92d8/docs/adr/0095-bounded-workspace-sync-go-pilot.md).

`go test ./...` builds the candidate executable and compares it with the pinned
shell using independent, equivalent temporary fixtures. Set
`WORKSPACE_SYNC_TEST_BINARY` to an executable path to exercise a packaged
candidate instead. Tests need Bash, jq, Git, coreutils and ripgrep. They isolate
Git configuration, disable prompting and permit only local file transport.
The original fixture script also runs against both implementations.

Comparisons cover exit codes, exact stdout, executable-authored stderr and
filesystem contents, including symlink targets and dirty tracked files. Git
metadata is excluded because independently created repositories and worktrees
contain fixture-specific IDs and paths. Git, jq, Bash and coreutils diagnostics
are dependency-owned and excluded from stderr comparisons. Injected Git records
NUL-separated arguments so URL and branch quoting are checked independently of
Git's interpretation of those arguments.

## Characterized compatibility details

- Unknown modes and invalid manifests exit 2. Missing root/manifest arguments
  exit 1. Root rejection exits 1. Normal sync and status report directory and
  path conflicts without necessarily failing. Directory-only mode continues
  past conflicts, then exits 1. Clone failures continue to later entries and
  produce a summary with exit 1.
- Manifest validation accepts extra fields. Required collections must be arrays;
  branch may be absent, null or a string. Profiles must be an array of strings.
  Empty and whitespace-only JSON streams are invalid. Dedicated candidate
  tests assert this independently of the pinned shell: jq versions differ in
  whether `jq -e` accepts an empty input stream without producing a result.
- The [compatibility decision approved on 2026-10-21](https://git.alc.xyz/alcxyz/nix-packages/issues/542#issuecomment-33517)
  retains multi-document manifests while validating all entries before creating
  paths. The executable reads the complete stream once and validates every
  document before reconciliation. Extraction uses only that validated snapshot,
  including for `-` and `/dev/stdin`. A stream containing any invalid document
  exits 2 without creating the root or any entries. A subsequent change to the
  manifest file does not change the entries consumed by that invocation.
  These approved changes replace the reference's final-result validation and
  drained-stdin extraction behavior; dedicated tests cover them separately from
  the remaining parity comparisons.
- jq's path regular expression accepts a single terminal newline. Directory
  extraction reads that as an additional empty line. Repository/link fields use
  Bash command substitution, which strips terminal newlines. NUL bytes in URL
  and branch strings are also discarded by Bash. Missing/null branch becomes
  an empty value. These observed coercions are retained at the jq boundary.
- Status creates neither the root nor missing parents, and status and
  directory-only mode never clone. Existing checkouts, dirty files, worktrees,
  ordinary directories and file conflicts remain untouched.
- Managed roots and parents reject symlinks. Existing correct relative links
  remain unchanged; wrong or dangling symlinks are conflicts. Missing canonical
  targets are reported successfully. Canonical targets resolving outside the
  root are skipped. The port adds no filesystem-race guarantee.
  A symlink above the managed root is permitted by the reference. A canonical
  target resolving to the root itself is skipped because the containment test
  requires a descendant.
- A failed relative realpath, resolved root realpath, resolved target realpath,
  or link creation aborts immediately with the dependency's original status.
  Injected exit 37 tests prove no final summary and no later link processing,
  including a realpath failure during status. Fake Git failures preserve the
  different clone policy: continue and finish with exit 1.
  A missing helper command retains exit 127, and a helper terminated by SIGTERM
  retains exit 143. The user approved retaining exit 127 for an executable
  whose interpreter is missing, matching the producer's Bash 5.2 baseline.
  Bash 5.3 returns 126 for that case; the test accepts that single reference
  difference and requires Go to return 127. All output, abort ordering and
  filesystem comparisons still apply. Oversized directory names preserve failure reporting
  while later directories still get created in sync and directory-only modes;
  status remains read-only.
  Sync still attempts a later repository after a directory creation failure;
  status and directory-only mode never clone it.
- Failed stdout writes exit 1. Help propagates its write failure, and an entry
  report failure stops before the next entry or final summary. Tests use a
  read-only stdout descriptor and a later-entry marker to verify this boundary.
- Executable helper files without a shebang retain Bash's ENOEXEC fallback.
  The executable invokes Bash with the selected file and the original argument
  array, preserving literal URL/branch arguments, partial clone failure
  continuation, and fatal realpath/link exit statuses.

Native package checks exercise this source and package contract. Darwin
evaluation or cross compilation does not establish native Darwin execution.
Consumer selection, generated wrappers and manifests, activation derivations,
credential-backed cloning and installed runtime acceptance require their own
consumer evidence; these synthetic tests do not activate a user profile.

Relative PATH entries can supply both an executable without a shebang and Bash;
the fallback retains that lookup behavior. The partial-failure fixture matches
only the synthetic URL and explicitly requires one failed and one cloned entry,
including the later repository directory.
