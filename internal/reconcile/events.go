package reconcile

import (
	"sync"
	"time"

	"github.com/pkronstrom/obsidian-archivist/protocol"
)

// Events carry protocol types directly: a duplicate ChangedFile here could
// drift from the one /v1/changes returns, and the whole point is that a
// consumer can use the two interchangeably.

type subscriber struct {
	ch chan protocol.Event
}

type broadcaster struct {
	mu   sync.Mutex
	subs map[*subscriber]struct{}
}

func newBroadcaster() *broadcaster {
	return &broadcaster{subs: map[*subscriber]struct{}{}}
}

// Subscribe returns a channel of commit hashes and a function to stop.
func (rc *Reconciler) Subscribe() (<-chan protocol.Event, func()) {
	s := &subscriber{ch: make(chan protocol.Event, 8)}
	rc.events.mu.Lock()
	rc.events.subs[s] = struct{}{}
	rc.events.mu.Unlock()

	return s.ch, func() {
		rc.events.mu.Lock()
		if _, ok := rc.events.subs[s]; ok {
			delete(rc.events.subs, s)
			close(s.ch)
		}
		rc.events.mu.Unlock()
	}
}

// notify publishes a new head. A slow or stalled subscriber is SKIPPED rather
// than waited on: blocking here would stall the commit path, and a dropped
// notification costs a consumer nothing because its cursor still tells it what
// it missed.
func (rc *Reconciler) notify(prev, head string) {
	if head == "" || head == prev {
		return
	}
	rc.events.mu.Lock()
	subs := make([]*subscriber, 0, len(rc.events.subs))
	for s := range rc.events.subs {
		subs = append(subs, s)
	}
	rc.events.mu.Unlock()
	if len(subs) == 0 {
		return // nobody listening; do not pay for the diff
	}

	// Built with the lock RELEASED: it does git I/O, and holding the lock
	// across that would stall every unsubscribe for the duration.
	ev := rc.buildEvent(prev, head)

	rc.events.mu.Lock()
	defer rc.events.mu.Unlock()
	for _, s := range subs {
		// Re-check membership under the same lock that guards close(). A
		// subscriber can disconnect during the unlocked window above, and
		// sending on a closed channel panics even inside a select -- which
		// would take the whole server down because a client hung up mid-commit.
		if _, live := rc.events.subs[s]; !live {
			continue
		}
		select {
		case s.ch <- ev:
		default: // slow subscriber: drop, the cursor still tells it what it missed
		}
	}
}

func (rc *Reconciler) buildEvent(prev, head string) protocol.Event {
	ev := protocol.Event{Head: head, Prev: prev, When: time.Now().UTC(), Changes: []protocol.Change{}}
	changes, err := rc.r.Changes(prev, head)
	if err != nil {
		return ev
	}
	ev.Count = len(changes)
	for i, c := range changes {
		if i >= protocol.MaxInlineChanges {
			ev.Truncated = true
			break
		}
		ev.Changes = append(ev.Changes, c)
	}
	return ev
}
