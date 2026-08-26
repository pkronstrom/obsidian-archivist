package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pkronstrom/obsidian-archivist/internal/auth"
	"github.com/pkronstrom/obsidian-archivist/internal/vault"
	"github.com/pkronstrom/obsidian-archivist/protocol"
)

func TestNoteMutationRoutesApplyAndReturnBothRevisions(t *testing.T) {
	h, v, r := newServer(t)
	before := []byte("before\ntarget\n")
	seedHead, seedRevision := seedAPINote(t, h, "Triage.md", before)

	wantAfterAppend := []byte("before\ntarget\nafter\n")
	w := do(t, h, http.MethodPost, "/v1/note/append", protocol.AppendNoteRequest{
		Path:            "Triage.md",
		Content:         "after\n",
		ContentRevision: seedRevision,
	}, true)
	appended := requireMutationSuccess(t, w)
	requireMutationResult(t, appended, "Triage.md", seedHead, protocol.HashContent(wantAfterAppend))
	requireVaultBytes(t, v, "Triage.md", wantAfterAppend)
	head, err := r.Head()
	if err != nil {
		t.Fatal(err)
	}
	if head != appended.Revision {
		t.Errorf("repo head = %q, append revision = %q", head, appended.Revision)
	}

	wantAfterEdit := []byte("before\nchanged\nafter\n")
	w = do(t, h, http.MethodPost, "/v1/note/edit", protocol.EditNoteRequest{
		Path:            "Triage.md",
		ContentRevision: appended.ContentRevision,
		OldText:         "target",
		NewText:         "changed",
	}, true)
	edited := requireMutationSuccess(t, w)
	requireMutationResult(t, edited, "Triage.md", appended.Revision, protocol.HashContent(wantAfterEdit))
	requireVaultBytes(t, v, "Triage.md", wantAfterEdit)
	head, err = r.Head()
	if err != nil {
		t.Fatal(err)
	}
	if head != edited.Revision {
		t.Errorf("repo head = %q, edit revision = %q", head, edited.Revision)
	}
}

func TestNoteMutationRoutesRequireWriteScopeWithoutChangingBytes(t *testing.T) {
	const readToken = "read-only-token"
	reg, _, _ := singleVaultRegistry(t, readToken)
	inst, err := reg.Get("personal")
	if err != nil {
		t.Fatal(err)
	}
	before := []byte("before\ntarget\n")
	if err := inst.Vault.Write("Triage.md", before); err != nil {
		t.Fatal(err)
	}
	set := auth.NewSetForTest(map[string]auth.Principal{
		readToken: {
			Label: "reader", Vaults: []string{"*"}, Scopes: []string{auth.ScopeRead},
		},
	})
	h := New(reg, set)

	requests := []struct {
		name string
		path string
		body any
	}{
		{
			name: "append",
			path: "/v1/note/append",
			body: protocol.AppendNoteRequest{Path: "Triage.md", Content: "after\n"},
		},
		{
			name: "edit",
			path: "/v1/note/edit",
			body: protocol.EditNoteRequest{
				Path: "Triage.md", ContentRevision: protocol.HashContent(before),
				OldText: "target", NewText: "changed",
			},
		},
	}
	for _, tt := range requests {
		t.Run(tt.name, func(t *testing.T) {
			w := doAs(t, h, http.MethodPost, tt.path, tt.body, readToken)
			requireMutationError(t, w, http.StatusForbidden, protocol.CodeForbidden)
			requireVaultBytes(t, inst.Vault, "Triage.md", before)
		})
	}
}

func TestEditNoteRouteReturnsStaleWithoutChangingBytes(t *testing.T) {
	h, v, _ := newServer(t)
	before := []byte("before\ntarget\nafter\n")
	_, _ = seedAPINote(t, h, "Triage.md", before)

	w := do(t, h, http.MethodPost, "/v1/note/edit", protocol.EditNoteRequest{
		Path:            "Triage.md",
		ContentRevision: "0000000000000000000000000000000000000000",
		OldText:         "target",
		NewText:         "changed",
	}, true)
	requireMutationError(t, w, http.StatusConflict, protocol.CodeStale)
	requireVaultBytes(t, v, "Triage.md", before)
}

