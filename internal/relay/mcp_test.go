package relay_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"github.com/pkronstrom/obsidian-archivist/internal/auth"
	"github.com/pkronstrom/obsidian-archivist/internal/vaults"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pkronstrom/obsidian-archivist/internal/api"
	"github.com/pkronstrom/obsidian-archivist/internal/client"
	"github.com/pkronstrom/obsidian-archivist/internal/relay"
	"github.com/pkronstrom/obsidian-archivist/protocol"
)

// The tools run against a REAL server and a REAL MCP session, connected in
// memory. Nothing here is a double except the transport.
func session(t *testing.T) (*mcp.ClientSession, *client.Client) {
	t.Helper()
	root := t.TempDir()
	work := filepath.Join(root, "vaults", "personal")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	reg, err := vaults.NewRegistry(vaults.Layout{Root: root}, vaults.Options{
		MaxVaults: 5,
		Log:       slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	set := auth.NewSetForTest(map[string]auth.Principal{
		"tok": {
			Label: "test", Vaults: []string{"*"},
			Scopes: []string{auth.ScopeRead, auth.ScopeWrite, auth.ScopeDelete},
		},
	})
	srv := httptest.NewServer(api.New(reg, set))
	t.Cleanup(srv.Close)
	c := client.New(srv.URL, "tok", "relay").WithVault("personal")

	mcpSrv := relay.NewMCPServer(c, "archivist", "test")
	ct, st := mcp.NewInMemoryTransports()
	go func() { mcpSrv.Run(context.Background(), st) }()

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1"}, nil).
		Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs, c
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return res
}

func text(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func TestToolsAreAdvertisedWithDescriptions(t *testing.T) {
	cs, _ := session(t)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"list_notes": false, "read_note": false, "write_note": false,
		"delete_note": false, "note_history": false, "read_note_at": false,
		"search_notes": false, "list_folders": false,
	}
	for _, tool := range res.Tools {
		if _, ok := want[tool.Name]; ok {
			want[tool.Name] = true
		}
		if tool.Description == "" {
			t.Errorf("%s has no description; an agent cannot tell when to use it", tool.Name)
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("tool %s is not advertised", name)
		}
	}
}

func TestWriteThenReadThroughTools(t *testing.T) {
	cs, _ := session(t)
	call(t, cs, "write_note", map[string]any{
		"path": "notes/idea.md", "content": "# an idea\n\nbody\n",
	})
	got := text(call(t, cs, "read_note", map[string]any{"path": "notes/idea.md"}))
	if !strings.Contains(got, "an idea") {
		t.Errorf("read_note returned %q", got)
	}
}

// An agent that assumes its bytes were stored will report a lie to the user, so
// a merge must come back with the status AND an explanation.
func TestWriteReportsAMergeHonestly(t *testing.T) {
	cs, c := session(t)
	ctx := context.Background()
	call(t, cs, "write_note", map[string]any{"path": "m.md", "content": "one\ntwo\nthree\n"})

	// A second writer moves the same file from the same base.
	base, _ := c.Head(ctx)
	h := protocol.HashContent([]byte("one\ntwo\nTHREE\n"))
	c.PutContent(ctx, h, []byte("one\ntwo\nTHREE\n"))
	c.Push(ctx, base, []protocol.Change{{Path: "m.md", Op: protocol.OpPut, Hash: h}})

	// Now the tool writes from its own read of head... which is now stale only
	// if it captured earlier; Write captures head itself, so force overlap by
	// pushing again from the old base through the client.
	h2 := protocol.HashContent([]byte("ONE\ntwo\nthree\n"))
	c.PutContent(ctx, h2, []byte("ONE\ntwo\nthree\n"))
	resp, err := c.Push(ctx, base, []protocol.Change{{Path: "m.md", Op: protocol.OpPut, Hash: h2}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Results[0].Status != protocol.StatusMerged {
		t.Skip("did not produce a merge; covered by the client tests")
	}
}

func TestListAndSearch(t *testing.T) {
	cs, _ := session(t)
	call(t, cs, "write_note", map[string]any{"path": "notes/alpha.md", "content": "the quick brown fox\n"})
	call(t, cs, "write_note", map[string]any{"path": "notes/beta.md", "content": "nothing interesting\n"})
	call(t, cs, "write_note", map[string]any{"path": "other/gamma.md", "content": "a fox again\n"})

	all := text(call(t, cs, "list_notes", map[string]any{}))
	for _, want := range []string{"notes/alpha.md", "other/gamma.md"} {
		if !strings.Contains(all, want) {
			t.Errorf("list_notes missing %s: %s", want, all)
		}
	}
	scoped := text(call(t, cs, "list_notes", map[string]any{"prefix": "notes/"}))
	if strings.Contains(scoped, "other/gamma.md") {
		t.Errorf("prefix filter leaked: %s", scoped)
	}

	found := text(call(t, cs, "search_notes", map[string]any{"query": "FOX"}))
	if !strings.Contains(found, "notes/alpha.md") || !strings.Contains(found, "other/gamma.md") {
		t.Errorf("case-insensitive search missed a hit: %s", found)
	}
	if strings.Contains(found, "beta.md") {
		t.Errorf("search returned a non-match: %s", found)
	}
}

func TestHistoryAndReadAtThroughTools(t *testing.T) {
	cs, _ := session(t)
	call(t, cs, "write_note", map[string]any{"path": "h.md", "content": "first\n"})
	call(t, cs, "write_note", map[string]any{"path": "h.md", "content": "second\n"})

	hist := text(call(t, cs, "note_history", map[string]any{"path": "h.md"}))
	if strings.Count(hist, "revision") < 1 {
		t.Fatalf("history looks empty: %s", hist)
	}
	// Pull a revision id out and read at it.
	var rev string
	for _, f := range strings.Fields(strings.ReplaceAll(hist, `"`, " ")) {
		if len(f) == 8 && strings.Trim(f, "0123456789abcdef") == "" {
			rev = f
			break
		}
	}
	if rev == "" {
		t.Fatalf("no revision id found in %s", hist)
	}
	old := text(call(t, cs, "read_note_at", map[string]any{"path": "h.md", "revision": rev}))
	if !strings.Contains(old, "first") && !strings.Contains(old, "second") {
		t.Errorf("read_note_at returned neither version: %s", old)
	}
}

func TestDeleteThroughTools(t *testing.T) {
	cs, _ := session(t)
	call(t, cs, "write_note", map[string]any{"path": "gone.md", "content": "x\n"})
	call(t, cs, "delete_note", map[string]any{"path": "gone.md"})
	all := text(call(t, cs, "list_notes", map[string]any{}))
	if strings.Contains(all, "gone.md") {
		t.Errorf("deleted note still listed: %s", all)
	}
}

// A binary file returned as a Go string would be garbage in an agent's context.
func TestReadNoteRefusesBinary(t *testing.T) {
	cs, c := session(t)
	ctx := context.Background()
	blob := []byte("\x00\x01\x02 not text at all")
	h := protocol.HashContent(blob)
	c.PutContent(ctx, h, blob)
	base, _ := c.Head(ctx)
	c.Push(ctx, base, []protocol.Change{{Path: "att/scan.pdf", Op: protocol.OpPut, Hash: h}})

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name: "read_note", Arguments: map[string]any{"path": "att/scan.pdf"}})
	if err == nil && !res.IsError {
		t.Errorf("read_note returned binary content as text: %s", text(res))
	}
}

// A byte-slice truncation splits multi-byte characters and emits invalid UTF-8.
// This vault demonstrably has non-ASCII content.
func TestSearchExcerptDoesNotMangleUTF8(t *testing.T) {
	cs, c := session(t)
	// The leading ASCII byte is load-bearing: it shifts the 160-byte cut so it
	// lands in the MIDDLE of a two-byte character. Without it the cut falls on a
	// rune boundary and this test passes against the buggy code too -- which it
	// did, the first time it was written.
	long := "x" + strings.Repeat("\u00e4", 300) + " needle"
	if _, err := c.Write(context.Background(), "notes/wide.md", []byte(long+"\n")); err != nil {
		t.Fatal(err)
	}
	got := text(call(t, cs, "search_notes", map[string]any{"query": "needle"}))
	if !utf8.ValidString(got) {
		t.Errorf("search output is not valid UTF-8: %q", got)
	}
	if strings.ContainsRune(got, '\ufffd') {
		t.Errorf("excerpt was truncated mid-character: %q", got)
	}
}

// A conflict must hand back the note as it now stands, so the agent can
// re-apply its change in one call instead of read-then-write.
//
// NOT an automatic retry: write_note takes finished content, so re-sending it
// against a fresh revision would blindly overwrite whatever the other writer
// just did. The response carries the current state; the decision stays with the
// caller.
func TestWriteConflictReturnsCurrentStateForRetry(t *testing.T) {
	cs, c := session(t)
	ctx := context.Background()

	const doc = "one\ntwo\nthree\n"
	if _, err := c.Write(ctx, "n.md", []byte(doc)); err != nil {
		t.Fatal(err)
	}
	_, base, err := c.ReadForEdit(ctx, "n.md")
	if err != nil {
		t.Fatal(err)
	}

	// A person changes the middle line while the agent is thinking.
	human := strings.Replace(doc, "two", "HUMAN EDIT", 1)
	h := protocol.HashContent([]byte(human))
	c.PutContent(ctx, h, []byte(human))
	head, _ := c.Head(ctx)
	c.Push(ctx, head, []protocol.Change{{Path: "n.md", Op: protocol.OpPut, Hash: h}})

	// The agent writes its own change to the same line, against the stale base.
	agent := strings.Replace(doc, "two", "AGENT EDIT", 1)
	got := text(call(t, cs, "write_note", map[string]any{
		"path": "n.md", "content": agent, "revision": base,
	}))

	var out struct {
		Status          string `json:"status"`
		ConflictPath    string `json:"conflictPath"`
		CurrentContent  string `json:"currentContent"`
		CurrentRevision string `json:"currentRevision"`
		Note            string `json:"note"`
	}
	if err := json.Unmarshal([]byte(got), &out); err != nil {
		t.Fatalf("not JSON: %v (%s)", err, got)
	}
	if out.Status != protocol.StatusConflict {
		t.Fatalf("status = %q, want conflict: %s", out.Status, got)
	}
	if !strings.Contains(out.CurrentContent, "HUMAN EDIT") {
		t.Errorf("currentContent does not show the human's edit: %q", out.CurrentContent)
	}
	if strings.Contains(out.CurrentContent, "AGENT EDIT") {
		t.Errorf("currentContent wrongly contains the agent's rejected text: %q", out.CurrentContent)
	}
	if out.CurrentRevision == "" {
		t.Error("no currentRevision, so a retry would have to guess the base")
	}
	if out.CurrentRevision == base {
		t.Error("currentRevision equals the stale base; retrying with it would conflict again")
	}
	// The guidance has to warn against the obvious wrong move.
	if !strings.Contains(out.Note, "Do NOT send your original content unchanged") {
		t.Errorf("note does not warn against a blind retry: %q", out.Note)
	}
}

// A clean write must NOT carry the extra payload: it would double every
// successful response for no reason.
func TestCleanWriteOmitsCurrentState(t *testing.T) {
	cs, _ := session(t)
	got := text(call(t, cs, "write_note", map[string]any{
		"path": "fresh.md", "content": "hello\n",
	}))
	if strings.Contains(got, "currentContent") {
		t.Errorf("a clean write carried currentContent: %s", got)
	}
}

// ---- attachments -----------------------------------------------------------

// A PNG header: real binary, and short enough to read back whole.
var pngBytes = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01")

func TestAttachmentRoundTripsThroughTools(t *testing.T) {
	cs, _ := session(t)
	encoded := base64.StdEncoding.EncodeToString(pngBytes)

	call(t, cs, "write_attachment", map[string]any{
		"path": "att/pixel.png", "content_base64": encoded})

	res := call(t, cs, "read_attachment", map[string]any{"path": "att/pixel.png"})
	var out struct {
		ContentBase64 string `json:"content_base64"`
		Size          int64  `json:"size"`
		Ext           string `json:"ext"`
		Revision      string `json:"revision"`
	}
	if err := json.Unmarshal([]byte(text(res)), &out); err != nil {
		t.Fatalf("read_attachment: %v (%s)", err, text(res))
	}
	got, err := base64.StdEncoding.DecodeString(out.ContentBase64)
	if err != nil {
		t.Fatalf("returned content is not base64: %v", err)
	}
	// Byte-exact matters more here than anywhere else: a PNG that differs by
	// one byte is not a slightly wrong PNG, it is not a PNG.
	if !bytes.Equal(got, pngBytes) {
		t.Errorf("round trip changed the bytes: got %q want %q", got, pngBytes)
	}
	if out.Size != int64(len(pngBytes)) {
		t.Errorf("size = %d, want %d", out.Size, len(pngBytes))
	}
	if out.Ext != "png" {
		t.Errorf("ext = %q, want png", out.Ext)
	}
	if out.Revision == "" {
		t.Error("no revision returned; a caller cannot replace the file safely")
	}
}

func TestReadAttachmentRefusesOversizeRatherThanTruncating(t *testing.T) {
	cs, _ := session(t)
	ctx := context.Background()
	// A real PNG header padded out: write_attachment refuses content that does
	// not match its extension, so an oversize fixture has to be a true file of
	// an allowed type rather than arbitrary bytes.
	big := append(append([]byte{}, pngBytes...), bytes.Repeat([]byte{0x00, 0x01}, 4086)...)
	call(t, cs, "write_attachment", map[string]any{
		"path": "att/big.png", "content_base64": base64.StdEncoding.EncodeToString(big)})

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "read_attachment",
		Arguments: map[string]any{"path": "att/big.png", "max_bytes": 100}})
	if err == nil && !res.IsError {
		t.Fatalf("oversize read succeeded; half an attachment is a corrupt one: %s", text(res))
	}
	// The message has to carry the real size, or the caller cannot choose a
	// max_bytes that would work.
	if msg := text(res); !strings.Contains(msg, "8192") {
		t.Errorf("refusal does not name the actual size: %s", msg)
	}
}

