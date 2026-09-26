# Git identity guard

This package supplies a Git hook directory at
`share/git-identity-guard/hooks`. Set `core.hooksPath` to that directory and
`identityGuard.policyFile` to a local JSON file. The policy schema is:

```json
{"blockedEmails": ["example@invalid.test"]}
```

The policy file is read at hook runtime. Keep sensitive policy contents out of
public source and caches. An absent policy setting leaves the dispatcher in delegation-only mode.
A configured file that is missing, empty, or malformed blocks the operation.
The guard checks effective author and committer identities, commit messages,
and outgoing commits. Annotated tag objects are checked for tagger identity and
message on push. A new remote ref uses that remote's tracking refs as the
published boundary. If none are available, or the push uses a URL, it checks
the full reachable history. Existing remote refs whose old object is not
available locally require a fetch before push.

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
