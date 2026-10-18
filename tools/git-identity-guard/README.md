# Git identity guard

This package supplies a Git hook directory at
`share/git-identity-guard/hooks`. Set `core.hooksPath` to that directory and
`identityGuard.policyFile` to a local JSON file. The policy schema is:

```json
{
  "blockedEmails": ["example@invalid.test"],
  "privateTerms": ["example-corp", "\\bacme-[a-z]"],
  "publicRepositories": ["git.example.test/owner/public"]
}
```

`privateTerms` and `publicRepositories` are optional but go together.
Terms are case-insensitive Python regular expressions. Repositories are
`HOST/OWNER/REPO`; a repository counts as public when any of its remotes, or
the push destination, points there, whatever the scheme, user, port or `.git`
suffix. SSH host aliases are not resolved, so list an alias as its own host.
In a public repository the guard rejects a commit whose staged change, new file
name or author or committer identity adds a term, and a push of a new commit,
annotated tag (name, tagger and message) or ref name that does. Every file is
scanned as text, including binary files and paths marked `-diff`. Commit
messages are checked at `commit-msg` without comment lines, as the editor
cleanup leaves them, and in full at push, which catches comment lines kept by
`-m` or `-F`. Lines a change removes never match, so cleaning up old mentions
works; deleting a ref is always allowed. A merge commit is checked at push
only, for lines and files missing from all its parents (`--cc`, `-c`). A push
that checks full history (below) also scans old commits, so history with old
mentions blocks it. Errors do not print the term.

The policy file is read at hook runtime. Keep sensitive policy contents out of
public source and caches. An absent policy setting leaves the dispatcher in
delegation-only mode.
A configured file that is missing, empty, or malformed blocks guarded commits
and pushes.
The guard checks effective author and committer identities, commit messages,
and outgoing commits. Annotated tag objects are checked for tagger identity and
message on push. Commits already reachable from the pushed-to remote's
tracking refs count as published, so merging a published branch into another
does not re-check its history. A new ref without tracking refs, or any push to
a URL, checks the full reachable history. Existing remote refs whose old object
is not available locally require a fetch before push.

The dispatcher also runs executable ordinary repository hooks from
`git rev-parse --git-common-dir`/`hooks`. To preserve a custom repository
hooks directory, set `identityGuard.repositoryHooksPath` to its original
`core.hooksPath` value and then remove the repository's `core.hooksPath`
override. Relative paths use Git's hook working directory. The original hook
receives the same arguments, stdin, and exit status; `pre-push` input is
buffered and replayed after the guard checks it.

Git hooks are a local guardrail, not a tamperproof restriction. A repository
`core.hooksPath` override, `git -c core.hooksPath=…`, or `git push --no-verify`
can bypass relevant checks. Some Git operations may bypass commit hooks too;
the pre-push scan catches affected commits when it runs. Check overrides and
published commits independently when auditing coverage.
