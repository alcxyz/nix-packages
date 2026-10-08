# T3 Code source pin

`source.json` pins the published upstream nightly: its version, exact release
tag commit, source hash, and Cargo and pnpm dependency hashes.

The six-hourly T3 scan compares the newest published nightly with `dev` and any
open update PR before setting up Nix. For a manual update from the repository
root, run:

```sh
bash scripts/update-packages/update-t3code.sh
python3 scripts/ci/test-t3code-updater.py
nix eval --raw .#t3code.version
```

The updater selects only published, non-draft nightly releases, resolves the
release tag to its commit, computes the dependency hashes, then builds and
verifies runtime provider versions. A failure restores the previous pin. Review
the generated pin before publication.
