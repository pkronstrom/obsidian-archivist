package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/pkronstrom/obsidian-archivist/internal/client"
	"github.com/pkronstrom/obsidian-archivist/protocol"
)

// Webhooks forwards the server's change events to configured URLs.
//
// Delivery is BEST-EFFORT and deliberately so: no acknowledgements, no retry
// queue, no dead-letter. That is not a shortcut, it is the consequence of a
// property the system already has -- git history is a durable, resumable feed,
// so a consumer that needs reliability keeps a cursor and asks
// /v1/changes?since= what it missed, however long it was away.
//
// Adding acknowledgement would make this relay own a queue, its retries and its
// failure modes, and it still could not beat the cursor. So a failed POST is
// logged and dropped, and the receiver recovers on its own terms or does not
// care in the first place.
//
// This lives in the relay rather than the server so the vault authority never
// makes outbound requests: no SSRF surface, no retry policy on the process that
// must not wedge.
type Webhooks struct {
	client   *client.Client
	targets  []string
	log      *slog.Logger
	http     *http.Client
	deadline time.Duration

	// queueDepth bounds each target's backlog. Small on purpose: a target that
	// is more than a few events behind is broken, and this is a notification
	// channel, not a queue -- the receiver recovers from /v1/changes?since=.
	queueDepth int

	mu        sync.Mutex
	delivered int
	failed    int
	dropped   int
}

func NewWebhooks(c *client.Client, targets []string, log *slog.Logger) *Webhooks {
	return &Webhooks{
		client:     c,
		targets:    targets,
		log:        log,
		deadline:   5 * time.Second,
		queueDepth: 16,
		http:       &http.Client{Timeout: 5 * time.Second},
	}
}

// Run follows the event stream and forwards each commit until ctx is done.
//
// It reconnects with backoff, and does NOT try to recover events missed while
// disconnected. See the type comment: that is the cursor's job, not this one's.
func (w *Webhooks) Run(ctx context.Context) error {
	if len(w.targets) == 0 {
		return nil
	}

	// One worker per target, each with its own queue. This is what actually
	// gives the isolation this type claims: a target that is slow or hanging
	// fills only its OWN queue, and the stream loop never waits on any of them.
	//
	// Previously deliver() did a wg.Wait() inline, so a single unresponsive
	// target stalled the reader for the full 5s deadline on every event, and the
	// server drops notifications once a subscriber falls behind -- one broken
	// target silently cost the healthy ones their events.
	queues := make([]chan []byte, len(w.targets))
	var workers sync.WaitGroup
	for i, url := range w.targets {
		q := make(chan []byte, w.queueDepth)
		queues[i] = q
		workers.Add(1)
		go func(url string, q chan []byte) {
			defer workers.Done()
			for body := range q {
				w.post(ctx, url, body)
			}
		}(url, q)
	}
	defer func() {
		for _, q := range queues {
			close(q)
		}
		workers.Wait()
	}()

	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return nil
		}
		events, errs, err := w.client.Events(ctx)
		if err != nil {
			w.log.Error("webhook: cannot open the event stream", "err", err, "retryIn", backoff)
			if !sleep(ctx, backoff) {
				return nil
			}
			backoff = grow(backoff)
			continue
		}
		w.log.Info("webhook: streaming", "targets", len(w.targets))
		backoff = time.Second

	stream:
		for {
			select {
			case <-ctx.Done():
				return nil
			case ev, ok := <-events:
				if !ok {
					break stream
				}
				if ev.Count == 0 && len(ev.Changes) == 0 {
					continue // the head sent on connect; nothing changed
				}
				w.deliver(queues, ev)
			case err, ok := <-errs:
				if ok && err != nil {
					w.log.Warn("webhook: stream dropped", "err", err)
				}
				break stream
			}
		}

		if !sleep(ctx, backoff) {
			return nil
		}
		backoff = grow(backoff)
	}
}

// deliver hands one event to every target's queue and returns immediately. It
// never blocks: a full queue means that target is behind, and the event is
// dropped for THAT target only.
func (w *Webhooks) deliver(queues []chan []byte, ev protocol.Event) {
	// Forwarded verbatim, including Truncated. A large commit arrives with a
	// partial list and that flag, and the receiver is expected to read
	// /v1/changes for the rest. Filling it in here would make the relay
	// stateful, which is the one thing it is not.
	body, err := json.Marshal(ev)
	if err != nil {
		w.log.Error("webhook: cannot encode event", "err", err)
		return
	}

	for i, q := range queues {
		select {
		case q <- body:
		default:
			w.noteDrop()
			w.log.Warn("webhook: target is behind, dropping this event",
				"url", w.targets[i], "head", short(ev.Head), "queue", w.queueDepth)
		}
	}
}

func (w *Webhooks) post(ctx context.Context, url string, body []byte) {
	ctx, cancel := context.WithTimeout(ctx, w.deadline)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		w.note(false)
		w.log.Error("webhook: bad target URL", "url", url, "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "archivist-relay")

	resp, err := w.http.Do(req)
	if err != nil {
		w.note(false)
		w.log.Warn("webhook: delivery failed, dropping", "url", url, "err", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		w.note(false)
		w.log.Warn("webhook: target rejected the event, dropping",
			"url", url, "status", resp.StatusCode)
		return
	}
	w.note(true)
	w.log.Debug("webhook: delivered", "url", url)
}

func (w *Webhooks) note(ok bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if ok {
		w.delivered++
	} else {
		w.failed++
	}
}

func (w *Webhooks) noteDrop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.dropped++
}

// Stats is for the health endpoint: a silently failing webhook is worse than a
// noisy one, so the counts are visible without reading logs. Drops are counted
// separately from failures -- a delivery that was attempted and rejected is a
// different problem from one that was never attempted because a target is behind.
func (w *Webhooks) Stats() (delivered, failed, dropped int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.delivered, w.failed, w.dropped
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func grow(d time.Duration) time.Duration {
	d *= 2
	if d > time.Minute {
		return time.Minute
	}
	return d
}

func short(h string) string {
	if len(h) > 8 {
		return h[:8]
	}
	return h
}
