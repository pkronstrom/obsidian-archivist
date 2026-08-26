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
	"encoding/base64"
	"errors"
	"fmt"
	"path"
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
		Name: "list_vaults",
		Description: "List the vaults this relay's token can address. Every " +
			"other tool takes an optional 'vault' parameter naming one of " +
			"these; omit it to use the relay's default vault.",
	}, listVaults(c))

	mcp.AddTool(s, &mcp.Tool{
		Name: "list_notes",
		Description: "List notes and attachments. Returns at most 100 entries " +
			"by default; check has_more and pass next_cursor to continue. " +
			"On a large vault prefer recursive=false, which lists one folder " +
			"like ls and collapses the rest into folder rows, or narrow with " +
			"prefix such as '2. Areas/'. To find a note by content or name use " +
			"search_notes instead; to see the shape of the vault use " +
			"list_folders.",
	}, listNotes(c))

	mcp.AddTool(s, &mcp.Tool{
		Name: "list_folders",
		Description: "Map the vault: every folder with how many files and how " +
			"many bytes it holds. Cheap on any vault size and the right first " +
			"call when you do not yet know where something lives. Returns no " +
			"file names -- follow up with list_notes and a prefix.",
	}, listFolders(c))

	mcp.AddTool(s, &mcp.Tool{
		Name: "read_note",
		Description: "Read the current content of one note by its vault path, " +
			"e.g. 'notes/idea.md'. Returns at most 16,000 Unicode characters by " +
			"default; when has_more is true, pass next_cursor as cursor to continue.",
	}, readNote(c))

	mcp.AddTool(s, &mcp.Tool{
		Name: "write_note",
		Description: "Create or replace a note. If another writer changed the " +
			"same note concurrently the server merges the two; if the edits " +
			"overlap it keeps both and reports a conflict. If this content is an " +
			"edit of a note you read, pass the 'revision' read_note returned -- " +
			"without it the write silently overwrites any concurrent change. " +
			"Always check the " +
			"returned status: 'applied' means your exact content was stored, " +
			"'merged' means the stored content differs from what you sent.",
	}, writeNote(c))

	mcp.AddTool(s, &mcp.Tool{
		Name: "read_attachment",
		Description: "Read an attachment (image, pdf, audio, any non-text file) as " +
			"base64. read_note refuses these on purpose; this is the tool for them. " +
			"Capped at 1 MB by default because the bytes land in your context — " +
			"raise max_bytes deliberately, up to 10 MB. Oversize is refused with " +
			"the real size, never truncated: half an attachment is a corrupt one.",
	}, readAttachment(c))

	mcp.AddTool(s, &mcp.Tool{
		Name: "write_attachment",
		Description: "Create or replace an attachment from base64 content. Same " +
			"concurrency rules as write_note: pass the revision you read at, and " +
			"check the returned status. Use write_note for text; this is for bytes. " +
			"Accepts the image, audio, video and pdf types Obsidian renders, and " +
			"refuses executables and any file whose bytes do not match its " +
			"extension — the refusal names the reason.",
	}, writeAttachment(c))

	mcp.AddTool(s, &mcp.Tool{
		Name: "delete_note",
		Description: "Delete a note, or any other file including an attachment — it is " +
			"the path that matters, not the extension. Its history is retained and it " +
			"can be restored.",
	}, deleteNote(c))

	mcp.AddTool(s, &mcp.Tool{
		Name: "move_note",
		Description: "Rename or move one file to a new vault path. Works for any " +
			"file — a note or an attachment. This is a true rename: the content " +
			"is preserved and the file keeps one history, where writing the new " +
			"path and deleting the old one would split it in two and needs a " +
			"delete scope this token may not hold. Refuses when the destination " +
			"already exists; delete it first if that is what you mean.",
	}, moveNote(c))

	mcp.AddTool(s, &mcp.Tool{
		Name: "note_history",
		Description: "List the revisions in which a note changed, newest first. " +
			"Use the returned revision with read_note_at to see an old version.",
	}, noteHistory(c))

	mcp.AddTool(s, &mcp.Tool{
		Name: "read_note_at",
		Description: "Read a note as it was at a past revision. This does not " +
			"restore or modify anything. Returns at most 16,000 Unicode characters " +
			"by default; when has_more is true, pass next_cursor as cursor to continue.",
	}, readNoteAt(c))

	mcp.AddTool(s, &mcp.Tool{
		Name: "unlock",
		Description: "Present a one-time code to unlock a vault that requires step-up " +
			"authentication. Ask the human for the current code from their authenticator " +
			"app; a code is single-use and cannot be reused. Call this after a tool fails " +
			"with step_up_required.",
	}, unlockVault(c))

	mcp.AddTool(s, &mcp.Tool{
		Name: "search_notes",
		Description: "Find notes whose content or path contains the given text. " +
			"Case-insensitive substring match, not a regular expression.",
	}, searchNotes(c))

	return s
}

