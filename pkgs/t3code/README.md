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

## Preview browser

T3 renders previews with a Chrome for Testing headless shell whose version and
archive hashes are pinned in `apps/server/src/preview/PreviewBrowser.ts`. It
normally downloads that build on first use, but the prebuilt binary cannot find
its libraries on NixOS. `preview-browser.nix` reads the pin from the T3 source
being built, fetches the same archive and links its libraries from the store,
so source updates need no extra pin and a changed pin layout fails evaluation
of `t3code`. Reading the pin is import from derivation: evaluating `t3code`
fetches the T3 source, and evaluators that disable import from derivation
cannot evaluate it.

The package exposes it as `t3code.previewBrowser` and links it at
`libexec/t3code/preview-browser/<platform>/<version>`. T3 does not look there:
host modules link that version directory into each base directory's
`tools/chrome-headless-shell/<platform>/`, where T3 treats it as installed
([nix-config ADR-0092](https://git.alc.xyz/alcxyz/nix-config/src/branch/dev/docs/adr/0092-t3-preview-browser-from-nix.md)).
