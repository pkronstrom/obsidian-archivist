// Package reconcile applies change sets to the vault and decides what happens
// when two devices touched the same file.
//
// This is the smallest package that matters most. Everything dangerous in a
// sync engine lives here: distinguishing a deletion from an absence, choosing
// a merge base, and deciding when not to merge at all.
//
// Two entry points, one for each ingress:
//
//	Push  a client change set, which carries a base and can therefore be merged
//	Scan  whatever is on disk now, which carries no base and is last-writer-wins
package reconcile

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/pkronstrom/obsidian-archivist/internal/guard"
	"github.com/pkronstrom/obsidian-archivist/internal/merge"
	"github.com/pkronstrom/obsidian-archivist/internal/repo"
	"github.com/pkronstrom/obsidian-archivist/internal/vault"
	"github.com/pkronstrom/obsidian-archivist/protocol"
)

// Statuses and wire shapes come from the protocol package: one definition, so
// the server and any client cannot disagree about them.
const (
	StatusApplied  = protocol.StatusApplied
	StatusMerged   = protocol.StatusMerged
	StatusConflict = protocol.StatusConflict
	StatusRefused  = protocol.StatusRefused
)

type Change = protocol.Change

type Result = protocol.Result

type Reconciler struct {
	// mu serialises the two ingress paths. Both mutate the working tree and
	// commit; interleaving them would produce commits that do not match the
	// tree they claim to describe.
	mu sync.Mutex
	v  *vault.Vault
	r  *repo.Repo

	// written records content this process just wrote, so the filesystem
	// watcher can recognise its own echo instead of treating it as a new edit.
	//
	// Entries expire. Suppression is only an optimisation -- a missed one costs
	// a redundant no-op commit, nothing more -- whereas an unbounded map grows
	// for the process lifetime whenever no matching event arrives, which is
	// guaranteed with -watch=false and possible any time an event is dropped.
	echoMu  sync.Mutex
	written map[string]writeRecord

	events *broadcaster

	// normalizeNFC canonicalises incoming paths. Set once at startup.
	normalizeNFC bool

	// g bounds how fast content may enter. Nil disables every guard, which is
	// what the CLI and most tests want.
	g *guard.Guard
	// notifyTrip reports a refused write. Nil is a no-op.
	notifyTrip func(code, path, reason string)
}

// echoTTL is how long a write stays suppressible. Comfortably longer than any
// sane debounce, far shorter than a process lifetime.
const echoTTL = 60 * time.Second

type writeRecord struct {
	hash string
	at   time.Time
}

func New(v *vault.Vault, r *repo.Repo) *Reconciler {
	return &Reconciler{v: v, r: r, written: map[string]writeRecord{}, events: newBroadcaster()}
}

func (rc *Reconciler) noteWrite(path string, content []byte) {
	rc.echoMu.Lock()
	defer rc.echoMu.Unlock()
	now := time.Now()
	rc.written[path] = writeRecord{hash: vault.Hash(content), at: now}
	// Opportunistic sweep: cheap, and it bounds the map without a goroutine.
	for p, rec := range rc.written {
		if now.Sub(rec.at) > echoTTL {
			delete(rc.written, p)
		}
	}
}

// WasOurWrite reports whether the content now at path is exactly what this
// process last wrote there, and consumes the record.
func (rc *Reconciler) WasOurWrite(path string, content []byte) bool {
	rc.echoMu.Lock()
	defer rc.echoMu.Unlock()
	rec, ok := rc.written[path]
	if !ok {
		return false
	}
	if time.Since(rec.at) > echoTTL {
		delete(rc.written, path)
		return false
	}
	if rec.hash == vault.Hash(content) {
		delete(rc.written, path)
		return true
	}
	return false
}

// Push applies a client's change set, which was computed against base.
// Origin is who pushed, and how it reached us.
//
// Device is client-supplied and therefore forgeable -- it is a label the sender
// chose. Token is not: the server resolved it from the credential that was
// actually presented, so it is the only part of a commit's provenance that
// cannot be lied about. Via records the road taken, which the relay sets on the
// caller's behalf because only it knows whether a request came in over REST or
// MCP.
type Origin struct {
	Device string
	Token  string
	Via    string
}