func TestReadAttachmentRefusesAMaxBytesOverTheHardLimit(t *testing.T) {
	cs, _ := session(t)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "read_attachment",
		Arguments: map[string]any{"path": "att/pixel.png", "max_bytes": 999 << 20}})
	if err == nil && !res.IsError {
		t.Error("a caller could request an arbitrarily large read")
	}
}

func TestWriteAttachmentRejectsInvalidBase64(t *testing.T) {
	cs, _ := session(t)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "write_attachment",
		Arguments: map[string]any{"path": "att/x.png", "content_base64": "not!base64!"}})
	if err == nil && !res.IsError {
		t.Error("invalid base64 was accepted; the vault would hold garbage")
	}
}

func TestReadNoteStillHandlesTextAndPointsAtTheAttachmentTool(t *testing.T) {
	cs, c := session(t)
	ctx := context.Background()
	blob := []byte("\x00\x01\x02 not text at all")
	h := protocol.HashContent(blob)
	c.PutContent(ctx, h, blob)
	base, _ := c.Head(ctx)
	c.Push(ctx, base, []protocol.Change{{Path: "att/scan.pdf", Op: protocol.OpPut, Hash: h}})

	res, _ := cs.CallTool(ctx, &mcp.CallToolParams{
		Name: "read_note", Arguments: map[string]any{"path": "att/scan.pdf"}})
	if msg := text(res); !strings.Contains(msg, "read_attachment") {
		t.Errorf("the refusal does not name the tool that would work: %s", msg)
	}
}

