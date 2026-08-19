// Package protocol is the wire contract between an Archivist server and any
// client: the Obsidian plugin, the relay, or something someone else writes.
//
// It is a leaf package with no dependencies, deliberately. Every type here is
// serialised over HTTP, so a change to it is a change to the contract, and
// keeping it in one file with no imports makes that obvious.
//
// The rule for clients: branch on Code, never on message text. Messages are for
// humans and will be reworded.
package protocol

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"time"
)

// Version is the wire contract. It moves ONLY when a client must change --
// a removed field, a changed meaning, a new required parameter. Adding an
// optional field does not count, because clients ignore what they do not know.
//
//	1  head, snapshot, changes, have, content, push, events, history, at,
//	   check, export. changes and events share one Change shape.
const Version = 2

// Operations.
const (
	OpPut = "put"
	OpDel = "del"
	// OpMove renames a path. It carries From and no Hash: the content is
	// already on the server, which is the whole point -- a move preserves it
	// where a delete destroys it, so a move needs only the write scope.
	//
	// Without this op a rename arrived as del+put and a token with write but
	// not delete could not rename at all. Inferring it from a del+put pair with
	// a matching hash was the alternative, and it is a delete bypass: a caller
	// who can read a note knows its hash and could pair a delete of anything
	// with a put of that content elsewhere.
	OpMove = "move"
)

// File kinds. Kind is sniffed from content, never guessed from the extension,
// so a client can trust it when deciding what it is able to read.
const (
	KindText   = "text"
	KindBinary = "binary"
)

// HeaderVia names how a push reached the server. Set by the relay on a caller's
// behalf, because only it knows whether the request arrived over REST or MCP;
// anything pushing directly leaves it unset and is recorded as "api".
//
// It is a HINT, not a credential. A caller can set it to anything, exactly like
// Device -- which is why the token label sits beside it in the trailers and is
// the part that cannot be forged.
const HeaderVia = "X-Archivist-Via"

// Per-path outcomes of a push.
//
// Only StatusApplied means the server now holds exactly the bytes that were
// sent. Merged means it holds a three-way merge; Conflict means it kept its own
// and parked the client's copy; Refused means it did nothing. A client that
// records its own hash for anything but Applied ends up silently diverged.
const (
	StatusApplied  = "applied"
	StatusMerged   = "merged"
	StatusConflict = "conflict"
	StatusRefused  = "refused"
)

// Error codes. Stable identifiers; the accompanying message is not.
const (
	CodeUnauthorized   = "unauthorized"
	CodeUnknownBase    = "unknown_base"     // re-bootstrap from /v1/snapshot
	CodeInvalidPath    = "invalid_path"     // not local to the vault
	CodeMissingContent = "missing_content"  // referenced but never uploaded
	CodeHashMismatch   = "hash_mismatch"    // body does not hash to the address
	CodeTooLarge       = "too_large"        // beyond MaxUploadBytes
	CodeMalformed      = "malformed"        // unparseable request
	CodeNotFound       = "not_found"        // no such object, path or revision
	CodeDuplicatePath  = "duplicate_path"   // one path named twice in a push
	CodeForbidden      = "forbidden"        // valid token, but it does not open this vault
	CodeInternal       = "internal"         // the server's fault
	CodeQuarantined    = "path_quarantined" // repeated writes to one path
	CodeThrottled      = "throttled"        // vault-wide write rate exceeded
	CodeDiskLow        = "disk_low"         // free disk below the floor
)

// MaxUploadBytes bounds a single content upload. Beyond it the server answers
// 413 rather than truncating.
const MaxUploadBytes = 512 << 20

// MaxInlineChanges bounds how many paths one event carries. Past it the event
// is marked Truncated and the consumer reads /v1/changes instead.
const MaxInlineChanges = 100

// Entry is one file in a snapshot. The map key is the path, so there is no
// Path field here.
type Entry struct {
	Hash string `json:"hash"`
	Size int64  `json:"size"`
}

// Change is one path-level difference, used identically by /v1/changes and the
// event stream. That is deliberate: a consumer uses the stream to learn WHEN
// and /v1/changes to learn WHAT, so the two must be interchangeable.
type Change struct {
	Path string `json:"path"`
	Op   string `json:"op"`
	Hash string `json:"hash,omitempty"` // absent for OpDel and OpMove
	From string `json:"from,omitempty"` // OpMove only: the path being renamed
	Size int64  `json:"size"`
	Ext  string `json:"ext"`  // lowercase, no leading dot
	Kind string `json:"kind"` // KindText or KindBinary
}

