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

	mu   sync.Mutex
	last map[string]time.Time
}

func New(url string, now func() time.Time) *Notifier {
	if now == nil {
		now = time.Now
	}
	return &Notifier{
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
		return
	}
	resp.Body.Close()
}