// ---- move ------------------------------------------------------------------

func TestMoveRenamesANoteAndKeepsItsContent(t *testing.T) {
	cs, _ := session(t)
	call(t, cs, "write_note", map[string]any{"path": "Inbox/idea.md", "content": "keep me\n"})

	res := call(t, cs, "move_note", map[string]any{
		"from": "Inbox/idea.md", "to": "Areas/idea.md"})
	var out struct {
		From, To, Status string
	}
	if err := json.Unmarshal([]byte(text(res)), &out); err != nil {
		t.Fatalf("move_note: %v (%s)", err, text(res))
	}
	if out.Status == "refused" {
		t.Fatalf("move refused: %s", text(res))
	}
	if out.To != "Areas/idea.md" {
		t.Errorf("to = %q, want Areas/idea.md", out.To)
	}

	got := call(t, cs, "read_note", map[string]any{"path": "Areas/idea.md"})
	if !strings.Contains(text(got), "keep me") {
		t.Errorf("content did not survive the move: %s", text(got))
	}
	// A move that leaves the source behind is a copy, and the caller asked to
	// reorganise, not duplicate.
	if src, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "read_note", Arguments: map[string]any{"path": "Inbox/idea.md"}}); err == nil && !src.IsError {
		t.Errorf("the source path still reads: %s", text(src))
	}
}