// Result is what happened to one pushed change.
type Result struct {
	Path   string `json:"path"`
	Status string `json:"status"`
	// Hash is what the server now holds at Path. For StatusMerged it is the
	// merged content, which is NOT what the client sent -- without it a client
	// must fetch the whole snapshot to repair a single path.
	Hash         string `json:"hash,omitempty"`
	Size         int64  `json:"size,omitempty"`
	ConflictPath string `json:"conflictPath,omitempty"`
	ConflictHash string `json:"conflictHash,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

// Event is one commit, pushed over /v1/events.
// WaitResponse answers a long-poll. Changed is advisory: the caller still asks
// /v1/changes what actually moved, so a spurious true costs one wasted sync and
// a false negative is caught by the next poll.
type WaitResponse struct {
	Head    string `json:"head"`
	Changed bool   `json:"changed"`
}

type Event struct {
	Head      string    `json:"head"`
	Prev      string    `json:"prev,omitempty"`
	When      time.Time `json:"when,omitempty"`
	Count     int       `json:"count"`
	Truncated bool      `json:"truncated,omitempty"`
	Changes   []Change  `json:"changes"`
}

// Revision is one point in a path's history.
type Revision struct {
	Commit  string    `json:"commit"`
	Short   string    `json:"short"`
	When    time.Time `json:"when"`
	Message string    `json:"message"`
	Size    int64     `json:"size"`
	Hash    string    `json:"hash"`
	Deleted bool      `json:"deleted,omitempty"`
}

// ---- requests and responses ------------------------------------------------

type HeadResponse struct {
	Head string `json:"head"`
}

type SnapshotResponse struct {
	Head  string           `json:"head"`
	Files map[string]Entry `json:"files"`
}

type ChangesResponse struct {
	Head    string   `json:"head"`
	Entries []Change `json:"entries"`
}

type HaveRequest struct {
	Hashes []string `json:"hashes"`
}

type HaveResponse struct {
	Missing []string `json:"missing"`
}

type PushRequest struct {
	// Base is the commit this change set was computed against. Required: an
	// empty Base means "no common ancestor", which makes every existing path
	// look concurrently changed and forbids deletion.
	Base    string   `json:"base"`
	Device  string   `json:"device"`
	Changes []Change `json:"changes"`
}

type PushResponse struct {
	Head    string   `json:"head"`
	Results []Result `json:"results"`
}

type HistoryResponse struct {
	Path      string     `json:"path"`
	Revisions []Revision `json:"revisions"`
}

type CheckResponse struct {
	Head      string   `json:"head"`
	Files     int      `json:"files"`
	Missing   []string `json:"missing,omitempty"`
	Extra     []string `json:"extra,omitempty"`
	Differing []string `json:"differing,omitempty"`
	OK        bool     `json:"ok"`
}

type IndexResponse struct {
	Service   string     `json:"service"`
	Version   string     `json:"version"`  // build, changes constantly
	Protocol  int        `json:"protocol"` // contract, changes rarely -- check this
	Notes     []string   `json:"notes,omitempty"`
	Endpoints []Endpoint `json:"endpoints"`
	// Vault names which vault this server serves, so a client can tell two
	// servers apart. One vault per process makes it unambiguous.
	Vault string `json:"vault"`
}

type Endpoint struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Does   string `json:"does"`
}

type HealthResponse struct {
	Status   string `json:"status"`
	Version  string `json:"version"`
	Protocol int    `json:"protocol"`
}

// Error is the body of every non-2xx response.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type ErrorResponse struct {
	Error Error `json:"error"`
}

// Error lets a *Error travel as an ordinary Go error, so a layer that refuses
// a request can name its own wire code instead of leaving the HTTP layer to
// infer one by matching on prose. errors.As recovers the code intact.
func (e *Error) Error() string {
	if e.Message == "" {
		return e.Code
	}
	return e.Code + ": " + e.Message
}

// HashContent returns the git blob object id for content:
//
//	sha1("blob " + <byte length> + "\0" + content)
//
// It lives here, in the leaf package, so the server and every client compute
// addresses the same way. The server used to derive this from go-git's
// plumbing, which no client can import without pulling in 56 packages and a
// full git implementation -- the exact weight the separate relay binary exists
// to avoid.
//
// The header carries the BYTE length, not the character count. Getting that
// wrong only diverges on non-ASCII content, so the tests include an emoji, and
// every expected value is taken from `git hash-object --stdin`.
func HashContent(content []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(content))
	h.Write(content)
	return hex.EncodeToString(h.Sum(nil))
}
