// Package relay is the sidecar: MCP tools, webhook fan-out and a plain file
// API, all on top of internal/client.
//
// Everything here is stateless. No vault, no cursor, no snapshot -- each
// operation is a straight call to the server. That is what makes the relay safe
// to restart at any moment and impossible to corrupt, and it is why mirror mode
// is deliberately absent: it would be a third implementation of the sync state
// machine, and reviews already found four data-loss bugs across the first two.
package relay

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pkronstrom/obsidian-archivist/internal/client"
	"github.com/pkronstrom/obsidian-archivist/protocol"
)

// NewMCPServer builds the tool set over a client.
//
// The tools are a facade over the wire protocol, which is why they live in this
// repository rather than in a general agent-tooling one: change the protocol
// and the tools change in the same commit, instead of being coupled across
// repositories to a contract they do not control.
func NewMCPServer(c *client.Client, name, version string) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: name, Version: version}, nil)

	mcp.AddTool(s, &mcp.Tool{
		Name: "list_notes",
		Description: "List notes and attachments in the vault. " +
			"Optionally filter by a path prefix such as 'notes/' or 'Inbox/'.",
	}, listNotes(c))

	mcp.AddTool(s, &mcp.Tool{
		Name: "read_note",
		Description: "Read the current content of one note by its vault path, " +
			"e.g. 'notes/idea.md'.",
	}, readNote(c))

	mcp.AddTool(s, &mcp.Tool{
		Name: "write_note",
		Description: "Create or replace a note. If another writer changed the " +
			"same note concurrently the server merges the two; if the edits " +
			"overlap it keeps both and reports a conflict. Always check the " +
			"returned status: 'applied' means your exact content was stored, " +
			"'merged' means the stored content differs from what you sent.",
	}, writeNote(c))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "delete_note",
		Description: "Delete a note. Its history is retained and it can be restored.",
	}, deleteNote(c))

	mcp.AddTool(s, &mcp.Tool{
		Name: "note_history",
		Description: "List the revisions in which a note changed, newest first. " +
			"Use the returned revision with read_note_at to see an old version.",
	}, noteHistory(c))

	mcp.AddTool(s, &mcp.Tool{
		Name: "read_note_at",
		Description: "Read a note as it was at a past revision. This does not " +
			"restore or modify anything.",
	}, readNoteAt(c))

	mcp.AddTool(s, &mcp.Tool{
		Name: "search_notes",
		Description: "Find notes whose content or path contains the given text. " +
			"Case-insensitive substring match, not a regular expression.",
	}, searchNotes(c))

	return s
}

// ---- tool inputs and outputs ----------------------------------------------
//
// Schemas are inferred from these types, and the jsonschema tags become the
// property descriptions an agent reads. They are the tool's real documentation.

type listInput struct {
	Prefix string `json:"prefix,omitempty" jsonschema:"only list paths starting with this, e.g. 'notes/'"`
}

type noteSummary struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

type listOutput struct {
	Count int           `json:"count"`
	Notes []noteSummary `json:"notes"`
}

func listNotes(c *client.Client) mcp.ToolHandlerFor[listInput, listOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in listInput) (*mcp.CallToolResult, listOutput, error) {
		files, err := c.List(ctx, in.Prefix)
		if err != nil {
			return nil, listOutput{}, err
		}
		out := listOutput{Notes: []noteSummary{}}
		for p, e := range files {
			out.Notes = append(out.Notes, noteSummary{Path: p, Size: e.Size})
		}
		sort.Slice(out.Notes, func(i, j int) bool { return out.Notes[i].Path < out.Notes[j].Path })
		out.Count = len(out.Notes)
		return nil, out, nil
	}
}

type pathInput struct {
	Path string `json:"path" jsonschema:"vault-relative path, e.g. 'notes/idea.md'"`
}

