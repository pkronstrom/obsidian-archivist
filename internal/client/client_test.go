package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pkronstrom/obsidian-archivist/protocol"
)

const tok = "test-token"

// stub is a hand-rolled server double. Unit tests use it for error shapes that
// are awkward to provoke for real; the behaviour that matters is covered
// against a real server in client_live_test.go.
func stub(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(srv.URL, tok, "test")
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(protocol.ErrorResponse{
		Error: protocol.Error{Code: code, Message: msg},
	})
}

func TestSendsBearerToken(t *testing.T) {
	var got string
	c := stub(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		writeJSON(w, protocol.HeadResponse{Head: "abc"})
	})
	if _, err := c.Head(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got != "Bearer "+tok {
		t.Errorf("Authorization = %q", got)
	}
}

// A client must branch on the server's stable code, never on the status alone
// or on message text. Before the protocol package, every 409 was assumed to
// mean unknown-base -- true only while one thing could produce a 409.
func TestUnknownBaseIsTypedFromTheCode(t *testing.T) {
	c := stub(t, func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusConflict, protocol.CodeUnknownBase, "re-bootstrap")
	})
	_, err := c.Changes(context.Background(), "deadbeef")
	if err == nil {
		t.Fatal("want an error")
	}
	if !IsCode(err, protocol.CodeUnknownBase) {
		t.Errorf("err = %v, want IsCode(unknown_base)", err)
	}
}

// A future 409 that is NOT unknown-base must not be mistaken for one.
func TestOther409IsNotTreatedAsUnknownBase(t *testing.T) {
	c := stub(t, func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusConflict, "some_future_conflict", "something else")
	})
	_, err := c.Changes(context.Background(), "abc")
	if IsCode(err, protocol.CodeUnknownBase) {
		t.Error("a non-unknown-base 409 was read as unknown-base")
	}
	if !IsCode(err, "some_future_conflict") {
		t.Errorf("err = %v, want the server's own code preserved", err)
	}
}

func TestDistinguishesTooLargeFromHashMismatch(t *testing.T) {
	cases := []struct {
		status int
		code   string
	}{
		{http.StatusRequestEntityTooLarge, protocol.CodeTooLarge},
		{http.StatusBadRequest, protocol.CodeHashMismatch},
	}
	for _, tc := range cases {
		c := stub(t, func(w http.ResponseWriter, r *http.Request) {
			writeErr(w, tc.status, tc.code, "nope")
		})
		err := c.PutContent(context.Background(), strings.Repeat("a", 40), []byte("x"))
		if !IsCode(err, tc.code) {
			t.Errorf("PutContent with %s: err = %v", tc.code, err)
		}
	}
}

// An error body with no envelope (for example, from a proxy) must
// still produce a usable error rather than a nil or a panic.
func TestBareErrorBodyStillYieldsAnError(t *testing.T) {
	c := stub(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte("<html>gateway blew up</html>"))
	})
	_, err := c.Head(context.Background())
	if err == nil {
		t.Fatal("want an error for a 502")
	}
	if !strings.Contains(err.Error(), "502") {
		t.Errorf("err = %v, want the status mentioned", err)
	}
}

// The version field exists so a client can refuse a server it cannot speak.
// A client that ignores it makes the field decoration.
func TestRefusesAnIncompatibleProtocol(t *testing.T) {
	c := stub(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, protocol.IndexResponse{
			Service: "archivist", Version: "9.9.9", Protocol: protocol.Version + 1,
		})
	})
	err := c.CheckCompatible(context.Background())
	if err == nil {
		t.Fatal("want an error for a newer protocol")
	}
	if !strings.Contains(err.Error(), "protocol") {
		t.Errorf("err = %v, want it to name the protocol mismatch", err)
	}
}

func TestAcceptsAMatchingProtocol(t *testing.T) {
	c := stub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Errorf("compatibility path = %q, want /healthz", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("health compatibility probe sent a credential: %q", got)
		}
		writeJSON(w, protocol.HealthResponse{
			Status: "ok", Version: "0.3.0", Protocol: protocol.Version,
		})
	})
	if err := c.CheckCompatible(context.Background()); err != nil {
		t.Errorf("CheckCompatible: %v", err)
	}
}

// The build version moves every release; treating it as a compatibility signal
// would produce constant false alarms.
func TestIgnoresTheBuildVersion(t *testing.T) {
	c := stub(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, protocol.IndexResponse{
			Service: "archivist", Version: "0.0.1-something-odd", Protocol: protocol.Version,
		})
	})
	if err := c.CheckCompatible(context.Background()); err != nil {
		t.Errorf("a differing build version was treated as incompatible: %v", err)
	}
}

