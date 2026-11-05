# ADR-0009: Devlog attribution and gap-fill deduplication

**Status:** Accepted
**Date:** 2026-10-14, updated 2026-11-05
**History:** [replaced text](archive/0009-devlog-attribution-and-gap-fill-history.md)
**Applies to:** `tools/devlog/`

## Context

Expanded Forgejo pushes can contain other accounts' work. Daily entries also
record reported commit SHAs, but reading only earlier entries lets a missing
older day repeat commits already reported on a later day.
[Issue #481](https://git.alc.xyz/alcxyz/nix-packages/issues/481) proposed options
for both cases; the user approved the following policies before implementation.

## Decision

- For expanded pushes, keep commits whose linked Forgejo author matches the
  configured user, or whose author is not linked to an account. Drop commits
  linked to other authors, regardless of who committed or pushed them. Unlinked
  authors remain eligible because the user's author email may be unlinked.
- Keep the existing 30-day backward deduplication window and also read every
  existing later daily entry's reported-commit record. Exclude the date being
  generated. The first written entry retains the commit, even if a gap filled
  later has an earlier date; existing entries are not rewritten.
- Every configured activity source is required. GitHub searches paginate and
  reject incomplete results, missing completeness metadata and totals beyond
  the search limit. Forgejo push expansion must retrieve the whole push before
  attribution filtering. A pruned base or truncated response fails the daily
  generation instead of persisting a partial day. A complete expansion that
  filters out all commits is still a successful empty result.

## Alternatives considered

- Require every author to be linked to the configured user. This would discard
  the user's work committed with an unlinked email.
- Retain every expanded commit because the user pushed it. This attributes
  collaborator and bot work to the user and spends the diff budget on it.
- Read only the next 30 days. This leaves older gap fills vulnerable to
  duplicates outside that forward window.
- Move previously reported work to the earliest date. This would rewrite
  existing daily and weekly entries and change the established append policy.

## Consequences

Unlinked authors can still include work by others; their identity is unknown.
Gap fills read more existing entries as the journal grows, but avoid rewriting
published history. Deduplication remains based on commit SHA across sources.

Source outages or unavailable history leave a day unwritten for a later retry.