type readOutput struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func readNote(c *client.Client) mcp.ToolHandlerFor[pathInput, readOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in pathInput) (*mcp.CallToolResult, readOutput, error) {
		if in.Path == "" {
			return nil, readOutput{}, fmt.Errorf("path is required")
		}
		body, err := c.Read(ctx, in.Path)
		if err != nil {
			return nil, readOutput{}, err
		}
		if isBinary(body) {
			// An agent cannot use raw bytes and would waste a large amount of
			// context discovering that. Say so instead.
			return nil, readOutput{}, fmt.Errorf(
				"%s is a binary file (%d bytes); read_note only returns text", in.Path, len(body))
		}
		return nil, readOutput{Path: in.Path, Content: string(body)}, nil
	}
}

type writeInput struct {
	Path    string `json:"path" jsonschema:"vault-relative path, e.g. 'notes/idea.md'"`
	Content string `json:"content" jsonschema:"the full new content of the note"`
}

type writeOutput struct {
	Path string `json:"path"`
	// Status is 'applied', 'merged', 'conflict' or 'refused'. Only 'applied'
	// means the stored content is exactly what was sent.
	Status       string `json:"status"`
	ConflictPath string `json:"conflictPath,omitempty"`
	Note         string `json:"note,omitempty"`
}

func writeNote(c *client.Client) mcp.ToolHandlerFor[writeInput, writeOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in writeInput) (*mcp.CallToolResult, writeOutput, error) {
		if in.Path == "" {
			return nil, writeOutput{}, fmt.Errorf("path is required")
		}
		res, err := c.Write(ctx, in.Path, []byte(in.Content))
		if err != nil {
			return nil, writeOutput{}, err
		}
		out := writeOutput{Path: res.Path, Status: res.Status, ConflictPath: res.ConflictPath}
		// Spell out the non-obvious outcomes: an agent that assumes its bytes
		// were stored will report a lie to the user.
		switch res.Status {
		case protocol.StatusMerged:
			out.Note = "another writer had changed this note; the two edits were merged, " +
				"so the stored content is not exactly what you sent. Re-read it before editing again."
		case protocol.StatusConflict:
			out.Note = "another writer had changed the same lines; the server kept its version " +
				"and saved yours alongside at " + res.ConflictPath + ". Both are in the vault."
		case protocol.StatusRefused:
			out.Note = "the server declined this write: " + res.Reason
		}
		return nil, out, nil
	}
}

type deleteOutput struct {
	Path   string `json:"path"`
	Status string `json:"status"`
	Note   string `json:"note,omitempty"`
}

func deleteNote(c *client.Client) mcp.ToolHandlerFor[pathInput, deleteOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in pathInput) (*mcp.CallToolResult, deleteOutput, error) {
		if in.Path == "" {
			return nil, deleteOutput{}, fmt.Errorf("path is required")
		}
		res, err := c.Delete(ctx, in.Path)
		if err != nil {
			return nil, deleteOutput{}, err
		}
		out := deleteOutput{Path: res.Path, Status: res.Status}
		if res.Status == protocol.StatusRefused {
			out.Note = "the server declined this deletion: " + res.Reason
		}
		return nil, out, nil
	}
}

type historyInput struct {
	Path  string `json:"path" jsonschema:"vault-relative path"`
	Limit int    `json:"limit,omitempty" jsonschema:"how many revisions to return, default 20"`
}

type revisionSummary struct {
	Revision string `json:"revision"`
	When     string `json:"when"`
	Message  string `json:"message"`
	Size     int64  `json:"size"`
	Deleted  bool   `json:"deleted,omitempty"`
}

type historyOutput struct {
	Path      string            `json:"path"`
	Revisions []revisionSummary `json:"revisions"`
}

func noteHistory(c *client.Client) mcp.ToolHandlerFor[historyInput, historyOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in historyInput) (*mcp.CallToolResult, historyOutput, error) {
		if in.Path == "" {
			return nil, historyOutput{}, fmt.Errorf("path is required")
		}
		limit := in.Limit
		if limit <= 0 {
			limit = 20
		}
		revs, err := c.History(ctx, in.Path, limit)
		if err != nil {
			return nil, historyOutput{}, err
		}
		out := historyOutput{Path: in.Path, Revisions: []revisionSummary{}}
		for _, r := range revs {
			out.Revisions = append(out.Revisions, revisionSummary{
				Revision: r.Short,
				When:     r.When.Format("2006-01-02 15:04"),
				Message:  r.Message,
				Size:     r.Size,
				Deleted:  r.Deleted,
			})
		}
		return nil, out, nil
	}
}

