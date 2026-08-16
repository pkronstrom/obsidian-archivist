package relay_test

import (
	"bytes"

	"encoding/json"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pkronstrom/obsidian-archivist/internal/relay"
	"github.com/pkronstrom/obsidian-archivist/protocol"
)

const relayTok = "relay-token"

func crudServer(t *testing.T) *httptest.Server {
	t.Helper()
	c := liveClient(t)
	srv := httptest.NewServer(relay.NewHandler(c, relayTok, quiet(), nil, nil))
	t.Cleanup(srv.Close)
	return srv
}

func req(t *testing.T, srv *httptest.Server, method, path string, body []byte, auth bool) *http.Response {
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
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestRelayRequiresItsOwnToken(t *testing.T) {
	srv := crudServer(t)
	for _, p := range []string{"/", "/list", "/file/a.md"} {
		if got := req(t, srv, "GET", p, nil, false).StatusCode; got != 401 {
			t.Errorf("GET %s unauthenticated = %d, want 401", p, got)
		}
	}
}

func TestWriteReadDeleteThroughTheRelay(t *testing.T) {
	srv := crudServer(t)

	resp := req(t, srv, "PUT", "/file/notes/a.md", []byte("# hello\n"), true)
	if resp.StatusCode != 200 {
		t.Fatalf("PUT = %d", resp.StatusCode)
	}
	var res protocol.Result
	json.NewDecoder(resp.Body).Decode(&res)
	if res.Status != protocol.StatusApplied {
		t.Errorf("status = %q, want applied", res.Status)
	}

	resp = req(t, srv, "GET", "/file/notes/a.md", nil, true)
	if resp.StatusCode != 200 {
		t.Fatalf("GET = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "markdown") {
		t.Errorf("Content-Type = %q, want markdown for a .md", ct)
	}
	buf := new(bytes.Buffer)
	buf.ReadFrom(resp.Body)
	if buf.String() != "# hello\n" {
		t.Errorf("GET body = %q", buf)
	}

	if got := req(t, srv, "DELETE", "/file/notes/a.md", nil, true).StatusCode; got != 200 {
		t.Errorf("DELETE = %d", got)
	}
	if got := req(t, srv, "GET", "/file/notes/a.md", nil, true).StatusCode; got == 200 {
		t.Error("the note is still readable after DELETE")
	}
}

func TestListAndPrefix(t *testing.T) {
	srv := crudServer(t)
	req(t, srv, "PUT", "/file/notes/a.md", []byte("a\n"), true)
	req(t, srv, "PUT", "/file/other/b.md", []byte("b\n"), true)

	var all struct {
		Count int                       `json:"count"`
		Files map[string]protocol.Entry `json:"files"`
	}
	json.NewDecoder(req(t, srv, "GET", "/list", nil, true).Body).Decode(&all)
	if all.Count != 2 {
		t.Errorf("count = %d, want 2", all.Count)
	}
	var scoped struct {
		Count int `json:"count"`
	}
	json.NewDecoder(req(t, srv, "GET", "/list?prefix=notes/", nil, true).Body).Decode(&scoped)
	if scoped.Count != 1 {
		t.Errorf("prefixed count = %d, want 1", scoped.Count)
	}
}

// Escaping paths must not create anything outside the vault.
//
// Asserting a specific status here was wrong: Go's http.Client normalises the
// path before sending, and http.ServeMux cleans it again and answers 307, so
// the handler never sees the hostile form. What matters is the OUTCOME, and
// the raw-socket version of this test (rawpath_test.go) covers the case where
// a client does not normalise for us.
func TestEscapingPathsCreateNothingOutsideTheVault(t *testing.T) {
	c := liveClient(t)
	srv := httptest.NewServer(relay.NewHandler(c, relayTok, quiet(), nil, nil))
	t.Cleanup(srv.Close)

	for _, p := range []string{"/file/../escape.md", "/file/a/../../b.md", "/file/./x.md"} {
		req(t, srv, "PUT", p, []byte("pwned\n"), true)
	}
	files, err := c.List(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	for path := range files {
		if strings.Contains(path, "..") || strings.HasPrefix(path, "/") {
			t.Errorf("an escaping path was created: %q", path)
		}
	}
}

// A conflict must be visible from the status alone, for callers that ignore
// the body.
// The conflict must surface through the RELAY, as an HTTP status.
//
// The previous version of this test pushed the conflicting change with the Go
// client and never sent it through srv at all, then ended in t.Skipf -- so it
// asserted nothing, and deleting the relay's 409 mapping would have kept it
// green. Same failure as the TypeScript client test that made the 409 path
// unreachable: a test that exercises the setup instead of the subject.
func TestConflictSurfacesAs409(t *testing.T) {
	c := liveClient(t)
	srv := crudServerFor(t, c)

	req(t, srv, "PUT", "/file/c.md", []byte(doc), true)
	etag := req(t, srv, "GET", "/file/c.md", nil, true).Header.Get("ETag")
	if etag == "" {
		t.Fatal("no ETag to be conditional on")
	}

	// Another writer changes the same line, so the two edits cannot merge.
	concurrentEditPath(t, c, "c.md", strings.Replace(doc, "line3", "line3 THEIRS", 1))

	mine := strings.Replace(doc, "line3", "line3 MINE", 1)
	resp := reqWithHeaders(t, srv, "PUT", "/file/c.md", []byte(mine), true,
		map[string]string{"If-Match": etag})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("conflicting PUT = %d, want 409", resp.StatusCode)
	}
	var res protocol.Result
	json.NewDecoder(resp.Body).Decode(&res)
	if res.Status != protocol.StatusConflict || res.ConflictPath == "" {
		t.Errorf("body does not describe the conflict: %+v", res)
	}
}

// A refused delete used to come back 200, so a caller checking only the status
// would report a note as removed while it was still in the vault.
func TestRefusedDeleteIsNot200(t *testing.T) {
	c := liveClient(t)
	srv := crudServerFor(t, c)

	req(t, srv, "PUT", "/file/d.md", []byte(doc), true)
	etag := req(t, srv, "GET", "/file/d.md", nil, true).Header.Get("ETag")

	// The note changes after the caller decided to delete it.
	concurrentEditPath(t, c, "d.md", strings.Replace(doc, "line3", "line3 THEIRS", 1))

	resp := reqWithHeaders(t, srv, "DELETE", "/file/d.md", nil, true,
		map[string]string{"If-Match": etag})
	var res protocol.Result
	json.NewDecoder(resp.Body).Decode(&res)
	if res.Status == protocol.StatusApplied {
		return // the server allowed it; nothing to assert
	}
	if resp.StatusCode == http.StatusOK {
		t.Errorf("delete came back %q but HTTP 200: a caller would report success",
			res.Status)
	}
}

// A wildcard If-Match cannot be honoured as a revision. Turning it into an
// absent base would make a request that ASKED to be conditional a blind
// overwrite, so it is refused instead.
func TestWildcardIfMatchIsRefused(t *testing.T) {
	srv := crudServer(t)
	resp := reqWithHeaders(t, srv, "PUT", "/file/w.md", []byte("x\n"), true,
		map[string]string{"If-Match": "*"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("If-Match: * = %d, want 400", resp.StatusCode)
	}
}

func TestIndexDescribesItself(t *testing.T) {
	srv := crudServer(t)
	var idx struct {
		Service   string              `json:"service"`
		Protocol  int                 `json:"protocol"`
		Endpoints []protocol.Endpoint `json:"endpoints"`
	}
	json.NewDecoder(req(t, srv, "GET", "/", nil, true).Body).Decode(&idx)
	if idx.Service != "archivist-relay" || idx.Protocol != protocol.Version {
		t.Errorf("index = %+v", idx)
	}
	if len(idx.Endpoints) < 4 {
		t.Errorf("only %d endpoints described", len(idx.Endpoints))
	}
	for _, e := range idx.Endpoints {
		if e.Does == "" {
			t.Errorf("%s %s has no description", e.Method, e.Path)
		}
	}
}

// A relay that is up but cannot reach the vault is not usefully healthy.
func TestHealthzReportsUpstream(t *testing.T) {
	srv := crudServer(t)
	resp := req(t, srv, "GET", "/healthz", nil, false) // no auth on purpose
	if resp.StatusCode != 200 {
		t.Fatalf("healthz = %d", resp.StatusCode)
	}
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	if out["status"] != "ok" {
		t.Errorf("status = %v", out["status"])
	}
	if _, ok := out["upstream"]; !ok {
		t.Error("healthz does not report upstream reachability")
	}
}

func TestOversizedBodyIs413(t *testing.T) {
	srv := crudServer(t)
	big := bytes.Repeat([]byte("x"), (32<<20)+64)
	if got := req(t, srv, "PUT", "/file/big.md", big, true).StatusCode; got != 413 {
		t.Errorf("code = %d, want 413", got)
	}
}

// Building the handler WITH MCP is the configuration that actually ships, and
// it used to panic at registration: a bare "GET /" catch-all conflicts with
// "/mcp". Every other test passed a nil MCP handler and so never built it.
func TestHandlerWithMCPEnabledDoesNotPanic(t *testing.T) {
	c := liveClient(t)
	mcpSrv := relay.NewMCPServer(c, "archivist", "test")
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpSrv }, nil)

	srv := httptest.NewServer(relay.NewHandler(c, relayTok, quiet(), h, nil))
	t.Cleanup(srv.Close)

	// The ordinary routes still work alongside it.
	if got := req(t, srv, "GET", "/", nil, true).StatusCode; got != 200 {
		t.Errorf("index = %d", got)
	}
	if got := req(t, srv, "PUT", "/file/a.md", []byte("x\n"), true).StatusCode; got != 200 {
		t.Errorf("write = %d", got)
	}
	// And /mcp is reachable and authenticated rather than 404.
	if got := req(t, srv, "GET", "/mcp", nil, false).StatusCode; got != 401 {
		t.Errorf("unauthenticated /mcp = %d, want 401", got)
	}
}

func TestMCPDisabledSaysSoClearly(t *testing.T) {
	srv := crudServer(t) // built with a nil MCP handler
	resp := req(t, srv, "GET", "/mcp", nil, true)
	if resp.StatusCode != 404 {
		t.Errorf("code = %d, want 404", resp.StatusCode)
	}
	var env protocol.ErrorResponse
	json.NewDecoder(resp.Body).Decode(&env)
	if !strings.Contains(env.Error.Message, "MCP is disabled") {
		t.Errorf("message = %q", env.Error.Message)
	}
}