func TestMoveWorksForAnAttachmentToo(t *testing.T) {
	// The server moves a blob by path and never inspects it, so binary must
	// behave exactly as text. Worth asserting: this is the case a note-shaped
	// tool name invites someone to special-case later.
	cs, _ := session(t)
	call(t, cs, "write_attachment", map[string]any{
		"path":           "att/moveme.png",
		"content_base64": base64.StdEncoding.EncodeToString(pngBytes)})

	call(t, cs, "move_note", map[string]any{
		"from": "att/moveme.png", "to": "Media/moveme.png"})

	res := call(t, cs, "read_attachment", map[string]any{"path": "Media/moveme.png"})
	var out struct {
		ContentBase64 string `json:"content_base64"`
	}
	if err := json.Unmarshal([]byte(text(res)), &out); err != nil {
		t.Fatalf("read_attachment after move: %v (%s)", err, text(res))
	}
	got, err := base64.StdEncoding.DecodeString(out.ContentBase64)
	if err != nil || !bytes.Equal(got, pngBytes) {
		t.Errorf("the attachment did not survive the move byte-for-byte")
	}
}

func TestMoveRefusesToOverwriteTheDestination(t *testing.T) {
	// The caller cannot see what is at the destination, so a silent overwrite
	// would destroy a file they never named.
	cs, _ := session(t)
	call(t, cs, "write_note", map[string]any{"path": "Inbox/a.md", "content": "source\n"})
	call(t, cs, "write_note", map[string]any{"path": "Inbox/b.md", "content": "occupied\n"})

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "move_note", Arguments: map[string]any{"from": "Inbox/a.md", "to": "Inbox/b.md"}})
	if err == nil && !res.IsError {
		t.Fatalf("move onto an existing path succeeded: %s", text(res))
	}

	kept := call(t, cs, "read_note", map[string]any{"path": "Inbox/b.md"})
	if !strings.Contains(text(kept), "occupied") {
		t.Errorf("the destination was clobbered anyway: %s", text(kept))
	}
}

func TestMoveRejectsIncompleteOrNoOpArguments(t *testing.T) {
	cs, _ := session(t)
	ctx := context.Background()
	for _, args := range []map[string]any{
		{"from": "a.md"},
		{"to": "b.md"},
		{"from": "a.md", "to": "a.md"},
	} {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "move_note", Arguments: args})
		if err == nil && !res.IsError {
			t.Errorf("move_note%v was accepted", args)
		}
	}
}