// forVault picks the client for a tool call.
//
// ONE relay serving every vault, not one per vault. Two relays would mean
// registering two MCP servers in every client, and every tool schema is in the
// agent's context on every request -- bounding list_notes cut that cost, and
// running two relays would hand it straight back.
func forVault(c *client.Client, name string) *client.Client {
	if name == "" {
		return c
	}
	return c.WithVault(name)
}

// ---- tool inputs and outputs ----------------------------------------------
//
// Schemas are inferred from these types, and the jsonschema tags become the
// property descriptions an agent reads. They are the tool's real documentation.

// listLimits bound what one list_notes call can return.
//
// MCP does not paginate tool results. Its cursor mechanism covers only
// resources/list, tools/list, prompts/list and resources/templates/list, so
// anything here is convention rather than protocol. The convention that holds
// up combines three things, and leaving any one out is a known failure:
//
//   - a server-enforced cap, because an unbounded default floods a context
//     window with one call
//   - total and has_more, so the caller knows what it did NOT see instead of
//     silently believing it saw everything
//   - a cursor, so full enumeration is still POSSIBLE; a cap without one just
//     makes the data unreachable
const (
	defaultListLimit = 100
	maxListLimit     = 500
)

type listInput struct {
	Vault     string `json:"vault,omitempty" jsonschema:"which vault to address. Omit to use the relay's default. Call list_vaults to see what this token opens"`
	Prefix    string `json:"prefix,omitempty" jsonschema:"only list paths under this, e.g. '2. Areas/'"`
	Recursive *bool  `json:"recursive,omitempty" jsonschema:"descend into subfolders. Default true. Set false to list one folder like ls, which is far cheaper on a large vault"`
	Limit     int    `json:"limit,omitempty" jsonschema:"how many entries to return, default 100, maximum 500"`
	Cursor    string `json:"cursor,omitempty" jsonschema:"opaque next_cursor from a previous call, to continue listing. Do not construct one"`
}

type noteSummary struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
	// Kind is "file" or "dir". Only non-recursive listings report "dir".
	Kind string `json:"kind,omitempty"`
	// Files counts what a directory contains, recursively. Zero for files.
	Files int `json:"files,omitempty"`
}

type listOutput struct {
	Count int           `json:"count"`
	Notes []noteSummary `json:"notes"`
	// Total is how many entries matched before the limit was applied.
	Total int `json:"total"`
	// HasMore says whether entries were withheld. Without it a truncated
	// listing is indistinguishable from a complete one.
	HasMore bool `json:"has_more"`
	// NextCursor continues the listing. Empty when there is nothing more.
	NextCursor string `json:"next_cursor,omitempty"`
	// Note tells an agent what to do about a truncated result, since it reads
	// prose more reliably than it infers from flags.
	Note string `json:"note,omitempty"`
}

func listNotes(c *client.Client) mcp.ToolHandlerFor[listInput, listOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in listInput) (*mcp.CallToolResult, listOutput, error) {
		c := forVault(c, in.Vault)
		limit := in.Limit
		if limit <= 0 {
			limit = defaultListLimit
		}
		if limit > maxListLimit {
			limit = maxListLimit
		}
		after, err := decodeCursor(in.Cursor)
		if err != nil {
			return nil, listOutput{}, err
		}

		files, err := c.List(ctx, in.Prefix)
		if err != nil {
			return nil, listOutput{}, err
		}

		recursive := in.Recursive == nil || *in.Recursive
		entries := flatEntries(files)
		if !recursive {
			entries = foldToOneLevel(files, in.Prefix)
		}

		// Cursor is a position in a stable sort, so a note added elsewhere
		// cannot shift the page under a caller mid-enumeration.
		sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })

		total := len(entries)
		if after != "" {
			idx := sort.Search(len(entries), func(i int) bool { return entries[i].Path > after })
			entries = entries[idx:]
		}

		out := listOutput{Notes: []noteSummary{}, Total: total}
		if len(entries) > limit {
			out.Notes = append(out.Notes, entries[:limit]...)
			out.HasMore = true
			out.NextCursor = encodeCursor(out.Notes[limit-1].Path)
			out.Note = fmt.Sprintf("showing %d of %d; pass next_cursor to continue, "+
				"or narrow with prefix, or use recursive=false to map folders first",
				limit, total)
		} else {
			out.Notes = append(out.Notes, entries...)
		}
		out.Count = len(out.Notes)
		return nil, out, nil
	}
}

