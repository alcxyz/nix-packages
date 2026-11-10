package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"time"
	"unicode/utf8"
)

// The alert loop's timing (ADR-0013).
var (
	liveInterval      = time.Minute
	liveNotifyTimeout = 30 * time.Second
)

const (
	// liveQuiet is the least time between two alerts for one page.
	liveQuiet = time.Hour
	// liveRetry is the least time between attempts at an alert whose
	// command failed.
	liveRetry = 10 * time.Minute
	// liveTextLimit bounds the alert text, in characters.
	liveTextLimit = 500
	// liveForget is how long a page may be absent from the passes before
	// its record is dropped. A pass that cannot read the content directory,
	// a collection or a bundle for a while sees fewer pages; keeping their
	// records keeps the last status and send time, so the page's return
	// is no turn to act and the hourly limit still holds.
	liveForget = 24 * time.Hour
)

// liveRecord is what the state file keeps about one page.
type liveRecord struct {
	Status string    `json:"status"`
	SentAt time.Time `json:"sentAt,omitzero"`
	// Pending is set while an alert for the page's turn to act has not gone
	// out because its command failed, at FailedAt.
	Pending  bool      `json:"pending,omitempty"`
	FailedAt time.Time `json:"failedAt,omitzero"`
	// LastSeen is when a pass last found the page; a state file written
	// before deck recorded it loads with the load time.
	LastSeen time.Time `json:"lastSeen,omitzero"`
}

type liveState struct {
	Pages map[string]liveRecord `json:"pages"`
}

// liveAlerts runs the [live] notify command when a live page turns to act.
// One goroutine owns it.
type liveAlerts struct {
	deck   *Deck
	cfg    LiveConfig
	origin string
	log    *slog.Logger
	// run runs the alert command with its arguments filled in.
	run func(ctx context.Context, argv []string) error

	state   liveState
	loaded  bool
	written []byte
	// writeErr is the last failure to write the state file, logged once.
	writeErr string
}

func newLiveAlerts(d *Deck) *liveAlerts {
	return &liveAlerts{
		deck:   d,
		cfg:    *d.cfg.Live,
		origin: d.cfg.PublicOrigin,
		log:    d.log,
		run:    runNotify,
	}
}

