# T3 Code source pins

`source.json` pins the published upstream nightly and both promoted fork
channels. `t3code-fork` aliases `t3code-fork-nightly`; `t3code-fork-stable` is
selected explicitly. Each fork pin records its published release tag, exact
upstream tag commit, applied feature source commit, promoted commit, source hash,
and Cargo and pnpm dependency hashes.

The hourly T3 scan compares the promoted refs with `dev` and any open update PR
before setting up Nix. For a manual update from the repository root, run:

```sh
bash scripts/update-packages/update-t3code.sh
python3 scripts/ci/test-t3code-updater.py
nix eval --raw .#t3code-fork-nightly.version
nix eval --raw .#t3code-fork-stable.version
```

The updater checks published release tags and ancestry, tests the quota patch
on both fork sources before dependency hash work, then builds and verifies
runtime provider versions. A failure restores the previous pin. Review the
generated pin and preflight report before publication.