// message renders the commit subject and its provenance trailers.
//
// Trailers rather than a longer subject: the subject is what `git log --oneline`
// and every listing shows, and conflict filenames are already long enough
// without provenance in them too. An origin with nothing verified produces the
// bare subject, which is honest -- claiming a token we did not resolve would be
// worse than omitting it.
func (o Origin) message(prefix string) string {
	// Device is caller-controlled, exactly like Via. Left raw, a value carrying
	// "\n\nToken: admin" forges a trailer beside the server-resolved one and
	// defeats the only part of this that was supposed to be unforgeable.
	msg := prefix + " " + sanitiseTrailerValue(o.Device)
	var trailers []string
	if o.Token != "" {
		// The label comes from the tokens file, which only the operator writes,
		// but it costs nothing to hold every trailer to one rule.
		trailers = append(trailers, "Token: "+sanitiseTrailerValue(o.Token))
	}
	if o.Via != "" {
		trailers = append(trailers, "Via: "+sanitiseTrailerValue(o.Via))
	}
	if len(trailers) == 0 {
		return msg
	}
	return msg + "\n\n" + strings.Join(trailers, "\n") + "\n"
}

// sanitiseTrailerValue strips what would forge a trailer and bounds the rest.
// Applied to every value that reaches a commit message, because a value that is
// safe today acquires a new source tomorrow.
func sanitiseTrailerValue(s string) string {
	// Newlines would start a new trailer; a colon would make the remainder look
	// like a trailer key on whatever line it lands on. Dropping both leaves a
	// value that cannot be mistaken for structure no matter where it is placed.
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ':' {
			return -1
		}
		return r
	}, s)
	if len(s) > 64 {
		s = s[:64]
	}
	return strings.TrimSpace(s)
}

// Push applies a change set with no verified origin. Kept for the many callers
// -- tests, mostly -- that have no principal to name.
func (rc *Reconciler) Push(base, device string, changes []Change) (string, []Result, error) {
	return rc.PushWithOrigin(base, Origin{Device: device}, changes)
}

// PushWithOrigin applies a change set and records who sent it.
func (rc *Reconciler) PushWithOrigin(base string, origin Origin, changes []Change) (string, []Result, error) {
	device := origin.Device

	rc.mu.Lock()
	defer rc.mu.Unlock()

	// Canonicalise incoming paths when normalisation is on.
	//
	// Without this, a Mac pushing the decomposed spelling of a name whose
	// composed spelling is already in the vault creates a SECOND file for the
	// same note. Normalising here means every writer converges on one path,
	// which is the whole point -- doing it only on disk at startup would leave
	// the next push to undo it.
	if rc.normalizeNFC {
		for i := range changes {
			changes[i].Path = vault.ToNFC(changes[i].Path)
		}
	}

	// Guard every path before anything is applied, so a refused push leaves
	// the tree untouched rather than half-written.
	for _, ch := range changes {
		if err := rc.admit(ch.Path, changeSize(rc.r, ch)); err != nil {
			return "", nil, err
		}
	}

	head, err := rc.r.Head()
	if err != nil {
		return "", nil, err
	}

	// Which paths moved on the server since the client's base? Only these can
	// possibly conflict; everything else applies untouched, which is the
	// common case of two devices editing different notes.
	moved := map[string]bool{}
	if base != head {
		serverChanges, err := rc.r.Changes(base, head)
		if err != nil {
			return "", nil, err
		}
		for _, ch := range serverChanges {
			moved[ch.Path] = true
		}
	}

	// Validate EVERYTHING before touching the working tree.
	//
	// applyOne mutates as it goes, so a bad path or a missing blob halfway
	// through used to leave earlier entries written but uncommitted -- and the
	// caller was told the push failed. A later watcher scan would then commit
	// exactly the changes the client believes were rejected.
	seen := make(map[string]struct{}, len(changes))
	for _, ch := range changes {
		if err := vault.ValidPath(ch.Path); err != nil {
			return "", nil, err
		}
		if _, dup := seen[ch.Path]; dup {
			return "", nil, fmt.Errorf("reconcile: %q appears twice in one push", ch.Path)
		}
		seen[ch.Path] = struct{}{}
		switch ch.Op {
		case "del":
		case "move":
			// From is validated here so a malformed one fails the whole push
			// before anything is written, like every other path check.
			if ch.From == "" {
				return "", nil, fmt.Errorf("reconcile: move of %s names no source", ch.Path)
			}
			if err := vault.ValidPath(ch.From); err != nil {
				return "", nil, err
			}
			if ch.From == ch.Path {
				return "", nil, fmt.Errorf("reconcile: move of %s onto itself", ch.Path)
			}
		case "put":
			if !rc.r.HasBlob(ch.Hash) {
				return "", nil, fmt.Errorf(
					"reconcile: content %s for %s was never uploaded", ch.Hash, ch.Path)
			}
		default:
			return "", nil, fmt.Errorf("reconcile: unknown op %q for %s", ch.Op, ch.Path)
		}
	}

	results := make([]Result, 0, len(changes))
	for _, ch := range changes {
		res, err := rc.applyOne(base, device, head, moved, ch)
		if err != nil {
			// Everything was validated above, so reaching here means an I/O
			// failure mid-batch. Commit what landed rather than leaving the
			// tree dirty and unrecorded: the watcher would commit it anyway,
			// and an unrecorded change is worse than a recorded partial one.
			if _, cerr := rc.r.Commit(origin.message("partial push from")); cerr != nil {
				return "", nil, fmt.Errorf("%w (and the partial state could not be committed: %v)", err, cerr)
			}
			return "", nil, err
		}
		results = append(results, res)
	}

	newHead, err := rc.r.Commit(origin.message("sync from"))
	if err != nil {
		return "", nil, err
	}
	rc.notify(head, newHead)
	return newHead, results, nil
}

