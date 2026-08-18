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

// Verify posts a startup notification SYNCHRONOUSLY and returns the error.
//
// This is the only synchronous path in the package, and it exists because
// every other one is deliberately unobservable. Delivery must never fail a
// write, so failures are swallowed -- which meant a notifier that could never
// deliver was indistinguishable from a quiet one. The CA-certificate bug lived
// in production that way and surfaced only because someone happened to poll
// the topic.
//
// Doing it once at boot validates configuration, DNS, connectivity and TLS
// together, at the one moment a human is watching, and costs one low-priority
// message per restart. It is sent at minimum priority so it does not buzz a
// phone; its job is to prove the path works, not to be read.
//
// The caller decides what to do with the error. It should not be fatal: a
// broken notifier is not a reason to stop serving the vault.
func (n *Notifier) Verify(title, body string) error {
	if n == nil || n.url == "" {
		return nil
	}
	return n.deliver(title, body, "min", "white_check_mark")
}

func (n *Notifier) post(title, body string, urgent bool) {
	priority, tags := "default", "floppy_disk"
	if urgent {
		priority, tags = "high", "rotating_light"
	}
	// The error is already logged by deliver; post is the fire-and-forget
	// path and has no caller to return it to.
	_ = n.deliver(title, body, priority, tags)
}

// deliver posts once and reports the outcome. Failures are logged here so
// every path through the package is observable, including the ones whose
// callers discard the error.
func (n *Notifier) deliver(title, body, priority, tags string) error {
	req, err := http.NewRequest(http.MethodPost, n.url, bytes.NewReader([]byte(body)))
	if err != nil {
		n.reportFailure(err)
		return err
	}
	req.Header.Set("Title", title)
	req.Header.Set("Priority", priority)
	req.Header.Set("Tags", tags)

	resp, err := n.c.Do(req)
	if err != nil {
		n.reportFailure(err)
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		err := fmt.Errorf("ntfy answered %s", resp.Status)
		n.reportFailure(err)
		return err
	}

	n.mu.Lock()
	n.delivered = true
	n.failures = 0
	n.mu.Unlock()
	return nil
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
