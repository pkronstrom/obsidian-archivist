package api

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pkronstrom/vaultsync/internal/reconcile"
	"github.com/pkronstrom/vaultsync/internal/repo"
	"github.com/pkronstrom/vaultsync/internal/vault"
)

const token = "test-token"

func newServer(t *testing.T) (http.Handler, *vault.Vault, *repo.Repo) {
	t.Helper()
	base := t.TempDir()
	work := filepath.Join(base, "vault")
	v, err := vault.New(work)
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Open(work, filepath.Join(base, "git"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { v.Close() })
	return New(reconcile.New(v, r), r, token), v, r
}

func do(t *testing.T, h http.Handler, method, path string, body any, auth bool) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	if auth {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestUnauthenticatedIsRejected(t *testing.T) {
	h, _, _ := newServer(t)
	for _, path := range []string{"/v1/head", "/v1/snapshot", "/v1/changes"} {
		if got := do(t, h, "GET", path, nil, false).Code; got != http.StatusUnauthorized {
			t.Errorf("GET %s unauthenticated = %d, want 401", path, got)
		}
	}
}

func TestWrongTokenIsRejected(t *testing.T) {
	h, _, _ := newServer(t)
	req := httptest.NewRequest("GET", "/v1/head", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("wrong token = %d, want 401", w.Code)
	}
}

func TestHeadOnEmptyRepo(t *testing.T) {
	h, _, _ := newServer(t)
	w := do(t, h, "GET", "/v1/head", nil, true)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d: %s", w.Code, w.Body)
	}
	var got struct {
		Head string `json:"head"`
	}
	json.Unmarshal(w.Body.Bytes(), &got)
	if got.Head != "" {
		t.Errorf("head = %q, want empty", got.Head)
	}
}

// A content-addressed store that does not verify its own addresses is not
// content-addressed. A client that miscomputes a hash must find out at once.
func TestContentUploadRejectsAHashMismatch(t *testing.T) {
	h, _, _ := newServer(t)
	req := httptest.NewRequest("PUT", "/v1/content/"+strings.Repeat("a", 40),
		bytes.NewReader([]byte("these bytes hash to something else")))
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400: %s", w.Code, w.Body)
	}
}

func TestContentRoundTrip(t *testing.T) {
	h, _, r := newServer(t)
	content := []byte("# a note\n")
	hash, err := repo.HashContent(content)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("PUT", "/v1/content/"+hash, bytes.NewReader(content))
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", w.Code, w.Body)
	}
	if !r.HasBlob(hash) {
		t.Fatal("blob absent after upload")
	}

	w = do(t, h, "GET", "/v1/content/"+hash, nil, true)
	if w.Code != http.StatusOK {
		t.Fatalf("GET = %d", w.Code)
	}
	if !bytes.Equal(w.Body.Bytes(), content) {
		t.Errorf("GET body = %q, want %q", w.Body, content)
	}
}

func TestContentGetMissingIs404(t *testing.T) {
	h, _, _ := newServer(t)
	w := do(t, h, "GET", "/v1/content/"+strings.Repeat("b", 40), nil, true)
	if w.Code != http.StatusNotFound {
		t.Errorf("code = %d, want 404", w.Code)
	}
}

func TestHaveReportsOnlyMissingHashes(t *testing.T) {
	h, _, r := newServer(t)
	present, _ := r.WriteBlob([]byte("already here\n"))
	absent := strings.Repeat("c", 40)

	w := do(t, h, "POST", "/v1/have", map[string][]string{"hashes": {present, absent}}, true)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d: %s", w.Code, w.Body)
	}
	var got struct {
		Missing []string `json:"missing"`
	}
	json.Unmarshal(w.Body.Bytes(), &got)
	if len(got.Missing) != 1 || got.Missing[0] != absent {
		t.Errorf("missing = %v, want just %s", got.Missing, absent)
	}
}

// An unknown cursor is the client's problem to recover from, not a server
// fault. It must be 409 so the client knows to re-bootstrap from /snapshot.
func TestChangesFromUnknownCursorIs409(t *testing.T) {
	h, _, _ := newServer(t)
	w := do(t, h, "GET", "/v1/changes?since="+strings.Repeat("d", 40), nil, true)
	if w.Code != http.StatusConflict {
		t.Errorf("code = %d, want 409: %s", w.Code, w.Body)
	}
}

