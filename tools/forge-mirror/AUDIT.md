# Read-only repository audit

`forge-mirror audit [owner/name ...]` requires `FORGE_MIRROR_AUDIT_POLICY_FILE`.
Its versioned JSON contract replaces guesses about owners, branch names and
GitHub destinations. Inventory tooling should generate it from reviewed policy.
The package ships no repository inventory. Existing GitHub-primary, publication-denial and required-private lists remain
required and additive; they cannot be weakened by this file. Explicit activated
publication denials also retain the check for a GitHub pull-mirror source; class
publication policy alone permits preservation mirrors to pull from an upstream.

```json
{
  "schema_version": 1,
  "repositories": [{
    "full_name": "team/example",
    "class": "service",
    "canonical": "forgejo",
    "github": "distribution/example",
    "mirror": "optional",
    "default_branch": "main",
    "required_private": true,
    "retired": false,
    "protections": {
      "main": {"enable_push": false, "apply_to_admins": true}
    }
  }]
}
```

`canonical` is `forgejo`, `github`, `upstream`, `review` or `archived`.
`mirror` is `optional`, `required`, `forbidden`, `disabled`, `review` or
`continuity`. `class` is an opaque label for private reports. `github` is the
exact destination owner/name, independent of the Forgejo owner. Protections map
branch names to expected Forgejo API fields. Merge usernames must include the
declared users. Other fields match exactly, including push users, teams and
write deploy-key access, so a bot exception cannot grow silently.

The audit enumerates all accessible Forgejo owners and preserves each full
name. Declared repositories absent from search are read explicitly, so access
failures cannot disappear. Unclassified discoveries report owner-review findings.
Retired policy excludes active branch/target checks; private visibility and
publication denials still apply to retained private history. Live archival alone
does not retire an active policy entry.

Optional GitHub 404 responses do not stop local checks or require creating a
copy. An existing outgoing mirror with an inaccessible destination remains a
finding. Required and unresolved targets report 404 as absent or inaccessible,
without assuming deletion. GitHub-denied, retired and continuity entries have no
GitHub metadata dependency. Other API failures are execution errors. Server
error bodies and mirror error details are withheld because they can contain
credentials.

Exit codes: `0` means no findings or execution errors, `1` means policy findings,
`2` means execution or configuration errors (possibly alongside findings).
Separate output lines and totals distinguish those outcomes. An audit using an
older wrapper without the new policy fails explicitly with code 2; supply the
policy before upgrading that wrapper. Audit performs GET requests only and never
repairs or synchronizes repositories.
