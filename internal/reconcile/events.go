package reconcile

import "sync"

// Change notification.
//
// The DURABLE feed is already /v1/changes?since=<cursor>: git history is an
// append-only log, so a consumer that stores a cursor can always ask what it
// missed, however long it was away. That is the part worth relying on.
//
// What history cannot provide is latency -- a poller waits for its next tick.
// So this carries a notification and nothing else: "head moved to X". It is
// deliberately lossy. A consumer that misses one still reconciles correctly
// from its cursor, which means there is no delivery guarantee to implement, no
// queue to persist, and no backlog to manage.
//
// Notify, do not deliver.

type subscriber struct {
	ch chan string
}

type broadcaster struct {
	mu   sync.Mutex
	subs map[*subscriber]struct{}
}

func newBroadcaster() *broadcaster {
	return &broadcaster{subs: map[*subscriber]struct{}{}}
}

// Subscribe returns a channel of commit hashes and a function to stop.
func (rc *Reconciler) Subscribe() (<-chan string, func()) {
	s := &subscriber{ch: make(chan string, 8)}
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
func (rc *Reconciler) notify(head string) {
	if head == "" {
		return
	}
	rc.events.mu.Lock()
	defer rc.events.mu.Unlock()
	for s := range rc.events.subs {
		select {
		case s.ch <- head:
		default:
		}
	}
}
