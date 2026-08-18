// Package notify posts best-effort alerts to ntfy.
//
// Best-effort is the whole contract. Delivery runs on its own goroutine,
// every error is swallowed, and no caller ever waits. A notifier that can
// fail a write is worse than no notifier: it would turn a warning about a
// throttled path into a failed sync.
//
// The URL is archivist's own, deliberately not the backup notifier's. restic
// posts on a schedule and its notifications are routine, so a vault alarm
// arriving in that topic would be read as more backup noise. If this topic
// pings, something is wrong.
package notify

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

type Notifier struct {
	url string
	now func() time.Time
	c   *http.Client

	// Cooldown suppresses repeats of the same key. A loop tripping quarantine
	// every second must not produce a push every second.
	Cooldown time.Duration

	// log reports delivery failures. Best-effort must not mean invisible: a
	// notifier that can never deliver looks exactly like a quiet one, and the
	// first version of this package proved it -- the scratch image had no CA
	// certificates, every HTTPS post failed, and nothing anywhere said so.
	log *slog.Logger

	mu        sync.Mutex
	last      map[string]time.Time
	failures  int
	delivered bool
}

func New(url string, now func() time.Time, log *slog.Logger) *Notifier {
	if now == nil {
		now = time.Now
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Notifier{
		log:      log,
		url:      url,
		now:      now,
		c:        &http.Client{Timeout: 15 * time.Second},
		Cooldown: 10 * time.Minute,
		last:     map[string]time.Time{},
	}
}

// Send posts a notification. It never blocks and never returns an error.
func (n *Notifier) Send(title, body string, urgent bool) {
	if n == nil || n.url == "" {
		return
	}
	go n.post(title, body, urgent)
}

// SendKeyed is Send, suppressed to at most one delivery per key per Cooldown.
func (n *Notifier) SendKeyed(key, title, body string, urgent bool) {
	if n == nil || n.url == "" {
		return
	}
	now := n.now()

	n.mu.Lock()
	last, seen := n.last[key]
	if seen && now.Sub(last) < n.Cooldown {
		n.mu.Unlock()
		return
	}
	n.last[key] = now
	n.mu.Unlock()

	go n.post(title, body, urgent)
}

func (n *Notifier) post(title, body string, urgent bool) {
	req, err := http.NewRequest(http.MethodPost, n.url, bytes.NewReader([]byte(body)))
	if err != nil {
		n.reportFailure(err)
		return
	}
	req.Header.Set("Title", title)
	if urgent {
		req.Header.Set("Tags", "rotating_light")
		req.Header.Set("Priority", "high")
	} else {
		req.Header.Set("Tags", "floppy_disk")
		req.Header.Set("Priority", "default")
	}
	resp, err := n.c.Do(req)
	if err != nil {
		n.reportFailure(err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		n.reportFailure(fmt.Errorf("ntfy answered %s", resp.Status))
		return
	}

	n.mu.Lock()
	n.delivered = true
	n.failures = 0
	n.mu.Unlock()
}

// reportFailure logs a delivery failure without ever touching the caller.
//
// The FIRST failure is an error, because a notifier that has never delivered
// anything is almost always misconfigured rather than unlucky -- a wrong URL,
// or no CA certificates in the image. Later failures drop to warn so a flaky
// endpoint cannot flood the log.
func (n *Notifier) reportFailure(err error) {
	n.mu.Lock()
	n.failures++
	count, everDelivered := n.failures, n.delivered
	n.mu.Unlock()

	if count == 1 && !everDelivered {
		n.log.Error("ntfy delivery failed and this notifier has never delivered; "+
			"alerts are silently going nowhere", "url", n.url, "err", err)
		return
	}
	n.log.Warn("ntfy delivery failed", "err", err, "consecutive", count)
}
