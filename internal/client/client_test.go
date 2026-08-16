package client

import (
	"context"
	"encoding/json"
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

// An error body with no envelope (an old server, or a proxy in the way) must
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
		writeJSON(w, protocol.IndexResponse{
			Service: "archivist", Version: "0.3.0", Protocol: protocol.Version,
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
