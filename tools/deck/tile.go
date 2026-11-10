package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// tileLimit bounds tile.json, which a refresher writes beside its page's
// data (ADR-0013).
const tileLimit = 64 * 1024

// The effective statuses of a tile, in the order the tiles sort.
const (
	tileAct   = "act"
	tileWatch = "watch"
	tileError = "error"
	tileOK    = "ok"
)

var tileRank = map[string]int{tileAct: 0, tileWatch: 1, tileError: 2, tileOK: 3}

const (
	tileMissingHeadline = "No data yet."
	tileInvalidHeadline = "The tile's data could not be read."
)

// tileWarnInterval is how long deck stays quiet about the same unreadable
// tile.json before logging it again.
const tileWarnInterval = time.Hour

// Tile is a live page's tile as deck shows it: tile.json, validated, with
// its status raised for stale data.
type Tile struct {
	// Status is act, watch, error or ok.
	Status   string
	Headline string
	Metrics  []TileMetric
	Spark    *Sparkline
	// Fresh says how old the data is, such as "5 min ago" or
	// "stale, 2 h ago"; empty when tile.json gives no time.
	Fresh   string
	Stale   bool
	Updated time.Time
	// NoData is set when the bundle has no tile.json yet.
	NoData bool
}

type TileMetric struct {
	Label string `json:"label"`
	Value string `json:"value"`
	// Tone is "", ok, watch or act.
	Tone string `json:"tone"`
}

// Sparkline holds an inline SVG sparkline's points in a 100 by 30 box.
type Sparkline struct {
	Label string
	Line  string
	Area  string
}

// tileFile is tile.json as a refresher writes it; unknown fields are
// ignored.
type tileFile struct {
	Status   string       `json:"status"`
	Headline string       `json:"headline"`
	Metrics  []TileMetric `json:"metrics"`
	Series   *struct {
		Label  string     `json:"label"`
		Values []*float64 `json:"values"`
	} `json:"series"`
	Updated    *string `json:"updated"`
	StaleAfter float64 `json:"staleAfter"`
}

// StateText is the status as the tile's hidden label reads it.
func (t Tile) StateText() string {
	switch t.Status {
	case tileAct:
		return "Needs action."
	case tileWatch:
		return "Worth a look."
	case tileOK:
		return "OK."
	}
	return "Unavailable."
}

// UpdatedText is the data's time for the freshness label's title.
func (t Tile) UpdatedText() string {
	if t.Updated.IsZero() {
		return ""
	}
	return "Data from " + t.Updated.Local().Format("2 Jan 2006 15:04 MST")
}

// Current reports whether the tile states its page's status: tile.json
// was read and valid, and its data is not stale. Only a current tile changes
// what the live alerts record (ADR-0013).
func (t Tile) Current() bool {
	return t.Status != tileError && !t.NoData && !t.Stale
}

// NeedsLook reports whether the tile counts in the shell and the "Needs
// you" row.
func (t Tile) NeedsLook() bool {
	return t.Status == tileAct || t.Status == tileWatch || t.Status == tileError
}

// readTile reads and validates the bundle's tile.json at now. A missing
// file is a watch tile with no data; an unreadable or invalid one is an
// error tile, and the returned error says why.
func readTile(dir *os.Root, now time.Time) (Tile, error) {
	// openContentFile refuses a symlink or any other non-regular file, as
	// it does for every asset (ADR-0013).
	f, info, err := openContentFile(dir, "tile.json")
	if errors.Is(err, os.ErrNotExist) {
		return Tile{Status: tileWatch, Headline: tileMissingHeadline, NoData: true}, nil
	}
	invalid := Tile{Status: tileError, Headline: tileInvalidHeadline}
	if err != nil {
		return invalid, err
	}
	defer f.Close()
	if info.Size() > tileLimit {
		return invalid, errors.New("tile.json exceeds 64 KiB")
	}
	data, err := io.ReadAll(io.LimitReader(f, tileLimit+1))
	if err != nil {
		return invalid, err
	}
	if len(data) > tileLimit {
		return invalid, errors.New("tile.json exceeds 64 KiB")
	}
	var file tileFile
	if err := json.Unmarshal(data, &file); err != nil {
		return invalid, fmt.Errorf("tile.json: %w", err)
	}
	tile, err := file.tile(now)
	if err != nil {
		return invalid, fmt.Errorf("tile.json: %w", err)
	}
	return tile, nil
}