// flatEntries is every file, the full recursive listing.
func flatEntries(files map[string]protocol.Entry) []noteSummary {
	out := make([]noteSummary, 0, len(files))
	for p, e := range files {
		out = append(out, noteSummary{Path: p, Size: e.Size, Kind: "file"})
	}
	return out
}

// foldToOneLevel collapses everything below the prefix into directory rows,
// the way ls shows a folder.
//
// This is the cheap path and usually the right one. A vault's breadth is small
// and stays small -- 19 top-level entries against 321 files here -- while its
// depth grows without limit, so listing one level costs a near-constant amount
// however large the vault becomes.
func foldToOneLevel(files map[string]protocol.Entry, prefix string) []noteSummary {
	type dirAcc struct {
		files int
		bytes int64
	}
	dirs := map[string]*dirAcc{}
	var out []noteSummary

	for p, e := range files {
		rest := strings.TrimPrefix(p, prefix)
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			name := prefix + rest[:i] + "/"
			d := dirs[name]
			if d == nil {
				d = &dirAcc{}
				dirs[name] = d
			}
			d.files++
			d.bytes += e.Size
			continue
		}
		out = append(out, noteSummary{Path: p, Size: e.Size, Kind: "file"})
	}
	for name, d := range dirs {
		out = append(out, noteSummary{Path: name, Kind: "dir", Files: d.files, Size: d.bytes})
	}
	return out
}

// Cursors are opaque by contract: clients must not parse or build one. Base64
// is the common convention and keeps that honest -- a caller reaching for
// arithmetic on a page number has to work at it.
func encodeCursor(after string) string {
	return base64.StdEncoding.EncodeToString([]byte(after))
}

func decodeCursor(cursor string) (string, error) {
	if cursor == "" {
		return "", nil
	}
	raw, err := base64.StdEncoding.DecodeString(cursor)
	if err != nil {
		// Never fall back to the first page. A caller that silently restarts
		// enumerates forever.
		return "", fmt.Errorf("invalid cursor: pass next_cursor from a previous " +
			"list_notes result unchanged, or omit it to start over")
	}
	return string(raw), nil
}

type folderInput struct {
	Vault string `json:"vault,omitempty" jsonschema:"which vault to address. Omit to use the relay's default. Call list_vaults to see what this token opens"`
	Depth int    `json:"depth,omitempty" jsonschema:"how many folder levels to report, default 2. Use 1 for the broadest map"`
}

type folderSummary struct {
	Path  string `json:"path"`
	Files int    `json:"files"`
	Bytes int64  `json:"bytes"`
}

type folderOutput struct {
	Folders    []folderSummary `json:"folders"`
	TotalFiles int             `json:"total_files"`
	TotalBytes int64           `json:"total_bytes"`
}

