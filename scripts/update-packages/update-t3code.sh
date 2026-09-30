#!/usr/bin/env bash
# Update the published upstream nightly and both validated fork channels.
set -euo pipefail
# shellcheck source=scripts/ci/t3code-nix-home.sh disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/../ci/t3code-nix-home.sh"

PIN_FILE=pkgs/t3code/source.json
HELPER=scripts/update-packages/t3code-source.py
FAKE_HASH=sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=
PREFLIGHT_REPORT="${T3CODE_PREFLIGHT_REPORT:-${RUNNER_TEMP:-${TMPDIR:-/tmp}}/t3code-patch-preflight.json}"
rm -f -- "$PREFLIGHT_REPORT"
work=$(mktemp -d)
success=false
cleanup() {
  if [[ "$success" != true && -f "$work/original.json" ]]; then
    cp "$work/original.json" "${PIN_FILE}.restore"
    mv "${PIN_FILE}.restore" "$PIN_FILE"
  fi
  rm -rf -- "$work"
}
trap cleanup EXIT

python3 "$HELPER" target >"$work/target.json"
should_update=$(python3 "$HELPER" decide "$PIN_FILE" "$work/target.json")
if [[ "$should_update" == false && "${FORCE_UPDATE:-false}" != true ]]; then
  echo 'Already up to date.'
  echo 'updated=false' >>"${GITHUB_OUTPUT:-/dev/null}"
  success=true
  exit 0
fi
cp "$PIN_FILE" "$work/original.json"

prefetch_source() {
  local owner=$1 revision=$2
  clean_homeless_shelter
  nix store prefetch-file --unpack --json "https://github.com/${owner}/t3code/archive/${revision}.tar.gz" |
    python3 -c 'import json,sys; print(json.load(sys.stdin)["hash"])'
}

upstream_hash=$(prefetch_source pingdotgg "$(jq -r .revision "$work/target.json")")
nightly_hash=$(prefetch_source alcxyz "$(jq -r .forks.nightly.revision "$work/target.json")")
stable_hash=$(prefetch_source alcxyz "$(jq -r .forks.stable.revision "$work/target.json")")
python3 - "$work/hashes.json" "$upstream_hash" "$nightly_hash" "$stable_hash" "$FAKE_HASH" <<'PY'
import json, sys
from pathlib import Path
path = Path(sys.argv[1])
_, upstream, nightly, stable, fake = sys.argv[1:]
path.write_text(json.dumps({channel: {"hash": value, "cargoHash": fake, "pnpmDepsHash": fake}
                            for channel, value in (("upstream", upstream), ("nightly", nightly), ("stable", stable))}))
PY

stage() {
  python3 "$HELPER" materialize "$PIN_FILE" "$work/target.json" "$work/hashes.json" "$work/original.json"
}
set_hash() {
  python3 - "$work/hashes.json" "$1" "$2" "$3" <<'PY'
import json, sys
from pathlib import Path
path = Path(sys.argv[1])
hashes = json.loads(path.read_text())
hashes[sys.argv[2]][sys.argv[3]] = sys.argv[4]
path.write_text(json.dumps(hashes))
PY
  stage
}
report_preflight() {
  python3 - "$PREFLIGHT_REPORT" "$1" "$2" <<'PY'
import json, sys
from pathlib import Path
path = Path(sys.argv[1])
report = json.loads(path.read_text()) if path.exists() else {
    "schemaVersion": 2, "check": "t3code-fork-quota-patch-application",
    "scope": "quota-patch-application-only", "channels": {}, "fullBuildValidated": False,
}
report["channels"][sys.argv[2]] = {"status": "passed" if sys.argv[3] == "0" else "failed",
                                      "exitCode": int(sys.argv[3])}
path.parent.mkdir(parents=True, exist_ok=True)
path.write_text(json.dumps(report, indent=2) + "\n")
PY
}
preflight() {
  local channel=$1 status=0 log
  log=$(mktemp)
  clean_homeless_shelter
  nix build -L ".#t3code-fork-${channel}.src" --no-link 2>"$log" || status=$?
  cat "$log" >&2
  rm -f "$log"
  report_preflight "$channel" "$status"
  if ((status != 0)); then
    echo "T3 Code ${channel} quota patch failed; stopping before dependency hash builds." >&2
    return "$status"
  fi
}
collect_hash() {
  local attr=$1 label=$2 log hash
  log=$(mktemp)
  clean_homeless_shelter
  if nix build -L "$attr" --no-link 2>"$log"; then
    cat "$log" >&2
    rm -f "$log"
    echo "Expected ${label} build to fail with the placeholder hash" >&2
    return 1
  fi
  cat "$log" >&2
  hash=$(sed -nE 's/^[[:space:]]*got:[[:space:]]+(sha256-[^[:space:]]+).*/\1/p' "$log" | tail -n1)
  rm -f "$log"
  if [[ ! "$hash" =~ ^sha256-.+ ]]; then
    echo "Unable to collect ${label} hash" >&2
    return 1
  fi
  printf '%s\n' "$hash"
}

stage
for channel in nightly stable; do
  preflight "$channel"
done
for channel in upstream nightly stable; do
  attr=t3code
  [[ "$channel" == upstream ]] || attr="t3code-fork-${channel}"
  set_hash "$channel" cargoHash "$(collect_hash ".#${attr}.resourceMonitor" "${channel} cargoHash")"
  set_hash "$channel" pnpmDepsHash "$(collect_hash ".#${attr}.pnpmDeps" "${channel} pnpmDepsHash")"
done
T3CODE_VERIFY_ALWAYS=true scripts/ci/verify-t3code-providers.sh
success=true
upstream_version=$(jq -r .version "$work/target.json")
nightly_version=$(jq -r .forks.nightly.version "$work/target.json")
stable_version=$(jq -r .forks.stable.version "$work/target.json")
nightly_revision=$(jq -r .forks.nightly.revision "$work/target.json")
stable_revision=$(jq -r .forks.stable.revision "$work/target.json")
{
  echo 'updated=true'
  echo "version=${upstream_version} + fork nightly ${nightly_version} + stable ${stable_version}"
  echo "upstream_url=https://github.com/pingdotgg/t3code/releases/tag/v${upstream_version} and fork nightly https://github.com/alcxyz/t3code/commit/${nightly_revision} and stable https://github.com/alcxyz/t3code/commit/${stable_revision}"
} >>"${GITHUB_OUTPUT:-/dev/null}"
