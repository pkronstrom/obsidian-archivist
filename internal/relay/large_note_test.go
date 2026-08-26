package relay_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pkronstrom/obsidian-archivist/internal/client"
	"github.com/pkronstrom/obsidian-archivist/protocol"
)

const continuationNote = "pass next_cursor as cursor to continue"

type readPageResult struct {
	Path            string `json:"path"`
	Content         string `json:"content"`
	Revision        string `json:"revision"`
	ContentRevision string `json:"content_revision"`
	HasMore         *bool  `json:"has_more"`
	NextCursor      string `json:"next_cursor"`
	Note            string `json:"note"`
}

func readPage(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) readPageResult {
	t.Helper()
	res := call(t, cs, tool, args)
	if res.IsError {
		t.Fatalf("%s returned an MCP error: %s", tool, text(res))
	}
	var out readPageResult
	if err := json.Unmarshal([]byte(text(res)), &out); err != nil {
		t.Fatalf("decode %s result: %v (%s)", tool, err, text(res))
	}
	if out.HasMore == nil {
		t.Fatalf("%s result omitted has_more: %s", tool, text(res))
	}
	return out
}

func readRefusal(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) string {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		return err.Error()
	}
	if res == nil {
		t.Fatalf("%s returned neither a result nor an error", tool)
	}
	if !res.IsError {
		t.Fatalf("%s unexpectedly succeeded: %s", tool, text(res))
	}
	return text(res)
}

func requireReadCode(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any, code string) {
	t.Helper()
	if got := readRefusal(t, cs, tool, args); !strings.Contains(got, code) {
		t.Fatalf("%s refusal = %q, want error code %q", tool, got, code)
	}
}

func writeClientNote(t *testing.T, c *client.Client, path, content string) {
	t.Helper()
	if _, err := c.Write(context.Background(), path, []byte(content)); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func largeUnicodeNote(t *testing.T) (string, []string) {
	t.Helper()
	lines := make([]string, 5_200)
	var body strings.Builder
	for i := range lines {
		lines[i] = fmt.Sprintf("line-%04d αβ🙂界 lorem\n", i+1)
		body.WriteString(lines[i])
	}
	doc := body.String()
	if got := utf8.RuneCountInString(doc); got <= 100_000 {
		t.Fatalf("large-note fixture has only %d code points", got)
	}
	return doc, lines
}

func prefixRunes(s string, count int) string {
	for offset := range s {
		if count == 0 {
			return s[:offset]
		}
		count--
	}
	return s
}

func checkPageEnvelope(t *testing.T, out readPageResult, path, contentRevision string, maxChars int) {
	t.Helper()
	if out.Path != path {
		t.Errorf("path = %q, want %q", out.Path, path)
	}
	if out.Revision == "" {
		t.Error("repository revision is empty")
	}
	if out.ContentRevision != contentRevision || len(out.ContentRevision) != 40 {
		t.Errorf("content_revision = %q, want full hash %q", out.ContentRevision, contentRevision)
	}
	if !utf8.ValidString(out.Content) {
		t.Error("page content is not valid UTF-8")
	}
	if got := utf8.RuneCountInString(out.Content); got > maxChars {
		t.Errorf("page contains %d code points, max_chars is %d", got, maxChars)
	}
	if *out.HasMore {
		if len(out.NextCursor) != 23 {
			t.Errorf("continuation cursor length = %d, want 23: %q", len(out.NextCursor), out.NextCursor)
		}
		if out.Note != continuationNote {
			t.Errorf("continuation note = %q, want %q", out.Note, continuationNote)
		}
	} else {
		if out.NextCursor != "" {
			t.Errorf("final page returned next_cursor %q", out.NextCursor)
		}
		if out.Note != "" {
			t.Errorf("final page returned continuation note %q", out.Note)
		}
	}
}

func TestReadNotePagesUnicodeExactlyWithChangingLimits(t *testing.T) {
	cs, c := session(t)
	doc, _ := largeUnicodeNote(t)
	const path = "notes/large-unicode.md"
	writeClientNote(t, c, path, doc)

	contentRevision := protocol.HashContent([]byte(doc))
	limits := []int{16_000, 37, 5_003, 100_000}
	var joined bytes.Buffer
	var cursor string

	for pageNumber, maxChars := range limits {
		args := map[string]any{"path": path}
		if pageNumber > 0 {
			args["cursor"] = cursor
			args["max_chars"] = maxChars
		}
		out := readPage(t, cs, "read_note", args)
		checkPageEnvelope(t, out, path, contentRevision, maxChars)
		joined.WriteString(out.Content)

		if pageNumber == 0 && !*out.HasMore {
			t.Fatal("default first page returned the entire 100,000+ code-point note")
		}
		if !*out.HasMore {
			if got := joined.String(); got != doc {
				t.Fatalf("joined pages differ from source: got %d bytes, want %d", len(got), len(doc))
			}
			return
		}
		cursor = out.NextCursor
	}
	t.Fatal("read_note still had more content after the changing page-size sequence")
}

func TestReadNotePagesStartAtKnownLine(t *testing.T) {
	cs, c := session(t)
	doc, lines := largeUnicodeNote(t)
	const (
		path       = "notes/lines.md"
		startLine  = 4_321
		pageLength = 41
	)
	writeClientNote(t, c, path, doc)

	out := readPage(t, cs, "read_note", map[string]any{
		"path": path, "start_line": startLine, "max_chars": pageLength,
	})
	suffix := strings.Join(lines[startLine-1:], "")
	if want := prefixRunes(suffix, pageLength); out.Content != want {
		t.Fatalf("content at start_line %d = %q, want %q", startLine, out.Content, want)
	}
	checkPageEnvelope(t, out, path, protocol.HashContent([]byte(doc)), pageLength)
}

func TestReadNotePagesRejectInvalidPagingInputs(t *testing.T) {
	cs, c := session(t)
	const path = "notes/input-validation.md"
	writeClientNote(t, c, path, "one\ntwo\nthree\nfour\n")

	first := readPage(t, cs, "read_note", map[string]any{"path": path, "max_chars": 3})
	if !*first.HasMore {
		t.Fatal("fixture did not produce a continuation cursor")
	}

	for _, tc := range []struct {
		name string
		args map[string]any
		code string
	}{
		{name: "cursor with start_line", args: map[string]any{"path": path, "cursor": first.NextCursor, "start_line": 2}},
		{name: "zero max_chars", args: map[string]any{"path": path, "max_chars": 0}},
		{name: "negative max_chars", args: map[string]any{"path": path, "max_chars": -1}},
		{name: "max_chars over hard maximum", args: map[string]any{"path": path, "max_chars": 100_001}},
		{name: "bad cursor", args: map[string]any{"path": path, "cursor": "not-a-valid-cursor"}, code: "invalid_cursor"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.code == "" {
				readRefusal(t, cs, "read_note", tc.args)
				return
			}
			requireReadCode(t, cs, "read_note", tc.args, tc.code)
		})
	}
}