// listFolders answers "what is in this vault" without naming a single file.
//
// It exists because that question is asked far more often than "give me every
// path", and answering it with a full listing is what makes an agent's first
// call its most expensive one. Cost here scales with the number of FOLDERS,
// which stays small as a vault grows, rather than with the number of files,
// which does not.
func listFolders(c *client.Client) mcp.ToolHandlerFor[folderInput, folderOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in folderInput) (*mcp.CallToolResult, folderOutput, error) {
		c := forVault(c, in.Vault)
		depth := in.Depth
		if depth <= 0 {
			depth = 2
		}
		files, err := c.List(ctx, "")
		if err != nil {
			return nil, folderOutput{}, err
		}

		type acc struct {
			files int
			bytes int64
		}
		dirs := map[string]*acc{}
		out := folderOutput{Folders: []folderSummary{}}

		for p, e := range files {
			out.TotalFiles++
			out.TotalBytes += e.Size

			// Every ancestor of a file, capped at depth, gets credited with it,
			// so a parent's count includes what its children hold.
			parts := strings.Split(p, "/")
			for i := 1; i < len(parts) && i <= depth; i++ {
				name := strings.Join(parts[:i], "/") + "/"
				d := dirs[name]
				if d == nil {
					d = &acc{}
					dirs[name] = d
				}
				d.files++
				d.bytes += e.Size
			}
		}

		for name, d := range dirs {
			out.Folders = append(out.Folders, folderSummary{
				Path: name, Files: d.files, Bytes: d.bytes,
			})
		}
		sort.Slice(out.Folders, func(i, j int) bool {
			return out.Folders[i].Path < out.Folders[j].Path
		})
		return nil, out, nil
	}
}

type pathInput struct {
	Vault string `json:"vault,omitempty" jsonschema:"which vault to address. Omit to use the relay's default. Call list_vaults to see what this token opens"`
	Path  string `json:"path" jsonschema:"vault-relative path, e.g. 'notes/idea.md'"`
}

type readInput struct {
	Vault     string `json:"vault,omitempty" jsonschema:"which vault to address. Omit to use the relay's default. Call list_vaults to see what this token opens"`
	Path      string `json:"path" jsonschema:"vault-relative path, e.g. 'notes/idea.md'"`
	StartLine *int   `json:"start_line,omitempty" jsonschema:"one-based line to start at; valid only on the initial call without cursor"`
	Cursor    string `json:"cursor,omitempty" jsonschema:"next_cursor from the preceding page; mutually exclusive with start_line"`
	MaxChars  *int   `json:"max_chars,omitempty" jsonschema:"maximum Unicode code points to return, default 16000, maximum 100000"`
}

type readOutput struct {
	Path            string `json:"path"`
	Content         string `json:"content"`
	Revision        string `json:"revision,omitempty"`
	ContentRevision string `json:"content_revision"`
	HasMore         bool   `json:"has_more"`
	NextCursor      string `json:"next_cursor,omitempty"`
	Note            string `json:"note,omitempty"`
}

func readNote(c *client.Client) mcp.ToolHandlerFor[readInput, readOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in readInput) (*mcp.CallToolResult, readOutput, error) {
		c := forVault(c, in.Vault)
		if in.Path == "" {
			return nil, readOutput{}, fmt.Errorf("path is required")
		}
		body, base, err := c.ReadForEdit(ctx, in.Path)
		if err != nil {
			return nil, readOutput{}, err
		}
		if isBinary(body) {
			// An agent cannot use raw bytes and would waste a large amount of
			// context discovering that. Say so instead.
			return nil, readOutput{}, fmt.Errorf(
				"%s is a binary file (%d bytes); read_note only returns text. "+
					"Use read_attachment to get it as base64", in.Path, len(body))
		}
		out, err := pageReadNote(body, base, in)
		return nil, out, err
	}
}

func pageReadNote(body []byte, repositoryRevision string, in readInput) (readOutput, error) {
	if in.Cursor != "" && in.StartLine != nil {
		return readOutput{}, fmt.Errorf("cursor and start_line are mutually exclusive")
	}

	maxChars := defaultNoteChars
	if in.MaxChars != nil {
		maxChars = *in.MaxChars
		if maxChars <= 0 || maxChars > maxNoteChars {
			return readOutput{}, fmt.Errorf("max_chars must be between 1 and %d", maxNoteChars)
		}
	}

	note, err := validateNoteText(body)
	if err != nil {
		return readOutput{}, err
	}
	contentRevision := protocol.HashContent(body)

	var start uint32
	if in.Cursor != "" {
		cursor, err := decodeNoteCursor(in.Cursor)
		if err != nil {
			return readOutput{}, fmt.Errorf("invalid_cursor: %w", err)
		}
		if err := validateNoteCursor(cursor, contentRevision); err != nil {
			if errors.Is(err, errStaleCursor) {
				return readOutput{}, fmt.Errorf("stale_cursor: %w", err)
			}
			return readOutput{}, fmt.Errorf("invalid_cursor: %w", err)
		}
		start = cursor.offset
	} else {
		line := 1
		if in.StartLine != nil {
			line = *in.StartLine
		}
		start, err = offsetForLine(note, line)
		if err != nil {
			return readOutput{}, err
		}
	}

	page, err := pageNote(note, start, maxChars)
	if err != nil {
		if errors.Is(err, errInvalidCursor) {
			return readOutput{}, fmt.Errorf("invalid_cursor: %w", err)
		}
		return readOutput{}, err
	}
	out := readOutput{
		Path:            in.Path,
		Content:         page.content,
		Revision:        repositoryRevision,
		ContentRevision: contentRevision,
		HasMore:         page.hasMore,
	}
	if page.hasMore {
		out.NextCursor, err = encodeNoteCursor(contentRevision, page.nextOffset)
		if err != nil {
			return readOutput{}, fmt.Errorf("invalid_cursor: %w", err)
		}
		out.Note = "pass next_cursor as cursor to continue"
	}
	return out, nil
}

