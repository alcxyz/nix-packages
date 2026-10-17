# revkit changelog

`revkit --version` and the `revkitVersion` field of `results.json` name the
running release. Bump `version` in `default.nix` with every change to the
tool and add an entry here:

- major: an incompatible change to the command line, the configuration
  schema or `results.json`;
- minor: a new command or option, or a change in what a run does;
- patch: a fix that keeps the intended behaviour.

## 0.2.0 (2026-10-17)

- Findings store (#557, ADR-0011 amendment): every distinct problem a run
  reports gets a stable ID in the repository's store; an optional `matcher`
  reviewer groups findings by meaning, within a run and with earlier runs,
  and title matching is the fallback. Reports show each finding's ID and
  whether it is new.
- `revkit findings list|show|dismiss|reopen` manages the store.
- `repo-review --resume` reuses the jobs that completed in the latest run on
  the same commit.
- Reviewers flag security-relevant findings.

## 0.1.1 (2026-10-17)

- Reviewers get only the absolute entries of `PATH`, so an empty or relative
  entry cannot look commands up in the reviewed tree; with none left they get
  `/usr/local/bin:/usr/bin:/bin`.
- With a Codex reviewer, a state directory under `/tmp` is refused: Codex's
  permission profile denies `/tmp`, which would hide the tree, and Codex
  refuses to set up a home there.

## 0.1.0 (2026-10-17)

- First release (#557, ADR-0011): the shared review core (tree from raw git
  objects; read-only reviewers that load no user instructions, Claude in
  restricted and safe mode, Codex in a home holding only its login and under
  a deny-by-default file system profile; an environment allowlist; usage
  including cache reads; and the usage windows the clients report) and `revkit repo-review`, which runs
  each configured lens with every reviewer, has each reviewer's findings
  verified by another, and writes a local report. A reviewer that fails,
  replies with something else than the asked-for JSON, reports an error or
  makes no successful tool call, and a verification that leaves findings
  without a verdict, are missing reviews.
