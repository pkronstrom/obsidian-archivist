package reconcile

import (
	"path"
	"strings"
	"sync"
	"time"

	"github.com/pkronstrom/vaultsync/internal/merge"
	"github.com/pkronstrom/vaultsync/internal/repo"
)

// maxInlineChanges bounds what a single event carries. A bulk import can touch
// thousands of paths, and an event that large is useless to a consumer and
// expensive to fan out. Past this the event says how many there were and the
// consumer reads /v1/changes, which is the durable path anyway.
const maxInlineChanges = 100

// ChangedFile is one path in an event, with enough metadata to triage on
// without a round-trip: is it a note or an attachment, how big, did it appear
// or vanish.
type ChangedFile struct {
	Path string `json:"path"`
	Op   string `json:"op"`   // "put" or "del"
	Ext  string `json:"ext"`  // "md", "pdf", "" -- lowercase, no dot
	Kind string `json:"kind"` // "text" or "binary"
	Size int64  `json:"size"`
	Hash string `json:"hash"`
}

// Event is one commit.
type Event struct {
	Head      string        `json:"head"`
	Prev      string        `json:"prev"`
	When      time.Time     `json:"when"`
	Count     int           `json:"count"`
	Truncated bool          `json:"truncated,omitempty"`
	Changes   []ChangedFile `json:"changes"`
}

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
	ch chan Event
}

type broadcaster struct {
	mu   sync.Mutex
	subs map[*subscriber]struct{}
}

func newBroadcaster() *broadcaster {
	return &broadcaster{subs: map[*subscriber]struct{}{}}
}

// Subscribe returns a channel of commit hashes and a function to stop.
func (rc *Reconciler) Subscribe() (<-chan Event, func()) {
	s := &subscriber{ch: make(chan Event, 8)}
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

	ev := rc.buildEvent(prev, head)
	rc.events.mu.Lock()
	defer rc.events.mu.Unlock()
	for _, s := range subs {
		select {
		case s.ch <- ev:
		default:
		}
	}
}

func (rc *Reconciler) buildEvent(prev, head string) Event {
	ev := Event{Head: head, Prev: prev, When: time.Now().UTC(), Changes: []ChangedFile{}}
	changes, err := rc.r.Changes(prev, head)
	if err != nil {
		return ev
	}
	ev.Count = len(changes)
	for i, c := range changes {
		if i >= maxInlineChanges {
			ev.Truncated = true
			break
		}
		ev.Changes = append(ev.Changes, rc.describe(c))
	}
	return ev
}

func (rc *Reconciler) describe(c repo.Change) ChangedFile {
	ext := strings.ToLower(strings.TrimPrefix(path.Ext(c.Path), "."))
	f := ChangedFile{Path: c.Path, Op: c.Op, Ext: ext, Size: c.Size, Hash: c.Hash, Kind: "text"}
	if c.Op == "del" {
		return f
	}
	// Sniff rather than trust the extension: an agent deciding whether it can
	// read something cares about the bytes, not the name.
	if content, err := rc.r.ReadBlob(c.Hash); err == nil && merge.IsBinary(content) {
		f.Kind = "binary"
	}
	return f
}