type writeInput struct {
	Vault   string `json:"vault,omitempty" jsonschema:"which vault to address. Omit to use the relay's default. Call list_vaults to see what this token opens"`
	Path    string `json:"path" jsonschema:"vault-relative path, e.g. 'notes/idea.md'"`
	Content string `json:"content" jsonschema:"the full new content of the note"`
	// Optimistic concurrency. Without this the write is a blind overwrite and a
	// concurrent edit is lost instead of merged, so an agent that read the note
	// before editing it MUST send the revision back.
	Revision string `json:"revision,omitempty" jsonschema:"the revision from read_note, if this content is an edit of what you read; omit only when creating or wholly replacing a note"`
}

type writeOutput struct {
	Path string `json:"path"`
	// Status is 'applied', 'merged', 'conflict' or 'refused'. Only 'applied'
	// means the stored content is exactly what was sent.
	Status       string `json:"status"`
	ConflictPath string `json:"conflictPath,omitempty"`
	Note         string `json:"note,omitempty"`

	// Current* carry the note as it now stands, sent only when the write did NOT
	// land exactly as given.
	//
	// This is deliberately NOT an automatic retry. write_note takes finished
	// content, not a transformation, so "re-read and write it again" is a blind
	// overwrite of whatever the other writer just did -- the precise failure the
	// base/revision machinery exists to prevent. A conflict means two writers
	// disagree about the same lines and no machine can pick between them.
	//
	// What is safe is removing the reason to fumble the retry: the caller gets
	// the current content and revision here, so re-applying its change is one
	// call instead of read_note followed by write_note, and it cannot
	// accidentally re-apply against a revision that has moved again in between.
	CurrentContent  string `json:"currentContent,omitempty"`
	CurrentRevision string `json:"currentRevision,omitempty"`
}

