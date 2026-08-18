package notify

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestSendPostsTitleBodyAndPriority(t *testing.T) {
	var gotTitle, gotBody, gotPriority string
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTitle = r.Header.Get("Title")
		gotPriority = r.Header.Get("Priority")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		close(done)
	}))
	defer srv.Close()

	n := New(srv.URL, nil)
	n.Send("archivist: quarantine", "a.md is quarantined", true)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("no request arrived")
	}
	if gotTitle != "archivist: quarantine" {
		t.Errorf("Title = %q", gotTitle)
	}
	if gotBody != "a.md is quarantined" {
		t.Errorf("body = %q", gotBody)
	}
	if gotPriority != "high" {
		t.Errorf("Priority = %q, want high for an urgent alert", gotPriority)
	}
}

// A dead endpoint must never block or fail the caller. This is the property
// that matters most: a broken notifier that can fail a sync is worse than no
// notifier at all.
func TestSendToDeadEndpointReturnsImmediately(t *testing.T) {
	n := New("http://127.0.0.1:1/never", nil)
	start := time.Now()
	n.Send("t", "m", true)
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("Send blocked for %v", elapsed)
	}
}

func TestEmptyURLDisablesSending(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	defer srv.Close()

	n := New("", nil)
	n.Send("t", "m", true)
	time.Sleep(100 * time.Millisecond)
	if atomic.LoadInt32(&hits) != 0 {
		t.Fatal("a disabled notifier sent a request")
	}
}

// A nil Notifier must be usable, so a caller never needs a nil check.
func TestNilNotifierIsSafe(t *testing.T) {
	var n *Notifier
	n.Send("t", "m", true)
	n.SendKeyed("k", "t", "m", true)
}

// A loop tripping quarantine repeatedly must not produce one push per attempt.
func TestRepeatedKeysAreSuppressed(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	defer srv.Close()

	now := time.Unix(1000, 0)
	n := New(srv.URL, func() time.Time { return now })
	n.Cooldown = 10 * time.Minute

	for i := 0; i < 5; i++ {
		n.SendKeyed("quarantine:a.md", "t", "m", true)
	}
	time.Sleep(200 * time.Millisecond)
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("sent %d notifications, want 1", got)
	}

	now = now.Add(11 * time.Minute)
	n.SendKeyed("quarantine:a.md", "t", "m", true)
	time.Sleep(200 * time.Millisecond)
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("sent %d after the cooldown, want 2", got)
	}
}

// Distinct keys are independent: a quarantine on one path must not silence
// an alert about another.
func TestDistinctKeysAreNotSuppressed(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	defer srv.Close()

	now := time.Unix(1000, 0)
	n := New(srv.URL, func() time.Time { return now })

	n.SendKeyed("quarantine:a.md", "t", "m", true)
	n.SendKeyed("quarantine:b.md", "t", "m", true)
	time.Sleep(200 * time.Millisecond)

	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("sent %d notifications, want 2", got)
	}
}
