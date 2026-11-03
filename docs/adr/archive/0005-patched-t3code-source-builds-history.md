**History of** [ADR-0005](../0005-patched-t3code-source-builds.md): text replaced on 2026-11-03.

## Text replaced on 2026-11-03: updater validation without builds

In the consequences, the six-hourly updater bullet read:

> - The six-hourly updater selects the latest published upstream nightly. A
>   lightweight check compares its identity with `dev` and any open update PR
>   before Nix setup. A changed candidate triggers source, dependency,
>   full-build, and embedded-provider validation before opening an update PR. A
>   failed build leaves the last package pin in place.
