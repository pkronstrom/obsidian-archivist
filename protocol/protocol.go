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
	"strconv"
	"strings"
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
	// OpMove renames a path. It carries From, and Hash as If-Match on the
	// source: the content is already on the server, which is the whole point
	// -- a move preserves it where a delete destroys it, so a move needs only
	// the write scope.
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

// HeaderClient names the calling program and its version: "plugin/0.21.0",
// "relay/v0.21.0". The server logs it, and can turn away a plugin older than
// it supports before that plugin reads or writes anything.
//
// Like HeaderVia it is a hint, not a credential. A caller that sends none is
// let through: curl sends none, and neither does a plugin from before 0.21.
// Turning THOSE away is what the protocol number is for.
const HeaderClient = "X-Archivist-Client"

// ClientPlugin is the program name the Obsidian plugin sends in HeaderClient.
const ClientPlugin = "plugin"

// MinPluginVersion is the oldest plugin this server accepts, "" for any.
//
// Raise it in the release where an older plugin would do harm rather than
// merely miss a feature -- write something this server would misread, say.
// For anything less, add to Features and let old plugins carry on.
const MinPluginVersion = ""

// Features are what this server can do beyond the base protocol, listed in
// the index. A client asks for the feature it needs and switches that feature
// off when it is missing, rather than comparing version strings. A server
// from before the list existed sends none, which reads as "has none of them".
const (
	FeatureEncryptedVaults = "encrypted-vaults"
	FeatureEncryptedFiles  = "encrypted-files"
)

// Features returns the list for the index.
func Features() []string {
	return []string{FeatureEncryptedVaults, FeatureEncryptedFiles}
}

// ClientOlderThan reports whether the HeaderClient value names program at a
// version below min. Anything it cannot read is not older: a missing header,
// another program, a dev build.
func ClientOlderThan(header, program, min string) bool {
	name, ver, ok := strings.Cut(header, "/")
	if !ok || name != program {
		return false
	}
	have, ok1 := parseVersion(ver)
	want, ok2 := parseVersion(min)
	if !ok1 || !ok2 {
		return false
	}
	for i := range have {
		if have[i] != want[i] {
			return have[i] < want[i]
		}
	}
	return false
}

// parseVersion reads "0.21.0" or "v0.21.0".
func parseVersion(s string) ([3]int, bool) {
	var v [3]int
	parts := strings.Split(strings.TrimPrefix(s, "v"), ".")
	if len(parts) != 3 {
		return v, false
	}
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return v, false
		}
		v[i] = n
	}
	return v, true
}

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
	CodeUnauthorized    = "unauthorized"
	CodeUnknownBase     = "unknown_base"     // re-bootstrap from /v1/snapshot
	CodeInvalidPath     = "invalid_path"     // not local to the vault
	CodeMissingContent  = "missing_content"  // referenced but never uploaded
	CodeHashMismatch    = "hash_mismatch"    // body does not hash to the address
	CodeTooLarge        = "too_large"        // beyond MaxUploadBytes
	CodeMalformed       = "malformed"        // unparseable request
	CodeNotFound        = "not_found"        // no such object, path or revision
	CodeDuplicatePath   = "duplicate_path"   // one path named twice in a push
	CodeForbidden       = "forbidden"        // valid token, but it does not open this vault
	CodeInternal        = "internal"         // the server's fault
	CodeQuarantined     = "path_quarantined" // repeated writes to one path
	CodeThrottled       = "throttled"        // vault-wide write rate exceeded
	CodeDiskLow         = "disk_low"         // free disk below the floor
	CodeNotText         = "not_text"
	CodeStale           = "stale"
	CodeNoMatch         = "no_match"
	CodeMultipleMatches = "multiple_matches"
	// CodeStaleHead is a precondition failure: the caller named a head that is
	// no longer the tip. Distinct from a conflict inside a push, because the
	// fix is different -- flush, re-read head, retry once -- and a client that
	// cannot tell them apart cannot recover automatically.
	CodeStaleHead = "stale_head"
	// CodeStepUpRequired is a valid token that must present a one-time code
	// first. Distinct from forbidden so a client can turn it into a request for
	// a code rather than reporting a permanent refusal.
	CodeStepUpRequired = "step_up_required"
	// CodePlaintextRefused is content without the envelope, sent to an
	// encrypted vault. Distinct from malformed because the request was
	// well-formed: the client is one that does not encrypt.
	CodePlaintextRefused = "plaintext_refused"
	// CodeNoKey is an encrypted vault whose key check has not been installed.
	// Content is refused until it is, so no ciphertext can exist under a key
	// nobody recorded.
	CodeNoKey = "no_key"
	// CodeKeyCheckSet is a second attempt to install a key check. It is
	// write-once: two devices pairing at the same moment must not both
	// believe they generated the vault's key.
	CodeKeyCheckSet = "key_check_set"
	// CodeEncryptedVault is a route that reads or writes plaintext on the
	// server, called on a vault whose content the server cannot read.
	CodeEncryptedVault = "encrypted_vault"
	// CodeEncryptedPath is the same refusal for one path: a file marked
	// .encrypted in a vault that is otherwise ordinary.
	CodeEncryptedPath = "encrypted_path"
	// CodeClientTooOld is a plugin below MinPluginVersion. Nothing was read
	// or written; the message says which version is needed.
	CodeClientTooOld = "client_too_old"
)