type readAtInput struct {
	Path     string `json:"path" jsonschema:"vault-relative path"`
	Revision string `json:"revision" jsonschema:"a revision from note_history, e.g. '4f3538ca'"`
}

func readNoteAt(c *client.Client) mcp.ToolHandlerFor[readAtInput, readOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in readAtInput) (*mcp.CallToolResult, readOutput, error) {
		if in.Path == "" || in.Revision == "" {
			return nil, readOutput{}, fmt.Errorf("path and revision are both required")
		}
		body, err := c.ReadAt(ctx, in.Revision, in.Path)
		if err != nil {
			return nil, readOutput{}, err
		}
		return nil, readOutput{Path: in.Path, Content: string(body)}, nil
	}
}

type searchInput struct {
	Query string `json:"query" jsonschema:"text to look for, case-insensitive"`
	Limit int    `json:"limit,omitempty" jsonschema:"how many matches to return, default 20"`
}

type searchHit struct {
	Path    string `json:"path"`
	Line    int    `json:"line,omitempty"`
	Excerpt string `json:"excerpt,omitempty"`
}

type searchOutput struct {
	Query   string      `json:"query"`
	Count   int         `json:"count"`
	Matches []searchHit `json:"matches"`
}

// searchNotes reads and scans client-side.
//
// The server has no search endpoint, deliberately: adding one is a real feature
// with real scope, and every consumer that runs ON the server can already grep
// the directory. Doing it here keeps the server small and is fast enough at
// vault scale -- a few thousand notes of a few kilobytes each. If that stops
// being true, the fix belongs on the server and this tool's shape does not
// change.
func searchNotes(c *client.Client) mcp.ToolHandlerFor[searchInput, searchOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in searchInput) (*mcp.CallToolResult, searchOutput, error) {
		if strings.TrimSpace(in.Query) == "" {
			return nil, searchOutput{}, fmt.Errorf("query is required")
		}
		limit := in.Limit
		if limit <= 0 {
			limit = 20
		}
		files, err := c.List(ctx, "")
		if err != nil {
			return nil, searchOutput{}, err
		}

		paths := make([]string, 0, len(files))
		for p := range files {
			paths = append(paths, p)
		}
		sort.Strings(paths)

		needle := strings.ToLower(in.Query)
		out := searchOutput{Query: in.Query, Matches: []searchHit{}}
		for _, p := range paths {
			if len(out.Matches) >= limit {
				break
			}
			if strings.Contains(strings.ToLower(p), needle) {
				out.Matches = append(out.Matches, searchHit{Path: p})
				continue
			}
			// Skip anything too big to be a note; fetching a 200 MB attachment
			// to grep it would be worse than missing it.
			if files[p].Size > 1<<20 {
				continue
			}
			body, err := c.GetContent(ctx, files[p].Hash)
			if err != nil || isBinary(body) {
				continue
			}
			for i, line := range strings.Split(string(body), "\n") {
				if strings.Contains(strings.ToLower(line), needle) {
					out.Matches = append(out.Matches, searchHit{
						Path: p, Line: i + 1, Excerpt: excerpt(line),
					})
					break // one hit per note is enough to decide whether to read it
				}
			}
		}
		out.Count = len(out.Matches)
		return nil, out, nil
	}
}

// excerpt truncates by RUNES, not bytes. Slicing a string at a byte offset
// splits a multi-byte character in half and emits invalid UTF-8, which the JSON
// encoder then replaces with U+FFFD -- a mangled excerpt for any note that is
// not pure ASCII.
func excerpt(line string) string {
	line = strings.TrimSpace(line)
	if utf8.RuneCountInString(line) <= 160 {
		return line
	}
	n := 0
	for i := range line {
		if n == 160 {
			return line[:i] + "…"
		}
		n++
	}
	return line
}

// isBinary is git's heuristic: a NUL byte near the start.
func isBinary(b []byte) bool {
	if len(b) > 8000 {
		b = b[:8000]
	}
	for _, c := range b {
		if c == 0 {
			return true
		}
	}
	return false
}