// loop checks the live pages once a minute until ctx ends.
func (a *liveAlerts) loop(ctx context.Context) {
	ticker := time.NewTicker(liveInterval)
	defer ticker.Stop()
	for {
		a.pass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// livePage is a page with a tile, as one pass sees it. current is false
// for a tile that is missing, unreadable or stale.
type livePage struct {
	url, title, headline, status string
	current                      bool
}

// pass reads every tile page's status, as the overview does, alerts on each
// turn to act, and records the statuses.
//
// Only a current tile changes a page's record: a missing, unreadable or
// stale one leaves it as it was, so a blip is never a turn, and sends
// nothing, not even a pending retry. A page absent from the pass keeps its
// record until it has been gone for liveForget. On cancellation the pass
// saves what it has done so far.
func (a *liveAlerts) pass(ctx context.Context) {
	var pages []livePage
	for _, c := range a.deck.collections() {
		for _, p := range c.Pages {
			if p.Tile {
				pages = append(pages, livePage{url: p.URL, title: p.Title, headline: p.Live.Headline, status: p.Live.Status, current: p.Live.Current()})
			}
		}
	}
	// Without a state file every page is new: record it as it is, so a
	// first start never alerts on what was already the case.
	baseline := false
	if !a.loaded {
		baseline = !a.load()
	}
	now := a.deck.now()
	defer a.save()
	for _, p := range pages {
		rec := a.state.Pages[p.url]
		rec.LastSeen = now
		if p.current {
			if baseline {
				rec.Status = p.status
			} else if !a.update(ctx, &rec, p, now) {
				// Cancelled while sending: this page's record stays as it
				// was, and the deferred save keeps the others.
				return
			}
		}
		a.state.Pages[p.url] = rec
	}
	for url, rec := range a.state.Pages {
		if now.Sub(rec.LastSeen) > liveForget {
			delete(a.state.Pages, url)
		}
	}
}

// update applies a current reading of p to its record, sending the alert
// when it is due, and reports false when ctx ended during the send.
func (a *liveAlerts) update(ctx context.Context, rec *liveRecord, p livePage, now time.Time) bool {
	if p.status != tileAct {
		rec.Pending = false
	} else if rec.Status != tileAct && now.Sub(rec.SentAt) >= liveQuiet {
		rec.Pending = true
		rec.FailedAt = time.Time{}
	}
	if rec.Pending && now.Sub(rec.FailedAt) >= liveRetry {
		if err := a.notify(ctx, p); err != nil {
			if ctx.Err() != nil {
				return false
			}
			a.log.Warn("live alert failed; retrying later", "page", p.url, "err", err.Error())
			rec.FailedAt = now
		} else {
			a.log.Info("live alert sent", "page", p.url)
			rec.Pending = false
			rec.SentAt = now
			rec.FailedAt = time.Time{}
		}
	}
	rec.Status = p.status
	return true
}

// load reads the state file and reports whether there was one. Without
// one, or with one deck cannot read, the first pass records the statuses and
// sends nothing.
func (a *liveAlerts) load() bool {
	a.loaded = true
	a.state = liveState{Pages: map[string]liveRecord{}}
	data, err := os.ReadFile(a.cfg.StateFile)
	if errors.Is(err, os.ErrNotExist) {
		a.log.Info("live alerts: no state file yet; recording the current statuses without alerting", "stateFile", a.cfg.StateFile)
		return false
	}
	if err == nil {
		err = json.Unmarshal(data, &a.state)
	}
	if err != nil {
		a.log.Warn("live alerts: state file unreadable; recording the current statuses without alerting", "stateFile", a.cfg.StateFile, "err", err.Error())
		a.state = liveState{Pages: map[string]liveRecord{}}
		return false
	}
	if a.state.Pages == nil {
		a.state.Pages = map[string]liveRecord{}
	}
	now := a.deck.now()
	for url, rec := range a.state.Pages {
		if rec.LastSeen.IsZero() {
			rec.LastSeen = now
			a.state.Pages[url] = rec
		}
	}
	a.written = data
	return true
}

// save writes the state file when it changed, atomically and readable by
// deck's user only.
func (a *liveAlerts) save() {
	data, err := json.MarshalIndent(a.state, "", "  ")
	if err != nil {
		a.log.Error("live alerts: cannot encode the state", "err", err.Error())
		return
	}
	data = append(data, '\n')
	if string(data) == string(a.written) {
		return
	}
	if err := writeFileAtomic(a.cfg.StateFile, data); err != nil {
		if err.Error() != a.writeErr {
			a.log.Error("live alerts: cannot write the state file", "stateFile", a.cfg.StateFile, "err", err.Error())
		}
		a.writeErr = err.Error()
		return
	}
	a.writeErr = ""
	a.written = data
}

// notify runs the alert command for p.
func (a *liveAlerts) notify(ctx context.Context, p livePage) error {
	return a.run(ctx, notifyArgs(a.cfg.Notify, liveText(p.title, p.headline), a.origin+p.url))
}

// notifyArgs fills the arguments that are exactly {text} or {link}.
func notifyArgs(argv []string, text, link string) []string {
	args := make([]string, len(argv))
	for i, arg := range argv {
		switch arg {
		case "{text}":
			args[i] = text
		case "{link}":
			args[i] = link
		default:
			args[i] = arg
		}
	}
	return args
}

// liveText is the alert's text, cut to liveTextLimit characters.
func liveText(title, headline string) string {
	text := title + " needs action: " + headline
	if utf8.RuneCountInString(text) <= liveTextLimit {
		return text
	}
	runes := []rune(text)
	return string(runes[:liveTextLimit-1]) + "…"
}

// runNotify runs the alert command directly, without a shell, in its own
// process group, for at most liveNotifyTimeout. Its output is discarded:
// deck logs only how it ended.
func runNotify(ctx context.Context, argv []string) error {
	ctx, cancel := context.WithTimeout(ctx, liveNotifyTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if err := runGroup(cmd); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("%w after %s", err, liveNotifyTimeout)
		}
		return err
	}
	return nil
}