func TestNoteMutationRoutesReturnNotFoundForMissingOrMovedTargets(t *testing.T) {
	t.Run("missing append target", func(t *testing.T) {
		h, v, _ := newServer(t)
		w := do(t, h, http.MethodPost, "/v1/note/append", protocol.AppendNoteRequest{
			Path: "missing.md", Content: "new\n",
		}, true)
		requireMutationError(t, w, http.StatusNotFound, protocol.CodeNotFound)
		if _, err := v.Read("missing.md"); err == nil {
			t.Fatal("missing.md was created by a refused append")
		}
	})

	t.Run("moved edit target", func(t *testing.T) {
		h, v, _ := newServer(t)
		before := []byte("before\ntarget\nafter\n")
		head, contentRevision := seedAPINote(t, h, "old.md", before)
		moved := do(t, h, http.MethodPost, "/v1/push", protocol.PushRequest{
			Base: head, Device: "test",
			Changes: []protocol.Change{{Path: "new.md", Op: protocol.OpMove, From: "old.md"}},
		}, true)
		if moved.Code != http.StatusOK {
			t.Fatalf("move: status = %d, body = %s", moved.Code, moved.Body.String())
		}

		w := do(t, h, http.MethodPost, "/v1/note/edit", protocol.EditNoteRequest{
			Path: "old.md", ContentRevision: contentRevision,
			OldText: "target", NewText: "changed",
		}, true)
		requireMutationError(t, w, http.StatusNotFound, protocol.CodeNotFound)
		if _, err := v.Read("old.md"); err == nil {
			t.Fatal("old.md was recreated by a refused edit")
		}
		requireVaultBytes(t, v, "new.md", before)
	})
}

func TestEditNoteRouteRefusesAbsentAndDuplicateTargetsWithoutChangingBytes(t *testing.T) {
	tests := []struct {
		name string
		body []byte
		old  string
		code string
	}{
		{name: "absent", body: []byte("before\ntarget\nafter\n"), old: "missing", code: protocol.CodeNoMatch},
		{name: "duplicate", body: []byte("target\nbetween\ntarget\n"), old: "target", code: protocol.CodeMultipleMatches},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, v, _ := newServer(t)
			_, contentRevision := seedAPINote(t, h, "Triage.md", tt.body)
			w := do(t, h, http.MethodPost, "/v1/note/edit", protocol.EditNoteRequest{
				Path: "Triage.md", ContentRevision: contentRevision,
				OldText: tt.old, NewText: "changed",
			}, true)
			requireMutationError(t, w, http.StatusConflict, tt.code)
			requireVaultBytes(t, v, "Triage.md", tt.body)
		})
	}
}

func TestNoteMutationRoutesMapMalformedAndNonTextToBadRequest(t *testing.T) {
	tests := []struct {
		name string
		body []byte
		path string
		req  func(contentRevision string) any
		code string
	}{
		{
			name: "malformed append", body: []byte("before\n"), path: "/v1/note/append",
			req: func(contentRevision string) any {
				return protocol.AppendNoteRequest{Path: "Triage.md", ContentRevision: contentRevision}
			},
			code: protocol.CodeMalformed,
		},
		{
			name: "malformed edit", body: []byte("before\ntarget\n"), path: "/v1/note/edit",
			req: func(contentRevision string) any {
				return protocol.EditNoteRequest{
					Path: "Triage.md", ContentRevision: contentRevision, NewText: "changed",
				}
			},
			code: protocol.CodeMalformed,
		},
		{
			name: "non-text current note", body: []byte("before\x00after"), path: "/v1/note/append",
			req: func(contentRevision string) any {
				return protocol.AppendNoteRequest{
					Path: "Triage.md", Content: "suffix\n", ContentRevision: contentRevision,
				}
			},
			code: protocol.CodeNotText,
		},
		{
			name: "non-text replacement", body: []byte("before\ntarget\n"), path: "/v1/note/edit",
			req: func(contentRevision string) any {
				return protocol.EditNoteRequest{
					Path: "Triage.md", ContentRevision: contentRevision,
					OldText: "target", NewText: "changed\x00",
				}
			},
			code: protocol.CodeNotText,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, v, _ := newServer(t)
			_, contentRevision := seedAPINote(t, h, "Triage.md", tt.body)
			w := do(t, h, http.MethodPost, tt.path, tt.req(contentRevision), true)
			requireMutationError(t, w, http.StatusBadRequest, tt.code)
			requireVaultBytes(t, v, "Triage.md", tt.body)
		})
	}
}