func (rc *Reconciler) applyOne(base, device, head string, moved map[string]bool, ch Change) (Result, error) {
	res := Result{Path: ch.Path, Status: StatusApplied}

	// Paths were validated before any mutation began; this is belt and braces.
	if err := vault.ValidPath(ch.Path); err != nil {
		return res, err
	}
	if vault.Skip(ch.Path) {
		res.Status, res.Reason = StatusRefused, "excluded from sync"
		return res, nil
	}

	switch ch.Op {
	case "del":
		// A client with no base has no idea what exists on the server, so any
		// deletion it implies is unfounded. Refusing here is what stops a
		// freshly-bootstrapped device from wiping the vault -- the exact bug
		// Self-hosted LiveSync shipped in 1.0.12.
		if base == "" {
			res.Status, res.Reason = StatusRefused, "a client with no base may not delete"
			return res, nil
		}
		if moved[ch.Path] {
			// Deleted here, changed there. Keep the server's version: a
			// deletion is cheap to repeat and impossible to undo.
			res.Status, res.Reason = StatusRefused, "changed on the server since base"
			return res, nil
		}
		if err := rc.v.Remove(ch.Path); err != nil {
			return res, err
		}
		return res, nil

	case "put":
		content, err := rc.r.ReadBlob(ch.Hash)
		if err != nil {
			return res, fmt.Errorf("reconcile: content %s for %s was never uploaded: %w",
				ch.Hash, ch.Path, err)
		}
		if !moved[ch.Path] {
			res = rc.resultFor(res, content)
			return res, rc.write(ch.Path, content)
		}
		return rc.resolve(base, device, ch.Path, content)

	case "move":
		// A move is a WRITE, not a delete, and the difference is that content
		// survives: it exists at the new path when this returns. That is what
		// lets a token with write but not delete rename a note.
		//
		// It is also why the op is explicit rather than inferred from a del+put
		// pair with a matching hash. A caller who can read a note knows its
		// hash, so the inference would let them pair a delete of anything with
		// a put of that content elsewhere -- a delete bypass wearing a rename.
		if base == "" {
			// Same reasoning as delete: a client with no base does not know
			// what exists, so it cannot be trusted to say what should move.
			res.Status, res.Reason = StatusRefused, "a client with no base may not move"
			return res, nil
		}
		if err := vault.ValidPath(ch.From); err != nil {
			return res, err
		}
		// The SOURCE needs the exclusion check too, not just the destination.
		// Without it a write-scoped caller could move .obsidian/secrets.json
		// into a synced path -- publishing a file the vault deliberately does
		// not sync, and deleting the original on the way.
		if vault.Skip(ch.From) {
			res.Status, res.Reason = StatusRefused, "excluded from sync: "+ch.From
			return res, nil
		}
		if moved[ch.From] || moved[ch.Path] {
			res.Status, res.Reason = StatusRefused, "changed on the server since base"
			return res, nil
		}
		content, err := rc.v.Read(ch.From)
		if err != nil {
			// Refusing beats inventing. A move whose source is gone would
			// otherwise be a way to name any path and have it disappear.
			res.Status, res.Reason = StatusRefused, "no such path: "+ch.From
			return res, nil
		}
		// Hash on a move is what the client believes the source holds. It is
		// If-Match for a rename, and without it a retried move silently
		// diverges: a move refused because the source changed remotely is
		// re-sent on the next cycle with a fresh base, so the staleness check
		// above no longer fires, and the SERVER's newer bytes get carried to
		// the new path while the client records its own older snapshot there.
		if ch.Hash != "" && vault.Hash(content) != ch.Hash {
			res.Status, res.Reason = StatusRefused, "the source changed since it was read: "+ch.From
			return res, nil
		}
		if _, err := rc.v.Read(ch.Path); err == nil {
			// Overwriting would destroy the target's content, which is the one
			// thing a move must not do.
			res.Status, res.Reason = StatusRefused, "target already exists: "+ch.Path
			return res, nil
		}
		if err := rc.write(ch.Path, content); err != nil {
			return res, err
		}
		if err := rc.v.Remove(ch.From); err != nil {
			return res, err
		}
		res = rc.resultFor(res, content)
		return res, nil

	default:
		res.Status, res.Reason = StatusRefused, "unknown op "+ch.Op
		return res, nil
	}
}