func writeNote(c *client.Client) mcp.ToolHandlerFor[writeInput, writeOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in writeInput) (*mcp.CallToolResult, writeOutput, error) {
		c := forVault(c, in.Vault)
		if in.Path == "" {
			return nil, writeOutput{}, fmt.Errorf("path is required")
		}
		res, err := c.WriteAt(ctx, in.Path, []byte(in.Content), in.Revision)
		if err != nil {
			return nil, writeOutput{}, err
		}
		out := writeOutput{Path: res.Path, Status: res.Status, ConflictPath: res.ConflictPath}

		// Anything other than a clean apply means the stored content is not what
		// was sent, so hand back what IS there. Best-effort: failing to fetch it
		// must not turn a reported conflict into a reported error, which would
		// lose the fact that the write was set aside safely.
		if res.Status != protocol.StatusApplied {
			if body, rev, err := c.ReadForEdit(ctx, in.Path); err == nil {
				out.CurrentContent, out.CurrentRevision = string(body), rev
			}
		}
		// Spell out the non-obvious outcomes: an agent that assumes its bytes
		// were stored will report a lie to the user.
		switch res.Status {
		case protocol.StatusMerged:
			out.Note = "another writer had changed this note; the two edits were merged, " +
				"so the stored content is not exactly what you sent. Re-read it before " +
				"editing again, and check the merge did what you intended."
		case protocol.StatusConflict:
			// Actionable, not just descriptive. The agent's edit is
			// reproducible and the human's half-finished typing is not, so the
			// agent is the side that should yield and redo -- and it should
			// clear up the file it caused rather than leaving it in someone's
			// vault.
			out.Note = "a person edited the same lines while you were writing. Your version " +
				"was NOT stored at " + res.Path + "; it was set aside at " + res.ConflictPath +
				". currentContent and currentRevision below are the note as it now stands. " +
				"Do this: apply your change to currentContent, then write_note again passing " +
				"currentRevision. Then delete_note " + res.ConflictPath + ", since your change " +
				"is now in the note. Do NOT send your original content unchanged and do NOT " +
				"copy the conflict file over the note: either would discard their edit."
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

// Attachment ceilings.
//
// Read is capped low because the bytes go into the caller's context: 1 MB is
// the same threshold search_notes already treats as "too big to be a note". A
// caller that genuinely wants more raises max_bytes and pays for it knowingly,
// up to a hard limit that exists so a 200 MB video cannot be requested at all.
//
// Write is capped higher because nothing is read into a context to produce it,
// but not unbounded: the base64 arrives as one JSON string in memory.
const (
	defaultAttachmentRead = 1 << 20  // 1 MB
	maxAttachmentRead     = 10 << 20 // 10 MB
	maxAttachmentWrite    = 25 << 20 // 25 MB
)

type readAttachmentInput struct {
	Vault    string `json:"vault,omitempty" jsonschema:"which vault to address. Omit to use the relay's default. Call list_vaults to see what this token opens"`
	Path     string `json:"path" jsonschema:"vault-relative path, e.g. 'attachments/diagram.png'"`
	MaxBytes int    `json:"max_bytes,omitempty" jsonschema:"refuse anything larger than this. Default 1048576 (1 MB), hard maximum 10485760 (10 MB). The content lands in your context, so raise it deliberately"`
}

type readAttachmentOutput struct {
	Path string `json:"path"`
	// ContentBase64 is the file's exact bytes, base64-encoded. Decode before
	// use; this is never a truncated prefix.
	ContentBase64 string `json:"content_base64"`
	Size          int64  `json:"size"`
	Ext           string `json:"ext,omitempty"`
	// Revision is the vault revision these bytes were read at. Pass it back as
	// write_attachment's `revision` to replace the file safely.
	Revision string `json:"revision,omitempty"`
}

func readAttachment(c *client.Client) mcp.ToolHandlerFor[readAttachmentInput, readAttachmentOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in readAttachmentInput) (*mcp.CallToolResult, readAttachmentOutput, error) {
		c := forVault(c, in.Vault)
		if in.Path == "" {
			return nil, readAttachmentOutput{}, fmt.Errorf("path is required")
		}

		limit := in.MaxBytes
		if limit <= 0 {
			limit = defaultAttachmentRead
		}
		if limit > maxAttachmentRead {
			return nil, readAttachmentOutput{}, fmt.Errorf(
				"max_bytes %d exceeds the hard limit of %d", limit, maxAttachmentRead)
		}

		body, base, err := c.ReadForEdit(ctx, in.Path)
		if err != nil {
			return nil, readAttachmentOutput{}, err
		}
		// Refuse rather than truncate. A caller that receives half a PNG has no
		// way to know, and every use of it is wrong.
		if len(body) > limit {
			return nil, readAttachmentOutput{}, fmt.Errorf(
				"%s is %d bytes, over the %d-byte limit; raise max_bytes (hard maximum %d) if you "+
					"really want it in context. To get the file WITHOUT spending context, fetch it "+
					"straight to disk instead: GET <relay>/file/%s with your usual Authorization "+
					"header streams the raw bytes, no base64 and no size cap",
				in.Path, len(body), limit, maxAttachmentRead, in.Path)
		}

		return nil, readAttachmentOutput{
			Path:          in.Path,
			ContentBase64: base64.StdEncoding.EncodeToString(body),
			Size:          int64(len(body)),
			Ext:           strings.ToLower(strings.TrimPrefix(path.Ext(in.Path), ".")),
			Revision:      base,
		}, nil
	}
}

type writeAttachmentInput struct {
	Vault         string `json:"vault,omitempty" jsonschema:"which vault to address. Omit to use the relay's default. Call list_vaults to see what this token opens"`
	Path          string `json:"path" jsonschema:"vault-relative path, e.g. 'attachments/diagram.png'"`
	ContentBase64 string `json:"content_base64" jsonschema:"the file's bytes, base64-encoded"`
	Revision      string `json:"revision,omitempty" jsonschema:"the revision read_attachment returned, if you are replacing a file you read. Without it the write is a blind overwrite"`
}

func writeAttachment(c *client.Client) mcp.ToolHandlerFor[writeAttachmentInput, writeOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in writeAttachmentInput) (*mcp.CallToolResult, writeOutput, error) {
		c := forVault(c, in.Vault)
		if in.Path == "" {
			return nil, writeOutput{}, fmt.Errorf("path is required")
		}
		body, err := base64.StdEncoding.DecodeString(in.ContentBase64)
		if err != nil {
			return nil, writeOutput{}, fmt.Errorf("content_base64 is not valid base64: %w", err)
		}
		if len(body) > maxAttachmentWrite {
			return nil, writeOutput{}, fmt.Errorf(
				"%s is %d bytes, over the %d-byte write limit", in.Path, len(body), maxAttachmentWrite)
		}
		// Type and content checks. Relay-only on purpose: the sync API must keep
		// accepting any bytes, because a paired device legitimately syncs plugin
		// JavaScript and themes. It is this door an injected instruction reaches.
		if err := guardAttachment(in.Path, body); err != nil {
			return nil, writeOutput{}, err
		}

		res, err := c.WriteAt(ctx, in.Path, body, in.Revision)
		if err != nil {
			return nil, writeOutput{}, err
		}
		out := writeOutput{Path: res.Path, Status: res.Status, ConflictPath: res.ConflictPath}
		// A binary file has no line-level merge, so anything but a clean apply
		// means the stored bytes are not the bytes sent. Say so; do not hand
		// back the current content, which would be base64 nobody asked for.
		if res.Status != protocol.StatusApplied {
			out.Note = "the stored file is NOT what you sent (status " + res.Status +
				"). Binary content cannot be merged; re-read it and decide."
		}
		return nil, out, nil
	}
}