func TestPushSnapshotAndChangesFlow(t *testing.T) {
	h, _, _ := newServer(t)
	content := []byte("hello\n")
	hash, _ := repo.HashContent(content)

	req := httptest.NewRequest("PUT", "/v1/content/"+hash, bytes.NewReader(content))
	req.Header.Set("Authorization", "Bearer "+token)
	h.ServeHTTP(httptest.NewRecorder(), req)

	w := do(t, h, "POST", "/v1/push", pushRequest{
		Base:   "",
		Device: "mac",
		Changes: []reconcile.Change{
			{Path: "notes/a.md", Op: "put", Hash: hash},
		},
	}, true)
	if w.Code != http.StatusOK {
		t.Fatalf("push = %d: %s", w.Code, w.Body)
	}
	var pr pushResponse
	json.Unmarshal(w.Body.Bytes(), &pr)
	if pr.Head == "" {
		t.Fatal("push returned no head")
	}
	if len(pr.Results) != 1 || pr.Results[0].Status != reconcile.StatusApplied {
		t.Fatalf("results = %+v", pr.Results)
	}

	w = do(t, h, "GET", "/v1/snapshot", nil, true)
	var snap snapshotResponse
	json.Unmarshal(w.Body.Bytes(), &snap)
	if _, ok := snap.Files["notes/a.md"]; !ok {
		t.Errorf("snapshot = %+v, want notes/a.md", snap.Files)
	}
	if snap.Head != pr.Head {
		t.Errorf("snapshot head = %q, want %q", snap.Head, pr.Head)
	}

	w = do(t, h, "GET", "/v1/changes?since=", nil, true)
	var ch changesResponse
	json.Unmarshal(w.Body.Bytes(), &ch)
	if len(ch.Entries) != 1 || ch.Entries[0].Path != "notes/a.md" {
		t.Errorf("changes = %+v", ch.Entries)
	}
}

func TestPushWithBadPathIs400NotPanic(t *testing.T) {
	h, _, _ := newServer(t)
	w := do(t, h, "POST", "/v1/push", pushRequest{
		Device:  "mac",
		Changes: []reconcile.Change{{Path: "../escape.md", Op: "put", Hash: "x"}},
	}, true)
	if w.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400: %s", w.Code, w.Body)
	}
}

func TestMethodMismatchIs405(t *testing.T) {
	h, _, _ := newServer(t)
	w := do(t, h, "POST", "/v1/head", nil, true)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("code = %d, want 405", w.Code)
	}
}

// The stream must carry the CURRENT head on connect, then each new one. A
// consumer that just subscribed should not have to wait for the next commit to
// learn where things stand.
func TestEventsStreamsHeadOnConnectAndOnCommit(t *testing.T) {
	h, _, r := newServer(t)
	content := []byte("first\n")
	hash, _ := repo.HashContent(content)
	putContent(t, h, hash, content)
	first := pushOne(t, h, "", "notes/a.md", hash)

	srv := httptest.NewServer(h)
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}

	rd := bufio.NewReader(resp.Body)
	line, err := rd.ReadString('\n')
	if err != nil {
		t.Fatalf("reading the connect event: %v", err)
	}
	if !strings.Contains(line, first) {
		t.Errorf("connect event = %q, want the current head %s", line, first)
	}

	// A commit landing while subscribed must arrive.
	go func() {
		time.Sleep(100 * time.Millisecond)
		c2 := []byte("second\n")
		h2, _ := repo.HashContent(c2)
		putContent(t, h, h2, c2)
		pushOne(t, h, first, "notes/b.md", h2)
	}()

	// Parse properly rather than substring-matching: events now carry `prev`
	// as well as `head`, so a naive "does not contain the old hash" filter
	// skips the very event it is waiting for.
	type sseEvent struct {
		Head    string `json:"head"`
		Prev    string `json:"prev"`
		Count   int    `json:"count"`
		Changes []struct {
			Path string `json:"path"`
			Op   string `json:"op"`
			Ext  string `json:"ext"`
			Kind string `json:"kind"`
			Size int64  `json:"size"`
		} `json:"changes"`
	}

	done := make(chan sseEvent, 1)
	go func() {
		for {
			l, err := rd.ReadString('\n')
			if err != nil {
				return
			}
			if !strings.HasPrefix(l, "data:") {
				continue
			}
			var ev sseEvent
			if json.Unmarshal([]byte(strings.TrimPrefix(l, "data: ")), &ev) != nil {
				continue
			}
			if ev.Head != first {
				done <- ev
				return
			}
		}
	}()

	select {
	case ev := <-done:
		head, _ := r.Head()
		if ev.Head != head {
			t.Errorf("event head = %s, want %s", ev.Head, head)
		}
		if ev.Prev != first {
			t.Errorf("event prev = %s, want %s", ev.Prev, first)
		}
		// The point of the payload: enough to triage without another request.
		if ev.Count != 1 || len(ev.Changes) != 1 {
			t.Fatalf("changes = %+v, want exactly one", ev.Changes)
		}
		c := ev.Changes[0]
		if c.Path != "notes/b.md" || c.Op != "put" || c.Ext != "md" || c.Kind != "text" {
			t.Errorf("change = %+v, want notes/b.md put md text", c)
		}
		if c.Size == 0 {
			t.Error("size missing from the event")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no event for a commit made while subscribed")
	}
}

