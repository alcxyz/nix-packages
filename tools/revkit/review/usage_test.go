package review

import "testing"

func TestTotalInputCountsCacheReads(t *testing.T) {
	claude := claudeUsage(map[string]any{"input_tokens": 3.0, "cache_read_input_tokens": 2_500_000.0,
		"cache_creation_input_tokens": 90_000.0, "output_tokens": 13_000.0})
	if total, ok := claude.TotalInput("claude"); !ok || total != 2_590_003 {
		t.Fatalf("claude total = %d, %v", total, ok)
	}
	// Codex's input already includes its cache reads.
	codex := codexUsage(map[string]any{"input_tokens": 1_650_000.0, "cached_input_tokens": 1_500_000.0,
		"cache_write_input_tokens": 0.0, "output_tokens": 6000.0})
	if total, ok := codex.TotalInput("codex"); !ok || total != 1_650_000 {
		t.Fatalf("codex total = %d, %v", total, ok)
	}
	partial := claudeUsage(map[string]any{"input_tokens": 3.0})
	if _, ok := partial.TotalInput("claude"); ok {
		t.Fatal("a Claude total without cache reads counts as known")
	}
	if bad := claudeUsage(map[string]any{"input_tokens": -1.0}); bad.InputTokens != nil {
		t.Fatal("accepted a negative counter")
	}
}

func TestWindowsIgnoreUnknownShapes(t *testing.T) {
	for _, value := range []any{nil, "x", map[string]any{"unifiedWindows": []any{}},
		map[string]any{"unifiedWindows": map[string]any{"five_hour": map[string]any{"utilization": "high"}}}} {
		if windows := claudeWindows(value); windows != nil {
			t.Fatalf("claudeWindows(%v) = %v", value, windows)
		}
	}
	for _, value := range []any{nil, map[string]any{"primary": nil, "secondary": map[string]any{"used_percent": 5.0}}} {
		if windows := codexWindows(value); windows != nil {
			t.Fatalf("codexWindows(%v) = %v", value, windows)
		}
	}
	windows := codexWindows(map[string]any{"primary": map[string]any{"used_percent": 12.5, "window_minutes": 300.0, "resets_at": 1.0e9}})
	if len(windows) != 1 || windows[0].Name != "five_hour" || windows[0].UsedPercent != 12.5 {
		t.Fatalf("windows = %+v", windows)
	}
}