type deleteInput struct {
	Vault    string `json:"vault,omitempty" jsonschema:"which vault to address. Omit to use the relay's default. Call list_vaults to see what this token opens"`
	Path     string `json:"path" jsonschema:"vault-relative path, e.g. 'notes/idea.md'"`
	Revision string `json:"revision,omitempty" jsonschema:"the revision from read_note, if you read the note before deciding to delete it"`
}

func deleteNote(c *client.Client) mcp.ToolHandlerFor[deleteInput, deleteOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in deleteInput) (*mcp.CallToolResult, deleteOutput, error) {
		c := forVault(c, in.Vault)
		if in.Path == "" {
			return nil, deleteOutput{}, fmt.Errorf("path is required")
		}
		res, err := c.DeleteAt(ctx, in.Path, in.Revision)
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

type moveInput struct {
	Vault    string `json:"vault,omitempty" jsonschema:"which vault to address. Omit to use the relay's default. Call list_vaults to see what this token opens"`
	From     string `json:"from" jsonschema:"current vault-relative path, e.g. 'Inbox/idea.md'"`
	To       string `json:"to" jsonschema:"new vault-relative path, e.g. '2. Areas/Tech/idea.md'. Folders are implied by the path and need no creating"`
	Revision string `json:"revision,omitempty" jsonschema:"the revision you read the source at, if you read it before deciding to move it"`
}

type moveOutput struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Status string `json:"status"`
	Note   string `json:"note,omitempty"`
}

func moveNote(c *client.Client) mcp.ToolHandlerFor[moveInput, moveOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in moveInput) (*mcp.CallToolResult, moveOutput, error) {
		c := forVault(c, in.Vault)
		if in.From == "" || in.To == "" {
			return nil, moveOutput{}, fmt.Errorf("from and to are both required")
		}
		if in.From == in.To {
			return nil, moveOutput{}, fmt.Errorf("from and to are the same path: %s", in.From)
		}

		// Refuse rather than overwrite. A move that silently replaced the
		// destination would destroy a file the caller never named, and the
		// caller cannot see what is at the destination from here.
		if existing, err := c.List(ctx, in.To); err == nil {
			if _, taken := existing[in.To]; taken {
				return nil, moveOutput{}, fmt.Errorf(
					"refusing to move onto %s: something is already there. Delete it first, or choose another destination", in.To)
			}
		}

		res, err := c.MoveAt(ctx, in.From, in.To, in.Revision)
		if err != nil {
			return nil, moveOutput{}, err
		}
		out := moveOutput{From: in.From, To: res.Path, Status: res.Status}
		if res.Status == protocol.StatusRefused {
			out.Note = "the server declined this move: " + res.Reason
		}
		return nil, out, nil
	}
}

