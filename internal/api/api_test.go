package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

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