// resolve handles a path that both sides changed.
func (rc *Reconciler) resolve(base, device, path string, theirs []byte) (Result, error) {
	res := Result{Path: path}

	ours, readErr := rc.v.Read(path)
	if readErr != nil {
		// The server deleted it while the client edited it. Keep the edit --
		// content is recoverable, an absence is not.
		res.Status = StatusApplied
		res = rc.resultFor(res, theirs)
		return res, rc.write(path, theirs)
	}

	// An inventory describes one installation at one point in time. Never
	// combine fields from different scans, even when diff3 could merge cleanly.
	// The incoming whole file wins; the previous version stays in Git history.
	if vault.PluginInventoryPath(path) {
		res.Status = StatusApplied
		res = rc.resultFor(res, theirs)
		return res, rc.write(path, theirs)
	}

	// Binary never merges. A merged binary is corrupt, and conflict markers in
	// a PNG are worse than either version alone.
	if merge.IsBinary(ours) || merge.IsBinary(theirs) {
		return rc.keepBoth(device, path, theirs, nil)
	}

	baseContent, err := rc.r.ReadAt(base, path)
	if err != nil {
		// No common ancestor for this path -- both sides created it
		// independently. There is nothing to merge against, so there is nothing
		// to mark up either.
		return rc.keepBoth(device, path, theirs, nil)
	}

	merged, conflict, err := merge.MergeLabelled(baseContent, ours, theirs, "server", device)
	if err != nil {
		return res, err
	}
	if conflict {
		// Hand the marked-up merge to the conflict file. It shows both sides
		// against their common ancestor, which the raw losing version alone
		// cannot: with only one side you have to diff it against the note by
		// hand to find out what actually differs.
		return rc.keepBoth(device, path, theirs, merged)
	}
	res.Status = StatusMerged
	res = rc.resultFor(res, merged)
	return res, rc.write(path, merged)
}

