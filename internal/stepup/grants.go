package stepup

import (
	"sync"
	"time"
)

// Grants is the time-boxed permission an unlock creates, keyed by token and
// vault.
//
// In memory, and it dies with the process. A deploy therefore revokes every
// grant, which is the conservative direction: you did not consent to the new
// binary, and the cost lands on automated callers that can ask again rather
// than on a human at 2am.
//
// Expiry is scheduled, not swept. A polled sweep would leave a quiet stream
// authorised past its absolute deadline for up to the sweep interval, and that
// deadline is the entire promise of an absolute TTL.
type Grants struct {
	ttl time.Duration
	now func() time.Time

	mu     sync.Mutex
	closed bool
	live   map[string]*grant
}

type grant struct {
	until  time.Time
	lapsed chan struct{}
	timer  *time.Timer
}

func NewGrants(ttl time.Duration, now func() time.Time) *Grants {
	if now == nil {
		now = time.Now
	}
	return &Grants{ttl: ttl, now: now, live: map[string]*grant{}}
}

func grantKey(tokenHash, vault string) string { return tokenHash + "\x00" + vault }

// Grant starts a window, replacing any existing one. Expiry is absolute from
// this moment and is never extended by use: sliding renewal would convert
// consent-per-window into consent-once, and the clients most likely to hold a
// grant open forever are exactly the automated ones the gate exists to bound.
func (g *Grants) Grant(tokenHash, vault string) time.Time {
	key := grantKey(tokenHash, vault)
	until := g.now().Add(g.ttl)

	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return until
	}
	g.dropLocked(key)

	entry := &grant{until: until, lapsed: make(chan struct{})}
	// AfterFunc runs on wall time while `until` is measured on the injected
	// clock. A test clock that jumps expires through Held; the timer is what
	// makes a real deadline fire with nobody asking. Both paths end at
	// dropLocked, which is idempotent, and the identity check stops a
	// superseded timer expiring its successor.
	entry.timer = time.AfterFunc(g.ttl, func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		if cur, ok := g.live[key]; ok && cur == entry {
			g.dropLocked(key)
		}
	})
	g.live[key] = entry
	return until
}

// Held reports whether the window is open. It does not extend it.
func (g *Grants) Held(tokenHash, vault string) bool {
	key := grantKey(tokenHash, vault)

	g.mu.Lock()
	defer g.mu.Unlock()
	entry, ok := g.live[key]
	if !ok {
		return false
	}
	if !g.now().Before(entry.until) {
		g.dropLocked(key)
		return false
	}
	return true
}

// HeldWatch answers "is the window open" and "tell me when it closes" in ONE
// lock acquisition, for the exact grant that was live at that instant.
//
// Calling Held and then Watch separately is a TOCTOU window: a re-grant landing
// between them hands the caller the SUCCESSOR's channel, so a request admitted
// under the old grant survives that grant being replaced -- which re-granting
// deliberately closes watchers to prevent.
func (g *Grants) HeldWatch(tokenHash, vault string) (<-chan struct{}, bool) {
	key := grantKey(tokenHash, vault)

	g.mu.Lock()
	defer g.mu.Unlock()
	entry, ok := g.live[key]
	if !ok {
		return closedChan(), false
	}
	if !g.now().Before(entry.until) {
		g.dropLocked(key)
		return closedChan(), false
	}
	return entry.lapsed, true
}

func closedChan() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

// Watch returns a channel closed when the grant ends. A key with no live grant
// gets an already-closed channel, so a caller that never had one is not left
// waiting for a lapse that cannot come.
func (g *Grants) Watch(tokenHash, vault string) <-chan struct{} {
	key := grantKey(tokenHash, vault)

	g.mu.Lock()
	defer g.mu.Unlock()
	entry, ok := g.live[key]
	if !ok || !g.now().Before(entry.until) {
		return closedChan()
	}
	return entry.lapsed
}

// Close releases every grant and watcher. Bound to the server's lifetime so no
// timer outlives it.
func (g *Grants) Close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closed = true
	for key := range g.live {
		g.dropLocked(key)
	}
}

// dropLocked removes one grant, stops its timer and releases its watchers.
// Idempotent: the timer callback and an expiry noticed by Held both land here.
func (g *Grants) dropLocked(key string) {
	entry, ok := g.live[key]
	if !ok {
		return
	}
	delete(g.live, key)
	if entry.timer != nil {
		entry.timer.Stop()
	}
	close(entry.lapsed)
}
