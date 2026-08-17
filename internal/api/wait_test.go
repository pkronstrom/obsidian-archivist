package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pkronstrom/obsidian-archivist/internal/repo"
	"github.com/pkronstrom/obsidian-archivist/protocol"
)

const zeroCursor = "0000000000000000000000000000000000000000"

// waitFor calls the long-poll endpoint and reports both the answer and how long
// it blocked, because the timing IS the behaviour under test.
func waitFor(t *testing.T, url, since, timeout string) (protocol.WaitResponse, time.Duration) {
	t.Helper()
	u := url + "/v1/wait?since=" + since
	if timeout != "" {
		u += "&timeout=" + timeout
	}
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out protocol.WaitResponse
	json.NewDecoder(resp.Body).Decode(&out)
	return out, time.Since(start)
}

// commit writes one file through the API and returns the new head.
func commit(t *testing.T, h http.Handler, base, path, body string) string {
	t.Helper()
	hash, _ := repo.HashContent([]byte(body))
	putContent(t, h, hash, []byte(body))
	return pushOne(t, h, base, path, hash)
}

// A caller whose cursor is already stale gets an answer at once, rather than
// blocking for news it has in fact already missed.
func TestWaitReturnsAtOnceWhenAlreadyBehind(t *testing.T) {
	h, _, _ := newServer(t)
	commit(t, h, "", "a.md", "one\n")
	srv := httptest.NewServer(h)
	defer srv.Close()

	out, took := waitFor(t, srv.URL, zeroCursor, "30")
	if !out.Changed {
		t.Error("a stale cursor was not told it is behind")
	}
	if took > 3*time.Second {
		t.Errorf("blocked for %v; it should have answered immediately", took)
	}
}

// The point of the endpoint: block, then wake as soon as a commit lands.
func TestWaitWakesOnACommit(t *testing.T) {
	h, _, _ := newServer(t)
	head := commit(t, h, "", "a.md", "one\n")
	srv := httptest.NewServer(h)
	defer srv.Close()

	go func() {
		time.Sleep(400 * time.Millisecond)
		commit(t, h, head, "b.md", "two\n")
	}()

	out, took := waitFor(t, srv.URL, head, "30")
	if !out.Changed {
		t.Fatalf("did not report the change: %+v after %v", out, took)
	}
	if out.Head == head {
		t.Error("head did not move")
	}
	if took > 10*time.Second {
		t.Errorf("took %v; it should have woken on the commit, not timed out", took)
	}
}

// A timeout means "still nothing". It is a normal answer, not an error.
func TestWaitTimesOutQuietly(t *testing.T) {
	h, _, _ := newServer(t)
	head := commit(t, h, "", "a.md", "one\n")
	srv := httptest.NewServer(h)
	defer srv.Close()

	out, took := waitFor(t, srv.URL, head, "1")
	if out.Changed {
		t.Error("reported a change when nothing had happened")
	}
	if out.Head != head {
		t.Errorf("head = %q, want the unchanged %q", out.Head, head)
	}
	if took < 500*time.Millisecond {
		t.Errorf("returned after %v; it should have held for about a second", took)
	}
}

// A commit that lands just before the call is reported at once.
//
// NOT covered here: the handler also re-checks head AFTER subscribing, to close
// the window where a commit arrives between the first check and the
// subscription. This test does not reach that window -- it still passes with
// the re-check deleted, verified by deleting it. Hitting a few instructions
// inside the handler from an HTTP client is not something a test can do
// reliably, so the re-check stands on reasoning rather than coverage. Said
// plainly here so nobody later mistakes this test for proof of it.
func TestWaitReportsACommitThatLandedJustBefore(t *testing.T) {
	for i := 0; i < 8; i++ {
		h, _, _ := newServer(t)
		head := commit(t, h, "", "a.md", "one\n")
		srv := httptest.NewServer(h)

		go commit(t, h, head, "b.md", "two\n") // no sleep: race it on purpose

		out, took := waitFor(t, srv.URL, head, "5")
		srv.Close()
		if !out.Changed {
			t.Fatalf("round %d: missed a commit that raced the subscribe (waited %v)", i, took)
		}
	}
}

// A caller that walks away must not wedge the handler.
func TestWaitReleasesWhenTheClientGivesUp(t *testing.T) {
	h, _, _ := newServer(t)
	head := commit(t, h, "", "a.md", "one\n")
	srv := httptest.NewServer(h)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/v1/wait?since="+head+"&timeout=60", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	if _, err := http.DefaultClient.Do(req); err == nil {
		t.Error("expected the cancelled request to fail")
	}

	// And the server still answers afterwards.
	if out, _ := waitFor(t, srv.URL, zeroCursor, "5"); !out.Changed {
		t.Error("server stopped answering after a client walked away")
	}
}

func TestWaitRejectsABadTimeout(t *testing.T) {
	h, _, _ := newServer(t)
	w := do(t, h, "GET", "/v1/wait?since=x&timeout=nonsense", nil, true)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// Long-polling is only safe to advertise if it needs auth like everything else.
func TestWaitRequiresAuth(t *testing.T) {
	h, _, _ := newServer(t)
	w := do(t, h, "GET", "/v1/wait?since=x", nil, false)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}
