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

	mu        sync.Mutex
	delivered int
	failed    int
}

func NewWebhooks(c *client.Client, targets []string, log *slog.Logger) *Webhooks {
	return &Webhooks{
		client:   c,
		targets:  targets,
		log:      log,
		deadline: 5 * time.Second,
		http:     &http.Client{Timeout: 5 * time.Second},
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
				w.deliver(ctx, ev)
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

// deliver POSTs one event to every target concurrently. A slow or broken target
// must not hold up the others, and none of them can hold up the stream.
func (w *Webhooks) deliver(ctx context.Context, ev protocol.Event) {
	// Forwarded verbatim, including Truncated. A large commit arrives with a
	// partial list and that flag, and the receiver is expected to read
	// /v1/changes for the rest. Filling it in here would make the relay
	// stateful, which is the one thing it is not.
	body, err := json.Marshal(ev)
	if err != nil {
		w.log.Error("webhook: cannot encode event", "err", err)
		return
	}

	var wg sync.WaitGroup
	for _, url := range w.targets {
		wg.Add(1)
		go func(url string) {
			defer wg.Done()
			w.post(ctx, url, body, ev.Head)
		}(url)
	}
	wg.Wait()
}

func (w *Webhooks) post(ctx context.Context, url string, body []byte, head string) {
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
		w.log.Warn("webhook: delivery failed, dropping", "url", url, "head", short(head), "err", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		w.note(false)
		w.log.Warn("webhook: target rejected the event, dropping",
			"url", url, "head", short(head), "status", resp.StatusCode)
		return
	}
	w.note(true)
	w.log.Debug("webhook: delivered", "url", url, "head", short(head))
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

// Stats is for the health endpoint: a silently failing webhook is worse than a
// noisy one, so the counts are visible without reading logs.
func (w *Webhooks) Stats() (delivered, failed int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.delivered, w.failed
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
