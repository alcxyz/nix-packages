package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStartupRollback(t *testing.T) {
	for _, mode := range []string{"early", "save", "owned", "interrupt", "picker"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			os.Mkdir(bin, 0700)
			t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
			t.Setenv("XDG_RUNTIME_DIR", dir)
			t.Setenv("FIXTURE", dir)
			t.Setenv("MODE", mode)
			pactl := `#!/bin/sh
printf '%s\n' "$*" >> "$FIXTURE/calls"
case "$*" in
 '--format=json list sinks')
  if { [ "$MODE" = owned ] || [ "$MODE" = picker ]; } && [ ! -f "$FIXTURE/created" ]; then echo '[]'; else echo '[{"index":9,"name":"wcap-sink"}]'; fi ;;
 'load-module '*) touch "$FIXTURE/created"; echo 42 ;;
 '--format=json list sink-inputs') echo '[{"index":7,"sink":3,"properties":{"application.name":"fixture"}}]' ;;
 *) ;;
esac
`
			os.WriteFile(filepath.Join(bin, "pactl"), []byte(pactl), 0700)
			os.WriteFile(filepath.Join(bin, "fuzzel"), []byte("#!/bin/sh\nif [ \"$MODE\" = picker ]; then kill -TERM \"$PPID\"; exec sleep 30; fi\necho '7 fixture'\n"), 0700)
			recorder := `#!/bin/sh
if [ "$MODE" = early ] || [ "$MODE" = owned ]; then exit 2; fi
if [ "$MODE" = save ]; then mkdir "$XDG_RUNTIME_DIR/wcap/state.json"; fi
printf '%s\n' "$$" > "$FIXTURE/pid"
if [ "$MODE" = interrupt ]; then kill -INT "$PPID"; fi
exec sleep 30
`
			os.WriteFile(filepath.Join(bin, "gpu-screen-recorder"), []byte(recorder), 0700)
			expected := 1
			if mode == "interrupt" || mode == "picker" {
				expected = 130
			}
			started := time.Now()
			if status := runStart([]string{"--dir", filepath.Join(dir, "videos")}); status != expected {
				t.Fatalf("startup status %d", status)
			}
			calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
			if mode == "picker" && (time.Since(started) > 5*time.Second || strings.Contains(string(calls), "move-sink-input")) {
				t.Fatalf("picker cancellation delayed or routed audio: %s", calls)
			}
			if mode != "picker" && !strings.Contains(string(calls), "move-sink-input 7 3") {
				t.Fatalf("routing not restored: %s", calls)
			}
			unloaded := strings.Contains(string(calls), "unload-module 42")
			if unloaded != (mode == "owned" || mode == "picker") {
				t.Fatalf("sink ownership not respected: %s", calls)
			}
			if data, err := os.ReadFile(filepath.Join(dir, "pid")); err == nil {
				// The recorder is our child and runStart has reaped it before returning.
				if entries, err := os.ReadFile("/proc/" + strings.TrimSpace(string(data)) + "/stat"); err == nil {
					t.Fatalf("recorder still exists: %s", entries)
				}
			}
		})
	}
}
