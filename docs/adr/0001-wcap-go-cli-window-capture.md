# ADR-0001: wcap as a Go CLI wrapping gpu-screen-recorder

**Status:** Accepted
**Date:** 2026-05-01
**Applies to:** `tools/wcap`

## Context

Need to record individual browser windows with audio, where the recorded audio should not be audible in headphones during recording — but optionally monitorable via a toggle. OBS was the initial candidate but is significantly heavier than necessary for this use case.

## Decision

Implement `wcap` as a Go CLI tool in `tools/` that wraps `gpu-screen-recorder` for video capture and manages PipeWire audio routing dynamically at runtime. The tool exposes three subcommands:

- `start` — pick a window via xdg-desktop-portal, move the target app's audio stream to a PipeWire null sink (`wcap-sink`), and begin recording video + audio.
- `stop` — stop recording, restore the app's audio stream to the default output, send a desktop notification with the output path.
- `monitor` — toggle a PipeWire loopback from `wcap-sink` to the default output (hear what's being recorded).

Audio routing is ephemeral: `wcap start` moves the stream, `wcap stop` restores it. No persistent WirePlumber rules — the app's audio behaves normally when not recording.

## Alternatives Considered

- **OBS Studio** — full-featured but heavyweight; requires manual scene/source setup; overkill for single-window recording.
- **wf-recorder** — simpler but no per-window capture on Wayland; region-only.
- **Shell scripts** — harder to manage state (PID tracking, stream IDs, loopback toggle) reliably.
- **Persistent WirePlumber auto-routing rule** — would silence the app at all times, not just during recording. Too aggressive for the use case.

## Consequences

- `gpu-screen-recorder` becomes a required runtime dependency; must be available in PATH (declared as a Nix dep).
- Window selection uses `gpu-screen-recorder -w portal`, which requires `xdg-desktop-portal-hyprland` in the NixOS config.
- The tool is app-agnostic — it moves whichever PipeWire stream the user selects, not hardcoded to Helium.
- wcap must track state between `start` and `stop` (gpu-screen-recorder PID, original sink ID, stream ID). A small state file in `$XDG_RUNTIME_DIR/wcap/` handles this.
- PipeWire null sink (`wcap-sink`) must be declared in nix-config; it is passive and has no effect until wcap actively routes a stream to it.
- Recordings land in `~/Videos/wcap/` by default.
