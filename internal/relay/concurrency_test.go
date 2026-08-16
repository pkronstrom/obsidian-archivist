package relay_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pkronstrom/obsidian-archivist/internal/client"
	"github.com/pkronstrom/obsidian-archivist/internal/relay"
	"github.com/pkronstrom/obsidian-archivist/protocol"
)

const doc = "line1\nline2\nline3\nline4\nline5\n"

// concurrentEdit commits a change to n.md from a different writer, as if
// another device had synced while the caller was thinking.
func concurrentEdit(t *testing.T, c interface {
	PutContent(context.Context, string, []byte) error
	Head(context.Context) (string, error)
	Push(context.Context, string, []protocol.Change) (protocol.PushResponse, error)
}, content string) {
	t.Helper()
	ctx := context.Background()
	h := protocol.HashContent([]byte(content))
	if err := c.PutContent(ctx, h, []byte(content)); err != nil {
		t.Fatal(err)
	}
	head, err := c.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Push(ctx, head, []protocol.Change{
		{Path: "n.md", Op: protocol.OpPut, Hash: h}}); err != nil {
		t.Fatal(err)
	}
}

// An agent that reads a note, edits it and writes it back must not silently
// discard a change that landed in between. read_note hands back a revision and
// write_note takes it; this checks the round trip actually protects the note.
func TestMCPReadEditWriteDoesNotLoseAConcurrentEdit(t *testing.T) {
	cs, c := session(t)
	ctx := context.Background()
	c.Write(ctx, "n.md", []byte(doc))

	// The agent reads, and gets a revision with the content.
	readOut := text(call(t, cs, "read_note", map[string]any{"path": "n.md"}))
	rev := jsonField(t, readOut, "revision")
	if rev == "" {
		t.Fatal("read_note returned no revision; an agent cannot write back safely")
	}

	concurrentEdit(t, c, strings.Replace(doc, "line5", "line5 THEIRS", 1))

	// The agent writes its edit back, echoing the revision.
	call(t, cs, "write_note", map[string]any{
		"path":     "n.md",
		"content":  strings.Replace(doc, "line1", "line1 MINE", 1),
		"revision": rev,
	})

	final, _ := c.Read(ctx, "n.md")
	if !strings.Contains(string(final), "THEIRS") {
		t.Errorf("LOST UPDATE through MCP: %q", final)
	}
	if !strings.Contains(string(final), "MINE") {
		t.Errorf("the agent's own edit is gone: %q", final)
	}
}

// The same guarantee over the plain HTTP API, via ETag and If-Match.
func TestHTTPIfMatchDoesNotLoseAConcurrentEdit(t *testing.T) {
	c := liveClient(t)
	srv := crudServerFor(t, c)
	ctx := context.Background()
	c.Write(ctx, "n.md", []byte(doc))

	resp := req(t, srv, "GET", "/file/n.md", nil, true)
	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Fatal("GET returned no ETag; a caller cannot write back safely")
	}

	concurrentEdit(t, c, strings.Replace(doc, "line5", "line5 THEIRS", 1))

	mine := strings.Replace(doc, "line1", "line1 MINE", 1)
	w := reqWithHeaders(t, srv, "PUT", "/file/n.md", []byte(mine), true,
		map[string]string{"If-Match": etag})
	if w.StatusCode != 200 && w.StatusCode != 409 {
		t.Fatalf("PUT = %d", w.StatusCode)
	}

	final, _ := c.Read(ctx, "n.md")
	if !strings.Contains(string(final), "THEIRS") {
		t.Errorf("LOST UPDATE through the HTTP API: %q", final)
	}
	if !strings.Contains(string(final), "MINE") {
		t.Errorf("the caller's own edit is gone: %q", final)
	}
}

// Without If-Match the write is documented as a blind overwrite. Pinned so the
// difference stays deliberate.
func TestHTTPWithoutIfMatchOverwrites(t *testing.T) {
	c := liveClient(t)
	srv := crudServerFor(t, c)
	ctx := context.Background()
	c.Write(ctx, "n.md", []byte(doc))
	req(t, srv, "GET", "/file/n.md", nil, true)

	concurrentEdit(t, c, strings.Replace(doc, "line5", "line5 THEIRS", 1))
	req(t, srv, "PUT", "/file/n.md", []byte("replaced\n"), true)

	final, _ := c.Read(ctx, "n.md")
	if string(final) != "replaced\n" {
		t.Errorf("a PUT with no If-Match should overwrite; got %q", final)
	}
}

// --- helpers ---------------------------------------------------------------

func crudServerFor(t *testing.T, c *client.Client) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(relay.NewHandler(c, relayTok, quiet(), nil, nil))
	t.Cleanup(srv.Close)
	return srv
}

func reqWithHeaders(t *testing.T, srv *httptest.Server, method, path string,
	body []byte, auth bool, hdr map[string]string) *http.Response {
	t.Helper()
	var r *http.Request
	var err error
	if body != nil {
		r, err = http.NewRequest(method, srv.URL+path, bytes.NewReader(body))
	} else {
		r, err = http.NewRequest(method, srv.URL+path, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	if auth {
		r.Header.Set("Authorization", "Bearer "+relayTok)
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// jsonField pulls one string field out of a tool's JSON result.
func jsonField(t *testing.T, blob, key string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(blob), &m); err != nil {
		t.Fatalf("tool result is not JSON: %v (%s)", err, blob)
	}
	v, _ := m[key].(string)
	return v
}