func TestNoteMutationRouteRejectsMalformedJSONWithoutChangingBytes(t *testing.T) {
	h, v, _ := newServer(t)
	before := []byte("before\n")
	_, _ = seedAPINote(t, h, "Triage.md", before)

	req := httptest.NewRequest(
		http.MethodPost,
		"/personal/v1/note/append",
		strings.NewReader(`{"path":"Triage.md","content":`),
	)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	requireMutationError(t, w, http.StatusBadRequest, protocol.CodeMalformed)
	requireVaultBytes(t, v, "Triage.md", before)
}

func TestAppendNoteRouteMapsTooLargeWithoutChangingFile(t *testing.T) {
	h, v, _ := newServer(t)
	before := []byte("start\n")
	_, _ = seedAPINote(t, h, "Triage.md", before)

	notePath := filepath.Join(v.Dir(), "Triage.md")
	f, err := os.OpenFile(notePath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte{'Z'}, int64(protocol.MaxUploadBytes)); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	w := do(t, h, http.MethodPost, "/v1/note/append", protocol.AppendNoteRequest{
		Path: "Triage.md", Content: "suffix\n",
	}, true)
	requireMutationError(t, w, http.StatusRequestEntityTooLarge, protocol.CodeTooLarge)

	info, err := os.Stat(notePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != int64(protocol.MaxUploadBytes)+1 {
		t.Errorf("size = %d, want %d", info.Size(), int64(protocol.MaxUploadBytes)+1)
	}
	f, err = os.Open(notePath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	prefix := make([]byte, len(before))
	if _, err := f.ReadAt(prefix, 0); err != nil {
		t.Fatal(err)
	}
	if string(prefix) != string(before) {
		t.Errorf("prefix = %q, want %q", prefix, before)
	}
	var marker [1]byte
	if _, err := f.ReadAt(marker[:], int64(protocol.MaxUploadBytes)); err != nil {
		t.Fatal(err)
	}
	if marker[0] != 'Z' {
		t.Errorf("last byte = %q, want Z", marker[0])
	}
}

func TestIndexAdvertisesNoteMutationRoutes(t *testing.T) {
	h, _, _ := newServer(t)
	w := do(t, h, http.MethodGet, "/v1", nil, true)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var index protocol.IndexResponse
	if err := json.Unmarshal(w.Body.Bytes(), &index); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"POST /v1/note/append": false,
		"POST /v1/note/edit":   false,
	}
	for _, endpoint := range index.Endpoints {
		key := endpoint.Method + " " + endpoint.Path
		if _, ok := want[key]; ok {
			want[key] = true
		}
	}
	for endpoint, found := range want {
		if !found {
			t.Errorf("index does not advertise %s", endpoint)
		}
	}
}

func seedAPINote(t *testing.T, h http.Handler, path string, body []byte) (head, contentRevision string) {
	t.Helper()
	contentRevision = protocol.HashContent(body)
	putContent(t, h, contentRevision, body)
	head = pushOne(t, h, "", path, contentRevision)
	return head, contentRevision
}

func requireMutationSuccess(t *testing.T, w *httptest.ResponseRecorder) protocol.NoteMutationResponse {
	t.Helper()
	if w.Code != http.StatusOK {
		var envelope protocol.ErrorResponse
		_ = json.NewDecoder(w.Body).Decode(&envelope)
		t.Fatalf("status = %d, error = %+v", w.Code, envelope.Error)
	}
	var got protocol.NoteMutationResponse
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decoding mutation response: %v", err)
	}
	return got
}

func requireMutationResult(t *testing.T, got protocol.NoteMutationResponse, path, previousRevision, contentRevision string) {
	t.Helper()
	if got.Path != path {
		t.Errorf("path = %q, want %q", got.Path, path)
	}
	if got.Status != protocol.StatusApplied {
		t.Errorf("status = %q, want %q", got.Status, protocol.StatusApplied)
	}
	if got.Revision == "" || got.Revision == previousRevision {
		t.Errorf("revision = %q, want a new revision after %q", got.Revision, previousRevision)
	}
	if got.ContentRevision != contentRevision {
		t.Errorf("content_revision = %q, want %q", got.ContentRevision, contentRevision)
	}
}

func requireMutationError(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, status, w.Body.String())
	}
	var envelope protocol.ErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&envelope); err != nil {
		t.Fatalf("decoding error response: %v", err)
	}
	if envelope.Error.Code != code {
		t.Errorf("error code = %q, want %q", envelope.Error.Code, code)
	}
}

func requireVaultBytes(t *testing.T, v *vault.Vault, path string, want []byte) {
	t.Helper()
	got, err := v.Read(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if string(got) != string(want) {
		t.Errorf("%s bytes = %q, want %q", path, got, want)
	}
}
