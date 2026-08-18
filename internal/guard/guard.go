// Package guard bounds how fast content may enter the vault.
//
// It exists because nothing else does: git keeps every revision, and for
// binary content every revision is a full copy, so a client stuck in a write
// loop grows the repository until the disk is full. The guard holds counters
// and nothing else -- no git, no vault, no HTTP -- so it can be reasoned about
// and tested on its own.
package guard

import (
	"sync"
	"time"
)

// Error codes returned in a Verdict. protocol.Code* carries the same strings;
// they are repeated here rather than imported so this package depends on
// nothing.
const (
	CodeQuarantined = "path_quarantined"
	CodeThrottled   = "throttled"
	CodeDiskLow     = "disk_low"
)

// Limits are the thresholds. A zero value for any counter disables that
// counter; it does not mean "block everything".
type Limits struct {
	// Writes is the most writes one path may take within Window.
	Writes int
	// PathBytes is the most bytes one path may take within Window. Only
	// consulted once a path has taken at least two writes in the window, so a
	// single large upload can never trip it.
	PathBytes int64
	// TotalBytes is the most bytes the whole vault may take within Window.
	TotalBytes int64
	// Window is the rolling window all three counters share.
	Window time.Duration
	// Cooldown is how long a quarantine holds after the last attempt.
	Cooldown time.Duration
	// MinFreeBytes refuses writes when the filesystem has less free.
	MinFreeBytes int64
}

// Defaults are the shipped thresholds.
//
// Writes is derived rather than guessed: Obsidian auto-saves, and the plugin
// debounces sync 2s after typing stops, so one device cannot exceed roughly
// 150 writes to a path in five minutes. 300 clears two devices. A loop writes
// at network speed, orders of magnitude above it.
var Defaults = Limits{
	Writes:       300,
	PathBytes:    100 << 20,
	TotalBytes:   2 << 30,
	Window:       5 * time.Minute,
	Cooldown:     15 * time.Minute,
	MinFreeBytes: 20 << 30,
}

// Verdict is the answer to one admission request. The zero Verdict allows.
type Verdict struct {
	Code   string
	Reason string
	Retry  time.Duration
}

// OK reports whether the write may proceed.
func (v Verdict) OK() bool { return v.Code == "" }

type event struct {
	at    time.Time
	bytes int64
}

// FreeFunc reports free bytes on the vault filesystem. Injected so tests do
// not need a real full disk.
type FreeFunc func() (int64, error)

type Guard struct {
	mu   sync.Mutex
	lim  Limits
	now  func() time.Time
	free FreeFunc

	paths       map[string][]event
	total       []event
	quarantined map[string]time.Time
}

func New(l Limits, now func() time.Time, free FreeFunc) *Guard {
	if now == nil {
		now = time.Now
	}
	return &Guard{
		lim:         l,
		now:         now,
		free:        free,
		paths:       map[string][]event{},
		quarantined: map[string]time.Time{},
	}
}

// Admit records a write of n bytes to path and reports whether it may
// proceed. It records first and judges after, so the write that crosses a
// threshold lands and the next one is refused. That keeps the boundary
// unambiguous: the threshold is the last allowed value, not the first refused.
func (g *Guard) Admit(path string, n int64) Verdict {
	g.mu.Lock()
	defer g.mu.Unlock()

	now := g.now()
	cutoff := now.Add(-g.lim.Window)

	if until, ok := g.quarantined[path]; ok {
		if now.Before(until) {
			return Verdict{
				Code:   CodeQuarantined,
				Reason: "path is quarantined after repeated writes",
				Retry:  until.Sub(now),
			}
		}
		delete(g.quarantined, path)
	}

	if g.lim.MinFreeBytes > 0 && g.free != nil {
		// A probe failure allows the write. Failing closed would take the
		// vault down over a statfs error on a disk that is fine, which is a
		// worse outcome than the growth this floor exists to bound.
		if free, err := g.free(); err == nil && free < g.lim.MinFreeBytes {
			return Verdict{
				Code:   CodeDiskLow,
				Reason: "free disk below the configured floor",
			}
		}
	}

	g.paths[path] = append(prune(g.paths[path], cutoff), event{at: now, bytes: n})
	g.total = append(prune(g.total, cutoff), event{at: now, bytes: n})

	if g.lim.Writes > 0 && len(g.paths[path]) > g.lim.Writes {
		g.quarantined[path] = now.Add(g.lim.Cooldown)
		return Verdict{
			Code:   CodeQuarantined,
			Reason: "write rate exceeded for this path",
			Retry:  g.lim.Cooldown,
		}
	}

	// At least two writes before bytes can trip. One write is not a loop,
	// however large, and a single legitimate attachment must never quarantine
	// its own path. Single-upload size is bounded by protocol.MaxUploadBytes,
	// which is a separate control.
	if g.lim.PathBytes > 0 && len(g.paths[path]) >= 2 && sum(g.paths[path]) > g.lim.PathBytes {
		g.quarantined[path] = now.Add(g.lim.Cooldown)
		return Verdict{
			Code:   CodeQuarantined,
			Reason: "byte rate exceeded for this path",
			Retry:  g.lim.Cooldown,
		}
	}

	if g.lim.TotalBytes > 0 && sum(g.total) > g.lim.TotalBytes {
		return Verdict{
			Code:   CodeThrottled,
			Reason: "vault-wide byte rate exceeded",
			Retry:  g.lim.Window,
		}
	}

	return Verdict{}
}

// Pressure reports whether any quarantine is currently in force.
//
// The local write path uses this. It cannot refuse a write -- the bytes are
// already on disk by the time it sees them -- so it widens its commit
// debounce instead, which collapses a loop's intermediate revisions into a
// single blob per window.
func (g *Guard) Pressure() bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	now := g.now()
	for path, until := range g.quarantined {
		if now.Before(until) {
			return true
		}
		delete(g.quarantined, path)
	}
	return false
}

// prune drops events older than cutoff, reusing the backing array.
func prune(evs []event, cutoff time.Time) []event {
	keep := evs[:0]
	for _, e := range evs {
		if e.at.After(cutoff) {
			keep = append(keep, e)
		}
	}
	return keep
}

func sum(evs []event) int64 {
	var n int64
	for _, e := range evs {
		n += e.bytes
	}
	return n
}