// Envelope is the literal prefix of every object in an encrypted vault.
//
// The server holds no key and never parses past this. It exists so the server
// can refuse plaintext structurally, without guessing from entropy -- a
// heuristic that fails open on short content.
const Envelope = "archivist-vault/v1\n"

// ValidEncryptedPath reports whether every segment of path is lowercase
// base32, which is all an encrypted name can be.
//
// The server's usual path policy matches on plaintext names and never fires on
// encrypted ones, so in an encrypted vault this allowlist is the path policy.
// It is what stops a client that forgot to encrypt a name from writing a
// readable one into history.
func ValidEncryptedPath(path string) bool {
	if path == "" {
		return false
	}
	run := 0
	for i := 0; i < len(path); i++ {
		c := path[i]
		switch {
		case c == '/':
			if run == 0 {
				return false
			}
			run = 0
		case (c >= 'a' && c <= 'z') || (c >= '2' && c <= '7'):
			run++
		default:
			return false
		}
	}
	return run > 0
}

// EncryptedMarker marks a file or folder of an ordinary vault as encrypted:
// the server stores it and cannot read it.
//
// Fixed here and in src/crypt.ts, deliberately not a setting. A rule kept in
// the vault could be edited by anything that can write a note.
const EncryptedMarker = ".encrypted"

// EncryptedPath reports whether a path of an ORDINARY vault is marked
// encrypted. In an encrypted vault every path is, and this is not consulted.
//
// The grammar is LocalOnly's, for LocalOnly's reason. Any segment ending in
// the marker takes its whole subtree with it; the last segment is also marked
// when the marker sits before its extension. A folder has to END in the
// marker, because "project.encrypted.assets" would match the file spelling
// by coincidence.
//
// It gives the same answer for the path a device sees and the path the server
// stores: the two are identical up to and including the first marked segment,
// and that segment decides.
func EncryptedPath(path string) bool {
	_, ok := markedAt(path)
	return ok
}

// ValidMarkedPath reports whether a marked path is one the server may store:
// everything beneath a marked folder must be an encrypted name. A marked file
// has nothing beneath it. False for a path that is not marked at all.
func ValidMarkedPath(path string) bool {
	end, ok := markedAt(path)
	if !ok {
		return false
	}
	if end == len(path) {
		return true
	}
	return ValidEncryptedPath(path[end+1:])
}

// markedAt returns the offset just past the first marked segment.
func markedAt(path string) (end int, ok bool) {
	start := 0
	for i := 0; i <= len(path); i++ {
		if i < len(path) && path[i] != '/' {
			continue
		}
		seg := path[start:i]
		if endsMarked(seg) {
			return i, true
		}
		if i == len(path) && markedFile(seg) {
			return i, true
		}
		start = i + 1
	}
	return 0, false
}

// markedFile is the file spelling: name.encrypted.ext, with a name and
// exactly one extension after the marker.
func markedFile(seg string) bool {
	dot := -1
	for i := len(seg) - 1; i >= 0; i-- {
		if seg[i] == '.' {
			dot = i
			break
		}
	}
	if dot <= 0 || dot == len(seg)-1 {
		return false
	}
	return endsMarked(seg[:dot])
}

