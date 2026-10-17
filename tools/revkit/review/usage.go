package review

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// Usage keeps a reviewer's native token counters; nil means the client did
// not report that counter, never zero. The clients count differently:
// Claude's input excludes cache reads and writes, Codex's includes cache
// reads. TotalInput gives a figure that compares across both.
type Usage struct {
	InputTokens           *int64 `json:"input_tokens"`
	CachedInputTokens     *int64 `json:"cached_input_tokens"`
	CacheWriteInputTokens *int64 `json:"cache_write_input_tokens"`
	OutputTokens          *int64 `json:"output_tokens"`
	ReasoningOutputTokens *int64 `json:"reasoning_output_tokens"`
}

// TotalInput returns every input token the run consumed, cache reads and
// writes included, and whether the client reported enough to tell.
func (u Usage) TotalInput(client string) (int64, bool) {
	if u.InputTokens == nil {
		return 0, false
	}
	total := *u.InputTokens
	if client == "claude" {
		if u.CachedInputTokens == nil || u.CacheWriteInputTokens == nil {
			return 0, false
		}
		total += *u.CachedInputTokens + *u.CacheWriteInputTokens
	} else if u.CacheWriteInputTokens != nil {
		total += *u.CacheWriteInputTokens
	}
	return total, true
}

func claudeUsage(value any) Usage {
	fields, _ := value.(map[string]any)
	return Usage{
		InputTokens:           count(fields, "input_tokens"),
		CachedInputTokens:     count(fields, "cache_read_input_tokens"),
		CacheWriteInputTokens: count(fields, "cache_creation_input_tokens"),
		OutputTokens:          count(fields, "output_tokens"),
	}
}

func codexUsage(value any) Usage {
	fields, _ := value.(map[string]any)
	return Usage{
		InputTokens:           count(fields, "input_tokens"),
		CachedInputTokens:     count(fields, "cached_input_tokens"),
		CacheWriteInputTokens: count(fields, "cache_write_input_tokens"),
		OutputTokens:          count(fields, "output_tokens"),
		ReasoningOutputTokens: count(fields, "reasoning_output_tokens"),
	}
}

// count reads a non-negative whole number, or nil.
func count(fields map[string]any, name string) *int64 {
	number, ok := fields[name].(float64)
	if !ok || number < 0 || number != math.Trunc(number) || number > 1<<53 {
		return nil
	}
	value := int64(number)
	return &value
}

// Window is one usage window a client reported during a run. The shapes
// are undocumented client output: anything missing or unexpected leaves the
// window out, so no window means "capacity unknown", never "available".
type Window struct {
	Client      string    `json:"client"`
	Name        string    `json:"name"`
	UsedPercent float64   `json:"usedPercent"`
	ResetsAt    time.Time `json:"resetsAt"`
}

// claudeWindows reads rate_limit_info.unifiedWindows from Claude's stream:
// {"five_hour": {"utilization": 0.18, "resetsAt": <epoch seconds>}, ...}.
func claudeWindows(value any) []Window {
	info, _ := value.(map[string]any)
	unified, _ := info["unifiedWindows"].(map[string]any)
	var windows []Window
	for _, name := range sortedKeys(unified) {
		fields, _ := unified[name].(map[string]any)
		utilization, ok := fields["utilization"].(float64)
		resets, ok2 := fields["resetsAt"].(float64)
		if !ok || !ok2 || utilization < 0 || resets <= 0 {
			continue
		}
		windows = append(windows, Window{Client: "claude", Name: name,
			UsedPercent: math.Round(utilization*1000) / 10, ResetsAt: time.Unix(int64(resets), 0).UTC()})
	}
	return windows
}

// codexWindows reads rate_limits from a Codex token_count event:
// {"primary": {"used_percent": 28.0, "window_minutes": 10080, "resets_at": <epoch seconds>}, "secondary": ...}.
func codexWindows(value any) []Window {
	limits, _ := value.(map[string]any)
	var windows []Window
	for _, key := range []string{"primary", "secondary"} {
		fields, _ := limits[key].(map[string]any)
		used, ok := fields["used_percent"].(float64)
		minutes, ok2 := fields["window_minutes"].(float64)
		resets, ok3 := fields["resets_at"].(float64)
		if !ok || !ok2 || !ok3 || used < 0 || minutes <= 0 || resets <= 0 {
			continue
		}
		windows = append(windows, Window{Client: "codex", Name: windowName(int64(minutes)),
			UsedPercent: used, ResetsAt: time.Unix(int64(resets), 0).UTC()})
	}
	return windows
}

// windowName names a window by its length, the way Claude names its own.
func windowName(minutes int64) string {
	switch minutes {
	case 5 * 60:
		return "five_hour"
	case 7 * 24 * 60:
		return "seven_day"
	}
	return fmt.Sprintf("%d_minute", minutes)
}

func sortedKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