// keepBoth leaves the server's version in place and parks the client's copy
// beside it. Deliberately the dumbest possible resolution: a conflict file is
// an ordinary note, so it syncs everywhere and can be resolved on a phone by
// editing and deleting it.
//
// marked is the three-way merge carrying git-style conflict markers, when one
// could be produced. It goes in the conflict file rather than the note itself:
// the note stays clean and readable, and markers only ever appear in a file
// whose name says it needs attention. Markers in the note would render as
// literal text in every preview on every device until someone resolved them.
//
// marked is nil for binaries and for paths with no common ancestor, where there
// is no meaningful three-way result. Then the client's raw version is written,
// as before.
func (rc *Reconciler) keepBoth(device, path string, theirs, marked []byte) (Result, error) {
	body := theirs
	if marked != nil {
		body = fenceConflict(marked, device)
	}
	cp := conflictPath(path, device, theirs)
	res := Result{Path: path, Status: StatusConflict, ConflictPath: cp}
	// Both hashes: what the server kept at the real path, and where the
	// client's version went. A client can then repair itself with two content
	// fetches rather than a whole-snapshot download.
	if ours, err := rc.v.Read(path); err == nil {
		res = rc.resultFor(res, ours)
	}
	// The hash describes what is AT ConflictPath, so a client can fetch exactly
	// what it will find there. The client's own unmarked version is still in the
	// object store -- it uploaded it before pushing -- so nothing is unreachable.
	if h, err := repo.HashContent(body); err == nil {
		res.ConflictHash = h
	}
	return res, rc.write(cp, body)
}

// fenceConflict wraps a marked-up merge in a code fence.
//
// Git's markers are markdown syntax: ">>>>>>> device" at the start of a line is
// a blockquote, and "=======" makes the line above it a heading. Unfenced, a
// conflict renders as nested quote bars and phantom headings in every preview on
// every device -- which is precisely what makes conflicts unpleasant to resolve
// in the Obsidian Git plugin. Inside a fence everything is literal, so it reads
// the same on a phone as in source mode.
//
// Obsidian's own Sync writes a plain copy of the other version and no markers at
// all. That renders cleanly but tells you nothing about what differs; you diff
// two files by hand. The fence keeps both sides visible AND renders correctly.
func fenceConflict(marked []byte, device string) []byte {
	fence := strings.Repeat("`", longestBacktickRun(marked)+1)
	if len(fence) < 3 {
		fence = "```"
	}
	var b strings.Builder
	b.WriteString("Conflict: `server` and `" + device + "` changed the same lines.\n")
	b.WriteString("Edit below, then delete the fence and the marker lines.\n\n")
	b.WriteString(fence + "text\n")
	b.Write(marked)
	if len(marked) > 0 && marked[len(marked)-1] != '\n' {
		b.WriteString("\n")
	}
	b.WriteString(fence + "\n")
	return []byte(b.String())
}

// longestBacktickRun finds the longest run of backticks in the content, so the
// wrapping fence can be made longer than anything inside it. A note containing
// its own code block would otherwise terminate the fence early and the rest
// would render as markdown again.
func longestBacktickRun(b []byte) int {
	longest, run := 0, 0
	for _, c := range b {
		if c == '`' {
			run++
			if run > longest {
				longest = run
			}
		} else {
			run = 0
		}
	}
	return longest
}

func (rc *Reconciler) write(path string, content []byte) error {
	if err := rc.v.Write(path, content); err != nil {
		return err
	}
	rc.noteWrite(path, content)
	return nil
}

// resultFor fills in what the server now holds at a path. Without it a client
// that got StatusMerged knows only that its bytes were not kept -- not what
// replaced them -- and must fetch the whole snapshot to repair one file.
func (rc *Reconciler) resultFor(res Result, content []byte) Result {
	h, err := repo.HashContent(content)
	if err == nil {
		res.Hash = h
	}
	res.Size = int64(len(content))
	return res
}

// conflictPath keeps the original extension so editors still recognise the
// file: notes/idea.md -> notes/idea.conflict-mac-a1b2c3.md
func conflictPath(path, device string, content []byte) string {
	// Device and second alone are not unique: two conflicts on the same path
	// from the same relay inside one second would collide, and the second copy
	// would overwrite the first -- breaking the keep-both promise. A short
	// content fragment makes identical content collapse (which is correct) and
	// different content diverge (which is the point).
	// No timestamp. It used to carry one -- .conflict-mac-20260816T093012-a1b2c3
	// -- which made an already-awkward filename twice as long for information
	// git already holds: the commit that created the file is timestamped, and
	// `history` reports it. The content fragment alone gives what the NAME
	// needs, which is uniqueness: identical content collapses onto one file
	// (correct) and different content diverges (the point).
	// Twelve hex characters, not six. Six is 24 bits, and the promise this
	// filename makes is that two different versions both survive -- a collision
	// breaks exactly that, by overwriting the earlier file. Still far shorter
	// than the timestamp this replaced.
	frag := vault.Hash(content)[7:19]
	dot := strings.LastIndex(path, ".")
	slash := strings.LastIndex(path, "/")
	suffix := fmt.Sprintf(".conflict-%s-%s", sanitise(device), frag)
	if dot > slash {
		return path[:dot] + suffix + path[dot:]
	}
	return path + suffix
}