// An agent should be able to tell an attachment from a note without fetching it.
func TestEventsDistinguishBinaryFromText(t *testing.T) {
	h, _, _ := newServer(t)
	srv := httptest.NewServer(h)
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	rd := bufio.NewReader(resp.Body)

	go func() {
		time.Sleep(100 * time.Millisecond)
		blob := []byte("\x00\x01\x02 a pretend PDF")
		bh, _ := repo.HashContent(blob)
		putContent(t, h, bh, blob)
		pushOne(t, h, "", "att/scan.pdf", bh)
	}()

	deadline := time.After(3 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("no event")
		default:
		}
		l, err := rd.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(l, "data:") {
			continue
		}
		var ev struct {
			Changes []struct {
				Path, Op, Ext, Kind string
			} `json:"changes"`
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(l, "data: ")), &ev) != nil || len(ev.Changes) == 0 {
			continue
		}
		c := ev.Changes[0]
		if c.Ext != "pdf" {
			t.Errorf("ext = %q, want pdf", c.Ext)
		}
		if c.Kind != "binary" {
			t.Errorf("kind = %q, want binary -- sniffed from content, not the extension", c.Kind)
		}
		return
	}
}

func putContent(t *testing.T, h http.Handler, hash string, content []byte) {
	t.Helper()
	req := httptest.NewRequest("PUT", "/v1/content/"+hash, bytes.NewReader(content))
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("upload: %d %s", w.Code, w.Body)
	}
}

func pushOne(t *testing.T, h http.Handler, base, path, hash string) string {
	t.Helper()
	w := do(t, h, "POST", "/v1/push", pushRequest{
		Base: base, Device: "test",
		Changes: []reconcile.Change{{Path: path, Op: "put", Hash: hash}},
	}, true)
	if w.Code != http.StatusOK {
		t.Fatalf("push: %d %s", w.Code, w.Body)
	}
	var pr pushResponse
	json.Unmarshal(w.Body.Bytes(), &pr)
	return pr.Head
}

// The route table builds the mux, so this test guards the property that makes
// the index trustworthy: everything advertised actually answers.
func TestIndexListsRoutesThatAllExist(t *testing.T) {
	h, _, _ := newServer(t)
	w := do(t, h, "GET", "/v1", nil, true)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d", w.Code)
	}
	var idx struct {
		Endpoints []struct{ Method, Path, Does string } `json:"endpoints"`
	}
	json.Unmarshal(w.Body.Bytes(), &idx)
	if len(idx.Endpoints) < 10 {
		t.Fatalf("only %d endpoints listed", len(idx.Endpoints))
	}
	for _, e := range idx.Endpoints {
		if e.Does == "" {
			t.Errorf("%s %s has no description", e.Method, e.Path)
		}
		if strings.Contains(e.Path, "{") || e.Method != "GET" {
			continue // needs parameters or a body; existence is covered elsewhere
		}
		if e.Path == "/v1/events" {
			// An SSE stream never returns; calling it here hangs the test.
			// Covered by TestEventsStreamsHeadOnConnectAndOnCommit instead.
			continue
		}
		got := do(t, h, e.Method, e.Path, nil, true).Code
		if got == http.StatusNotFound || got == http.StatusMethodNotAllowed {
			t.Errorf("%s %s is advertised but answers %d", e.Method, e.Path, got)
		}
	}
}