func TestReadNotePagesDetectChangedAndMovedNotes(t *testing.T) {
	t.Run("changed content is stale", func(t *testing.T) {
		cs, c := session(t)
		const path = "notes/changing.md"
		writeClientNote(t, c, path, strings.Repeat("αβ🙂\n", 10_000))
		first := readPage(t, cs, "read_note", map[string]any{"path": path, "max_chars": 19})
		writeClientNote(t, c, path, "changed\n")

		requireReadCode(t, cs, "read_note", map[string]any{
			"path": path, "cursor": first.NextCursor,
		}, "stale_cursor")
	})

	t.Run("moved content is not followed", func(t *testing.T) {
		cs, c := session(t)
		const from = "notes/before-move.md"
		writeClientNote(t, c, from, strings.Repeat("move me αβ🙂\n", 2_000))
		first := readPage(t, cs, "read_note", map[string]any{"path": from, "max_chars": 19})
		moved := call(t, cs, "move_note", map[string]any{
			"from": from, "to": "archive/after-move.md",
		})
		if moved.IsError {
			t.Fatalf("move_note failed: %s", text(moved))
		}

		requireReadCode(t, cs, "read_note", map[string]any{
			"path": from, "cursor": first.NextCursor,
		}, "not_found")
	})
}

func TestShortReadReturnsWholeNoteWithoutCursor(t *testing.T) {
	cs, c := session(t)
	const (
		path = "notes/short.md"
		doc  = "# short α🙂\n\nall here\n"
	)
	writeClientNote(t, c, path, doc)

	out := readPage(t, cs, "read_note", map[string]any{"path": path})
	if out.Content != doc {
		t.Errorf("content = %q, want exact short note %q", out.Content, doc)
	}
	checkPageEnvelope(t, out, path, protocol.HashContent([]byte(doc)), 16_000)
	if *out.HasMore || out.NextCursor != "" {
		t.Fatalf("short note unexpectedly paged: %+v", out)
	}
}

func TestReadNoteAtPagesHistoricalContentAndIgnoresCurrentChanges(t *testing.T) {
	cs, c := session(t)
	original, _ := largeUnicodeNote(t)
	const path = "notes/historical-large.md"
	writeClientNote(t, c, path, original)

	revisions, err := c.History(context.Background(), path, 10)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(revisions) == 0 {
		t.Fatal("history returned no revision for fixture")
	}
	revision := revisions[0].Short
	writeClientNote(t, c, path, "current version one\n")

	contentRevision := protocol.HashContent([]byte(original))
	limits := []int{16_000, 53, 4_097, 100_000}
	var joined bytes.Buffer
	var cursor string
	for pageNumber, maxChars := range limits {
		args := map[string]any{"path": path, "revision": revision}
		if pageNumber > 0 {
			args["cursor"] = cursor
			args["max_chars"] = maxChars
		}
		out := readPage(t, cs, "read_note_at", args)
		checkPageEnvelope(t, out, path, contentRevision, maxChars)
		if out.Revision != revision {
			t.Errorf("historical repository revision = %q, want requested %q", out.Revision, revision)
		}
		joined.WriteString(out.Content)

		if pageNumber == 0 {
			writeClientNote(t, c, path, "current version two\n")
		}
		if !*out.HasMore {
			if got := joined.String(); got != original {
				t.Fatalf("historical pages changed: got %d bytes, want %d", len(got), len(original))
			}
			return
		}
		cursor = out.NextCursor
	}
	t.Fatal("read_note_at still had more content after the changing page-size sequence")
}