// endsMarked reports whether s ends in the marker and has a name before it.
//
// Case is ignored, and that is a safety property rather than a convenience.
// A marker that fails to match does not stop a file syncing, it sends the
// file readable: "Clients.Encrypted", from a phone keyboard that capitalised
// it, would otherwise be published in plaintext by a rule the user believed
// they had invoked.
func endsMarked(s string) bool {
	if len(s) <= len(EncryptedMarker) {
		return false
	}
	tail := s[len(s)-len(EncryptedMarker):]
	for i := 0; i < len(tail); i++ {
		c := tail[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != EncryptedMarker[i] {
			return false
		}
	}
	return true
}

// ValidKeyCheck reports whether s is a key check: 32 bytes as lowercase hex.
func ValidKeyCheck(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// HasEnvelope reports whether content begins with Envelope.
func HasEnvelope(content []byte) bool {
	return len(content) >= len(Envelope) && string(content[:len(Envelope)]) == Envelope
}

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
	Hash string `json:"hash,omitempty"` // absent for OpDel; for OpMove, what the source must hold
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
	// Device is who made the change, as the commit recorded it: a device name
	// for a client push, "server" for an edit made in the vault directory.
	// Empty when the message predates the convention.
	Device string `json:"device,omitempty"`
	// Via names the intermediary when one was involved -- the relay, an agent.
	// Absent for an ordinary device push.
	Via string `json:"via,omitempty"`
	// Added and Removed count lines against the previous revision of this path.
	// Both zero for a binary file, a file too large to diff, or a first
	// revision, which is why Created distinguishes "nothing changed" from
	// "nothing to compare".
	Added   int  `json:"added,omitempty"`
	Removed int  `json:"removed,omitempty"`
	Created bool `json:"created,omitempty"`
}

// DeletedPath is a path that history holds but HEAD does not.
type DeletedPath struct {
	Path string    `json:"path"`
	When time.Time `json:"when"`
	// Revision is the commit whose tree still HOLDS the content: the parent of
	// the deletion. Pass it to restore.
	Revision string `json:"revision"`
	Short    string `json:"short"`
	// DeletedIn is the commit that removed it, for context only.
	DeletedIn string `json:"deletedIn"`
	Device    string `json:"device,omitempty"`
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

type AppendNoteRequest struct {
	Path            string `json:"path"`
	Content         string `json:"content"`
	ContentRevision string `json:"content_revision,omitempty"`
	Device          string `json:"device,omitempty"`
}

type EditNoteRequest struct {
	Path            string `json:"path"`
	ContentRevision string `json:"content_revision,omitempty"`
	OldText         string `json:"old_text"`
	NewText         string `json:"new_text"`
	Device          string `json:"device,omitempty"`
}

type NoteMutationResponse struct {
	Path            string `json:"path"`
	Status          string `json:"status"`
	Revision        string `json:"revision"`
	ContentRevision string `json:"content_revision"`
}

type HistoryResponse struct {
	Path      string     `json:"path"`
	Revisions []Revision `json:"revisions"`
	// HasMore reports that older revisions exist beyond this page. Without it
	// a caller cannot distinguish "that is all of them" from "the limit cut
	// you off", which is the difference between a complete revision list and a
	// silently truncated one.
	HasMore bool `json:"hasMore"`
	// Next is the cursor for the following page: pass it back as ?before=.
	// Empty when HasMore is false.
	Next string `json:"next,omitempty"`
}

type CheckResponse struct {
	Head      string   `json:"head"`
	Files     int      `json:"files"`
	Missing   []string `json:"missing,omitempty"`
	Extra     []string `json:"extra,omitempty"`
	Differing []string `json:"differing,omitempty"`
	// Stranded lists paths tracked in HEAD that today's rules exclude. They
	// cannot be edited or deleted through sync, so they need an explicit
	// removal commit. Distinct from Extra, which is the harmless opposite.
	Stranded []string `json:"stranded,omitempty"`
	// LocalOnly counts files excluded by the .local rule. Not drift -- they
	// are excluded on purpose -- but reported so they cannot pile up unseen.
	LocalOnly int  `json:"localOnly,omitempty"`
	OK        bool `json:"ok"`
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
