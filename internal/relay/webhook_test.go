package relay_test

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/pkronstrom/obsidian-archivist/internal/auth"
	"github.com/pkronstrom/obsidian-archivist/internal/vaults"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/pkronstrom/obsidian-archivist/internal/api"
	"github.com/pkronstrom/obsidian-archivist/internal/client"
	"github.com/pkronstrom/obsidian-archivist/internal/relay"
	"github.com/pkronstrom/obsidian-archivist/protocol"
)

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func liveClient(t *testing.T) *client.Client {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "vaults", "personal"), 0o755); err != nil {
		t.Fatal(err)
	}
	reg, err := vaults.NewRegistry(vaults.Layout{Root: root}, vaults.Options{
		MaxVaults: 5,
		Log:       slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	set := auth.NewSetForTest(map[string]auth.Principal{
		"tok": {
			Label: "test", Vaults: []string{"*"},
			Scopes: []string{auth.ScopeRead, auth.ScopeWrite, auth.ScopeDelete},
		},
	})
	srv := httptest.NewServer(api.New(reg, set))
	t.Cleanup(srv.Close)
	return client.New(srv.URL, "tok", "relay").WithVault("personal")
}

// livePool is liveClient's pass-through sibling: a Pool and health probe aimed
// at the same server. Ping strips the helper client's token, so health remains
// unauthenticated. Callers present relayTok, which the Pool forwards.
func livePool(t *testing.T) (*relay.Pool, *client.Client) {
	t.Helper()
	probe := liveClient(t)
	return relay.NewPool(probe.BaseURL(), "relay"), probe
}

// A collector stands in for n8n or Node-RED.
type collector struct {
	mu     sync.Mutex
	events []protocol.Event
	srv    *httptest.Server
}

func newCollector(t *testing.T, status int, delay time.Duration) *collector {
	t.Helper()
	c := &collector{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if delay > 0 {
			time.Sleep(delay)
		}
		var ev protocol.Event
		json.NewDecoder(r.Body).Decode(&ev)
		c.mu.Lock()
		c.events = append(c.events, ev)
		c.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *collector) got() []protocol.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]protocol.Event, len(c.events))
	copy(out, c.events)
	return out
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func TestEventIsForwardedToEveryTarget(t *testing.T) {
	c := liveClient(t)
	a := newCollector(t, 200, 0)
	b := newCollector(t, 200, 0)

	w := relay.NewWebhooks(c, []string{a.srv.URL, b.srv.URL}, quiet())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)
	time.Sleep(300 * time.Millisecond) // let the stream connect

	if _, err := c.Write(context.Background(), "notes/a.md", []byte("hello\n")); err != nil {
		t.Fatal(err)
	}

	if !waitFor(t, 5*time.Second, func() bool { return len(a.got()) > 0 && len(b.got()) > 0 }) {
		t.Fatalf("not delivered to both: a=%d b=%d", len(a.got()), len(b.got()))
	}
	ev := a.got()[0]
	if len(ev.Changes) == 0 || ev.Changes[0].Path != "notes/a.md" {
		t.Errorf("event body wrong: %+v", ev)
	}
	// The triage metadata is the point of the payload.
	if ev.Changes[0].Ext != "md" || ev.Changes[0].Kind != protocol.KindText {
		t.Errorf("event lacks triage metadata: %+v", ev.Changes[0])
	}
}

// One broken target must not deprive the others.
func TestAFailingTargetDoesNotBlockTheOthers(t *testing.T) {
	c := liveClient(t)
	good := newCollector(t, 200, 0)
	bad := newCollector(t, 500, 0)

	w := relay.NewWebhooks(c, []string{bad.srv.URL, good.srv.URL}, quiet())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)
	time.Sleep(300 * time.Millisecond)

	c.Write(context.Background(), "notes/a.md", []byte("hello\n"))

	if !waitFor(t, 5*time.Second, func() bool { return len(good.got()) > 0 }) {
		t.Fatal("the healthy target was starved by the failing one")
	}
	if !waitFor(t, 2*time.Second, func() bool {
		_, failed, _ := w.Stats()
		return failed > 0
	}) {
		t.Error("a rejected delivery was not counted as a failure")
	}
}

// Delivery is best-effort: a target that never answers is dropped, and the
// stream keeps running. Nothing queues up behind it.
func TestASlowTargetIsDroppedAndTheStreamSurvives(t *testing.T) {
	c := liveClient(t)
	slow := newCollector(t, 200, 8*time.Second) // longer than the 5s deadline
	fast := newCollector(t, 200, 0)

	w := relay.NewWebhooks(c, []string{slow.srv.URL, fast.srv.URL}, quiet())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)
	time.Sleep(300 * time.Millisecond)

	c.Write(context.Background(), "notes/one.md", []byte("one\n"))
	if !waitFor(t, 5*time.Second, func() bool { return len(fast.got()) >= 1 }) {
		t.Fatal("first event never reached the fast target")
	}

	// A second commit must still be delivered, i.e. the slow one did not wedge it.
	c.Write(context.Background(), "notes/two.md", []byte("two\n"))
	if !waitFor(t, 15*time.Second, func() bool { return len(fast.got()) >= 2 }) {
		t.Errorf("the stream stalled behind a slow target: fast got %d", len(fast.got()))
	}
}

func TestNoTargetsIsANoOp(t *testing.T) {
	c := liveClient(t)
	w := relay.NewWebhooks(c, nil, quiet())
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := w.Run(ctx); err != nil {
		t.Errorf("Run with no targets: %v", err)
	}
}

func TestRunStopsOnContextCancel(t *testing.T) {
	c := liveClient(t)
	target := newCollector(t, 200, 0)
	w := relay.NewWebhooks(c, []string{target.srv.URL}, quiet())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	time.Sleep(300 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Error("Run did not stop on cancellation")
	}
}

// The isolation claim, tested rather than asserted in a comment: a slow target
// must not delay a healthy one, and must not cost it events.
//
// The old fan-out did a wg.Wait() inline, so every event paid the slowest
// target's latency before the next one could even be read.
func TestASlowTargetDoesNotStallTheOthers(t *testing.T) {
	c := liveClient(t)

	slow := newCollector(t, 200, 2*time.Second)
	good := newCollector(t, 200, 0)

	w := relay.NewWebhooks(c, []string{slow.srv.URL, good.srv.URL}, quiet())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go w.Run(ctx)
	time.Sleep(300 * time.Millisecond)

	const events = 5
	start := time.Now()
	for i := 0; i < events; i++ {
		if _, err := c.Write(context.Background(),
			fmt.Sprintf("notes/n%d.md", i), []byte("x\n")); err != nil {
			t.Fatal(err)
		}
	}
	// Serialised behind a 2s target this needs >10s; isolated it is immediate.
	if !waitFor(t, 6*time.Second, func() bool { return len(good.got()) >= events }) {
		t.Fatalf("healthy target got %d/%d events in %v while another target was slow",
			len(good.got()), events, time.Since(start))
	}
}
