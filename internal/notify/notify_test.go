package notify

import (
	"bytes"
	"io"
	"log/slog"
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

	n := New(srv.URL, nil, nil)
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
	n := New("http://127.0.0.1:1/never", nil, nil)
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

	n := New("", nil, nil)
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
	n := New(srv.URL, func() time.Time { return now }, nil)
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
	n := New(srv.URL, func() time.Time { return now }, nil)

	n.SendKeyed("quarantine:a.md", "t", "m", true)
	n.SendKeyed("quarantine:b.md", "t", "m", true)
	time.Sleep(200 * time.Millisecond)

	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("sent %d notifications, want 2", got)
	}
}

// A notifier that has never delivered anything is almost always misconfigured.
// The first failure must say so out loud: the alternative is what shipped
// first -- a scratch image with no CA certificates, every post failing, and
// nothing anywhere saying so.
func TestFirstFailureIsLoggedAsAnError(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	n := New("http://127.0.0.1:1/never", nil, log)
	n.Send("t", "m", true)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if bytes.Contains(buf.Bytes(), []byte("going nowhere")) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no error logged for a failing notifier; got: %s", buf.String())
}

// A non-2xx response is a delivery failure too. A wrong topic or a rejected
// request answers with a status, not a transport error.
func TestHTTPErrorStatusIsAFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	n := New(srv.URL, nil, log)
	n.Send("t", "m", true)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if bytes.Contains(buf.Bytes(), []byte("403")) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("a 403 was not reported as a failure; got: %s", buf.String())
}

// Verify is the one synchronous path: it must return the error rather than
// swallow it, because it is the only chance a misconfiguration has to surface
// on its own.
func TestVerifyReturnsDeliveryErrors(t *testing.T) {
	n := New("http://127.0.0.1:1/never", nil, nil)
	if err := n.Verify("t", "m"); err == nil {
		t.Fatal("Verify returned nil for an unreachable endpoint")
	}
}

func TestVerifySucceedsAndSendsAtMinPriority(t *testing.T) {
	var gotPriority string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPriority = r.Header.Get("Priority")
	}))
	defer srv.Close()

	n := New(srv.URL, nil, nil)
	if err := n.Verify("archivist: started", "guards armed"); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	// Minimum priority: the startup check proves the path works, it is not
	// meant to buzz a phone on every restart.
	if gotPriority != "min" {
		t.Errorf("Priority = %q, want min", gotPriority)
	}
}

// A disabled notifier has nothing to verify and must not report a problem.
func TestVerifyOnDisabledNotifierIsNil(t *testing.T) {
	if err := New("", nil, nil).Verify("t", "m"); err != nil {
		t.Fatalf("Verify on a disabled notifier: %v", err)
	}
	var nilN *Notifier
	if err := nilN.Verify("t", "m"); err != nil {
		t.Fatalf("Verify on a nil notifier: %v", err)
	}
}
