"""Adapt the pinned hyprland-rs dependency for hyprscratch's address-based use."""

import hashlib
import json
from pathlib import Path
import sys

crate = Path(sys.argv[1]) / "hyprland-0.4.0-beta.3"

data = crate / "src/data/regular.rs"
text = data.read_text()
old = "    pub id: WorkspaceId,"
assert text.count(old) == 2
# Named and special workspaces no longer expose numeric IDs in 0.57.
# The paired application patch uses workspace addresses instead of these IDs.
replacement = (
    "    #[serde(default)]\n" + old
    + "\n    /// Canonical workspace address, independent of its display name."
    + "\n    pub address: String,"
)
data.write_text(text.replace(old, replacement))

events = crate / "src/event_listener/shared.rs"
text = events.read_text()
old = '        parse_int!($int, event: $event => WorkspaceId)'
assert text.count(old) == 1
# The first V2 workspace argument is now an addressable name. Hyprscratch
# uses addresses from JSON queries, never the obsolete event numeric ID.
# Keep strict parsing for monitor IDs (the other macro arm).
events.write_text(text.replace(old, '        $int.parse::<WorkspaceId>().unwrap_or_default()'))

# Cargo verifies vendored sources against this file before compiling them.
checksum = crate / ".cargo-checksum.json"
manifest = json.loads(checksum.read_text())
for path in (data, events):
    manifest["files"][str(path.relative_to(crate))] = hashlib.sha256(path.read_bytes()).hexdigest()
checksum.write_text(json.dumps(manifest))