func sanitise(s string) string {
	if s == "" {
		return "unknown"
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
			return r
		default:
			return '-'
		}
	}, s)
}

// Scan commits whatever is on disk now.
//
// This is how local edits reach history at all -- from the watcher while
// running, and at startup for everything that changed while the process was
// stopped. fsnotify replays nothing, so without the startup call those edits
// would stay invisible forever.
//
// There is no merge here and there cannot be: a filesystem writer supplies no
// base version, so this is last-writer-wins by construction.
// SetNormalizeNFC turns path canonicalisation on. Off by default: it changes
// what paths writes land on, so it is opt-in and set once at startup.
func (rc *Reconciler) SetNormalizeNFC(on bool) { rc.normalizeNFC = on }

// SetGuard installs the write guard. Nil disables it.
func (rc *Reconciler) SetGuard(g *guard.Guard) { rc.g = g }

// SetTripHandler installs the callback fired when a guard refuses a write.
func (rc *Reconciler) SetTripHandler(fn func(code, path, reason string)) {
	rc.notifyTrip = fn
}

// admit consults the guard for one path. It returns a protocol error when the
// write must be refused and nil when it may proceed.
func (rc *Reconciler) admit(path string, n int64) error {
	if rc.g == nil {
		return nil
	}
	v := rc.g.Admit(path, n)
	if v.OK() {
		return nil
	}
	if rc.notifyTrip != nil {
		rc.notifyTrip(v.Code, path, v.Reason)
	}
	return &protocol.Error{Code: v.Code, Message: v.Reason}
}

// changeSize reports how many bytes a change costs. A delete has no content
// and costs nothing.
//
// Size is a client-supplied wire field, so it is a hint: used when present,
// and the stored blob measured otherwise. A client understating its own
// writes gains little -- it still had to upload the content before the push,
// and the vault-wide counter and the disk floor both measure what landed.
func changeSize(r *repo.Repo, ch Change) int64 {
	if ch.Hash == "" {
		return 0
	}
	if ch.Size > 0 {
		return ch.Size
	}
	b, err := r.ReadBlob(ch.Hash)
	if err != nil {
		return 0
	}
	return int64(len(b))
}

func (rc *Reconciler) Scan(msg string) (string, error) {
	rc.mu.Lock()
	before, _ := rc.r.Head()
	head, err := rc.r.Commit(msg)
	rc.mu.Unlock()

	// The local path records but never refuses: these bytes are already on
	// disk, so refusing would not reclaim one of them and would silently
	// diverge the vault from its history. The watcher reads Pressure and
	// widens its commit debounce instead.
	//
	// This path matters as much as Push and is easy to miss -- the watcher
	// commits through here without touching Push, and SilverBullet and
	// memo-bridge both write vault files directly.
	if err == nil && rc.g != nil && head != before {
		if changed, cerr := rc.r.Changes(before, head); cerr == nil {
			for _, ch := range changed {
				if v := rc.g.Admit(ch.Path, ch.Size); !v.OK() && rc.notifyTrip != nil {
					rc.notifyTrip(v.Code, ch.Path, v.Reason)
				}
			}
		}
	}

	if err == nil {
		rc.notify(before, head)
	}
	return head, err
}

// Freeze runs fn while holding the commit lock, so no commit can land while it
// executes. Used by the backup export: a git directory read without this can
// capture a ref whose objects were not captured yet, which restores as
// "invalid sha1 pointer". Measured at 1 in 8 naive copies under load.
//
// Keep fn short. Everything that writes to the vault waits on it.
func (rc *Reconciler) Freeze(fn func() error) error {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return fn()
}
