# revkit changelog

`revkit --version` and the `revkitVersion` field of `results.json` name the
running release. Bump `version` in `default.nix` with every change to the
tool and add an entry here:

- major: an incompatible change to the command line, the configuration
  schema or `results.json`;
- minor: a new command or option, or a change in what a run does;
- patch: a fix that keeps the intended behaviour.

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