// tile validates the file against the contract (ADR-0013) and works out the
// tile at now.
func (f tileFile) tile(now time.Time) (Tile, error) {
	if !oneOf(f.Status, []string{tileOK, tileWatch, tileAct}) {
		return Tile{}, fmt.Errorf("status %q must be ok, watch or act", f.Status)
	}
	if strings.TrimSpace(f.Headline) == "" || utf8.RuneCountInString(f.Headline) > 200 {
		return Tile{}, errors.New("headline must be nonempty and at most 200 characters")
	}
	if len(f.Metrics) > 3 {
		return Tile{}, errors.New("at most 3 metrics")
	}
	for i, m := range f.Metrics {
		// A value fits the tile's large figure at 7 characters or fewer; up to
		// 12 it is cut with an ellipsis rather than costing the whole tile.
		if utf8.RuneCountInString(m.Label) > 40 || utf8.RuneCountInString(m.Value) > 12 {
			return Tile{}, fmt.Errorf("metric %d: label at most 40 and value at most 12 characters", i+1)
		}
		if m.Tone != "" && !oneOf(m.Tone, []string{tileOK, tileWatch, tileAct}) {
			return Tile{}, fmt.Errorf("metric %d: tone %q must be ok, watch or act", i+1, m.Tone)
		}
	}
	if !(f.StaleAfter > 0) {
		return Tile{}, errors.New("staleAfter must be above 0")
	}
	tile := Tile{Status: f.Status, Headline: f.Headline, Metrics: f.Metrics}
	if f.Series != nil {
		if n := len(f.Series.Values); n < 2 || n > 60 {
			return Tile{}, errors.New("series must have 2 to 60 values")
		}
		tile.Spark = sparkline(f.Series.Label, f.Series.Values)
	}
	if f.Updated != nil && *f.Updated != "" {
		updated, err := time.Parse(time.RFC3339, *f.Updated)
		if err != nil {
			return Tile{}, fmt.Errorf("updated %q must be RFC 3339", *f.Updated)
		}
		tile.Updated = updated
		age := now.Sub(updated)
		tile.Fresh = ago(age)
		if age > time.Duration(f.StaleAfter*float64(time.Minute)) {
			tile.Stale = true
			tile.Fresh = "stale, " + tile.Fresh
			if tile.Status == tileOK {
				tile.Status = tileWatch
			}
		}
	}
	return tile, nil
}

// ago says how long ago, as the tiles always have: "just now", minutes
// under an hour, hours under two days, then days.
func ago(d time.Duration) string {
	m := int(math.Round(math.Max(0, d.Minutes())))
	if m < 1 {
		return "just now"
	}
	if m < 60 {
		return strconv.Itoa(m) + " min ago"
	}
	h := int(math.Round(float64(m) / 60))
	if h < 48 {
		return strconv.Itoa(h) + " h ago"
	}
	return strconv.Itoa(int(math.Round(float64(h)/24))) + " d ago"
}

// sparkline draws the series' numbers, skipping missing ones, in a 100 by
// 30 box from the lower of 0 and the smallest value; nil with fewer than
// two numbers.
func sparkline(label string, values []*float64) *Sparkline {
	var nums []float64
	for _, v := range values {
		if v != nil {
			nums = append(nums, *v)
		}
	}
	if len(nums) < 2 {
		return nil
	}
	const w, h, pad = 100.0, 30.0, 2.0
	lo, hi := 0.0, nums[0]
	for _, v := range nums {
		lo = math.Min(lo, v)
		hi = math.Max(hi, v)
	}
	span := hi - lo
	if span == 0 {
		span = 1
	}
	points := make([]string, len(nums))
	for i, v := range nums {
		x := float64(i) / float64(len(nums)-1) * w
		y := h - pad - (v-lo)/span*(h-2*pad)
		points[i] = fixed2(x) + "," + fixed2(y)
	}
	line := strings.Join(points, " ")
	return &Sparkline{Label: label, Line: line, Area: "0,30 " + line + " 100,30"}
}

// fixed2 formats a nonnegative v with two decimals as JavaScript's
// toFixed(2) does: Go rounds an exact tie to even, JavaScript up.
func fixed2(v float64) string {
	if p := v * 200; p == math.Trunc(p) && math.FMA(v, 200, -p) == 0 && math.Mod(p, 2) == 1 {
		v = math.Nextafter(v, math.Inf(1))
	}
	return strconv.FormatFloat(v, 'f', 2, 64)
}

// sortTiles puts the tiles that need a look first, act, watch, error then
// ok, and keeps the order within each.
func sortTiles(pages []ContentPage) {
	sort.SliceStable(pages, func(i, j int) bool {
		return tileRank[pages[i].Live.Status] < tileRank[pages[j].Live.Status]
	})
}

type tileWarning struct {
	reason string
	at     time.Time
}

// warnTile logs why a page's tile.json could not be read, once an hour per
// page unless the reason changes.
func (d *Deck) warnTile(url string, err error) {
	now := d.now()
	d.tileMu.Lock()
	last, seen := d.tileWarned[url]
	quiet := seen && last.reason == err.Error() && now.Sub(last.at) < tileWarnInterval
	if !quiet {
		if d.tileWarned == nil {
			d.tileWarned = map[string]tileWarning{}
		}
		d.tileWarned[url] = tileWarning{reason: err.Error(), at: now}
	}
	d.tileMu.Unlock()
	if !quiet {
		d.log.Warn("live tile unreadable", "page", url, "err", err.Error())
	}
}