type historyInput struct {
	Vault string `json:"vault,omitempty" jsonschema:"which vault to address. Omit to use the relay's default. Call list_vaults to see what this token opens"`
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
		c := forVault(c, in.Vault)
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
	Vault     string `json:"vault,omitempty" jsonschema:"which vault to address. Omit to use the relay's default. Call list_vaults to see what this token opens"`
	Path      string `json:"path" jsonschema:"vault-relative path"`
	Revision  string `json:"revision" jsonschema:"a revision from note_history, e.g. '4f3538ca'"`
	StartLine *int   `json:"start_line,omitempty" jsonschema:"one-based line to start at; valid only on the initial call without cursor"`
	Cursor    string `json:"cursor,omitempty" jsonschema:"next_cursor from the preceding page; mutually exclusive with start_line"`
	MaxChars  *int   `json:"max_chars,omitempty" jsonschema:"maximum Unicode code points to return, default 16000, maximum 100000"`
}

func readNoteAt(c *client.Client) mcp.ToolHandlerFor[readAtInput, readOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in readAtInput) (*mcp.CallToolResult, readOutput, error) {
		c := forVault(c, in.Vault)
		if in.Path == "" || in.Revision == "" {
			return nil, readOutput{}, fmt.Errorf("path and revision are both required")
		}
		body, err := c.ReadAt(ctx, in.Revision, in.Path)
		if err != nil {
			return nil, readOutput{}, err
		}
		if isBinary(body) {
			return nil, readOutput{}, fmt.Errorf(
				"%s is a binary file (%d bytes); read_note_at only returns text. "+
					"Use read_attachment to get the current file as base64", in.Path, len(body))
		}
		out, err := pageReadNote(body, in.Revision, readInput{
			Vault: in.Vault, Path: in.Path, StartLine: in.StartLine,
			Cursor: in.Cursor, MaxChars: in.MaxChars,
		})
		return nil, out, err
	}
}

type searchInput struct {
	Vault string `json:"vault,omitempty" jsonschema:"which vault to address. Omit to use the relay's default. Call list_vaults to see what this token opens"`
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
		c := forVault(c, in.Vault)
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

type listVaultsInput struct{}

type listVaultsOutput struct {
	Vaults  []string `json:"vaults"`
	Default string   `json:"default"`
}

func listVaults(c *client.Client) mcp.ToolHandlerFor[listVaultsInput, listVaultsOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, _ listVaultsInput) (*mcp.CallToolResult, listVaultsOutput, error) {
		names, err := c.ListVaults(ctx)
		if err != nil {
			return nil, listVaultsOutput{}, err
		}
		return nil, listVaultsOutput{Vaults: names, Default: c.Vault()}, nil
	}
}

type unlockInput struct {
	Vault string `json:"vault,omitempty" jsonschema:"which vault to unlock; defaults to the token's only vault"`
	Code  string `json:"code" jsonschema:"the six-digit code from the authenticator app"`
}

type unlockOutput struct {
	Vault     string `json:"vault"`
	ExpiresAt int64  `json:"expiresAt"`
}

// unlockVault spends a code the human handed over.
//
// The relay stores nothing: the grant lives on the server, keyed by the caller's
// own token, which is what keeps this surface stateless.
func unlockVault(c *client.Client) mcp.ToolHandlerFor[unlockInput, unlockOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in unlockInput) (*mcp.CallToolResult, unlockOutput, error) {
		vc := forVault(c, in.Vault)
		if strings.TrimSpace(in.Code) == "" {
			return nil, unlockOutput{}, fmt.Errorf("code is required")
		}
		until, err := vc.Unlock(ctx, in.Code)
		if err != nil {
			return nil, unlockOutput{}, err
		}
		return nil, unlockOutput{Vault: in.Vault, ExpiresAt: until}, nil
	}
}