func TestContextCancellationIsHonoured(t *testing.T) {
	c := stub(t, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Head(ctx); err == nil {
		t.Error("want an error for a cancelled context")
	}
}

func TestVaultListingCarriesPolicyAndPosture(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"vaults":["personal","work"],"scopes":["read"],"label":"agent",
			"protectedVaults":["work"],"requiresStepUpAuth":["vault:work"]}`))
	}))
	defer srv.Close()

	got, err := New(srv.URL, "tok", "test").VaultListing(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.ProtectedVaults) != 1 || got.ProtectedVaults[0] != "work" {
		t.Errorf("ProtectedVaults = %v, want [work]", got.ProtectedVaults)
	}
	if len(got.RequiresStepUpAuth) != 1 || got.RequiresStepUpAuth[0] != "vault:work" {
		t.Errorf("RequiresStepUpAuth = %v, want [vault:work]", got.RequiresStepUpAuth)
	}
}

func TestUnlockPostsTheCodeToTheVault(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Write([]byte(`{"vault":"work","expiresAt":4102444800}`))
	}))
	defer srv.Close()

	until, err := New(srv.URL, "tok", "test").WithVault("work").
		Unlock(context.Background(), "123456")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/work/v1/unlock" {
		t.Errorf("path = %q, want /work/v1/unlock", gotPath)
	}
	if !strings.Contains(gotBody, `"123456"`) {
		t.Errorf("body = %q, does not carry the code", gotBody)
	}
	if until != 4102444800 {
		t.Errorf("expiresAt = %d", until)
	}
}

func TestAppendNotePostsTypedJSONAndDecodesBothRevisions(t *testing.T) {
	request := protocol.AppendNoteRequest{
		Path:            "Notes/Triage.md",
		Content:         "after\n",
		ContentRevision: "old-content-revision",
	}
	response := protocol.NoteMutationResponse{
		Path:            request.Path,
		Status:          protocol.StatusApplied,
		Revision:        "new-vault-revision",
		ContentRevision: "new-content-revision",
	}
	c := stub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			writeJSON(w, protocol.HealthResponse{Status: "ok", Protocol: protocol.Version})
			return
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if r.URL.Path != "/personal/v1/note/append" {
			t.Errorf("path = %q, want /personal/v1/note/append", r.URL.Path)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		gotBody, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		wantBody, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		if string(gotBody) != string(wantBody) {
			t.Errorf("body = %s, want %s", gotBody, wantBody)
		}
		writeJSON(w, response)
	}).WithVault("personal")
	c.device = ""

	got, err := c.AppendNote(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != response.Path ||
		got.Status != response.Status ||
		got.Revision != response.Revision ||
		got.ContentRevision != response.ContentRevision {
		t.Errorf("AppendNote response = %+v, want %+v", got, response)
	}
}

func TestEditNotePostsTypedJSONAndDecodesBothRevisions(t *testing.T) {
	request := protocol.EditNoteRequest{
		Path:            "Notes/Triage.md",
		ContentRevision: "old-content-revision",
		OldText:         "target",
		NewText:         "changed",
	}
	response := protocol.NoteMutationResponse{
		Path:            request.Path,
		Status:          protocol.StatusApplied,
		Revision:        "new-vault-revision",
		ContentRevision: "new-content-revision",
	}
	c := stub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			writeJSON(w, protocol.HealthResponse{Status: "ok", Protocol: protocol.Version})
			return
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if r.URL.Path != "/personal/v1/note/edit" {
			t.Errorf("path = %q, want /personal/v1/note/edit", r.URL.Path)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		gotBody, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		wantBody, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		if string(gotBody) != string(wantBody) {
			t.Errorf("body = %s, want %s", gotBody, wantBody)
		}
		writeJSON(w, response)
	}).WithVault("personal")
	c.device = ""

	got, err := c.EditNote(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != response.Path ||
		got.Status != response.Status ||
		got.Revision != response.Revision ||
		got.ContentRevision != response.ContentRevision {
		t.Errorf("EditNote response = %+v, want %+v", got, response)
	}
}

func TestEditNotePreservesStructuredStaleErrorCode(t *testing.T) {
	c := stub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			writeJSON(w, protocol.HealthResponse{Status: "ok", Protocol: protocol.Version})
			return
		}
		writeErr(w, http.StatusConflict, protocol.CodeStale, "note changed")
	}).WithVault("personal")

	_, err := c.EditNote(context.Background(), protocol.EditNoteRequest{
		Path:            "Notes/Triage.md",
		ContentRevision: "stale-content-revision",
		OldText:         "target",
		NewText:         "changed",
	})
	if !IsCode(err, protocol.CodeStale) {
		t.Errorf("err = %v, want IsCode(stale)", err)
	}
}

func TestTypedNoteMutationClientsSendConfiguredDevice(t *testing.T) {
	t.Run("append", func(t *testing.T) {
		var received protocol.AppendNoteRequest
		c := stub(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/healthz" {
				writeJSON(w, protocol.HealthResponse{Status: "ok", Protocol: protocol.Version})
				return
			}
			if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
				t.Fatal(err)
			}
			writeJSON(w, protocol.NoteMutationResponse{})
		})
		c.device = "configured-device"
		c = c.WithVault("personal")

		_, err := c.AppendNote(context.Background(), protocol.AppendNoteRequest{
			Path: "Triage.md", Content: "after\n",
		})
		if err != nil {
			t.Fatal(err)
		}
		if received.Device != "configured-device" {
			t.Errorf("device = %q, want configured-device", received.Device)
		}
	})

	t.Run("edit", func(t *testing.T) {
		var received protocol.EditNoteRequest
		c := stub(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/healthz" {
				writeJSON(w, protocol.HealthResponse{Status: "ok", Protocol: protocol.Version})
				return
			}
			if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
				t.Fatal(err)
			}
			writeJSON(w, protocol.NoteMutationResponse{})
		})
		c.device = "configured-device"
		c = c.WithVault("personal")

		_, err := c.EditNote(context.Background(), protocol.EditNoteRequest{
			Path: "Triage.md", ContentRevision: "content-revision",
			OldText: "target", NewText: "changed",
		})
		if err != nil {
			t.Fatal(err)
		}
		if received.Device != "configured-device" {
			t.Errorf("device = %q, want configured-device", received.Device)
		}
	})
}
