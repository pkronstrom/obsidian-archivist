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

	"github.com/pkronstrom/vaultsync/internal/merge"
	"github.com/pkronstrom/vaultsync/internal/repo"
	"github.com/pkronstrom/vaultsync/internal/vault"
)

// Statuses reported back per path.
const (
	StatusApplied  = "applied"  // written as sent
	StatusMerged   = "merged"   // three-way merged cleanly
	StatusConflict = "conflict" // both versions kept, user must resolve
	StatusRefused  = "refused"  // not applied, and why is the client's problem
)

// Change is one path-level operation from a client.
type Change struct {
	Path string `json:"path"`
	Op   string `json:"op"`   // "put" or "del"
	Hash string `json:"hash"` // git object hash of content already uploaded
}

// Result is what happened to one Change.
type Result struct {
	Path         string `json:"path"`
	Status       string `json:"status"`
	ConflictPath string `json:"conflictPath,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

type Reconciler struct {
	// mu serialises the two ingress paths. Both mutate the working tree and
	// commit; interleaving them would produce commits that do not match the
	// tree they claim to describe.
	mu sync.Mutex
	v  *vault.Vault
	r  *repo.Repo

	// written records content this process just wrote, so the filesystem
	// watcher can recognise its own echo instead of treating it as a new edit.
	echoMu  sync.Mutex
	written map[string]string
}

func New(v *vault.Vault, r *repo.Repo) *Reconciler {
	return &Reconciler{v: v, r: r, written: map[string]string{}}
}

func (rc *Reconciler) noteWrite(path string, content []byte) {
	rc.echoMu.Lock()
	defer rc.echoMu.Unlock()
	rc.written[path] = vault.Hash(content)
}

// WasOurWrite reports whether the content now at path is exactly what this
// process last wrote there, and consumes the record.
func (rc *Reconciler) WasOurWrite(path string, content []byte) bool {
	rc.echoMu.Lock()
	defer rc.echoMu.Unlock()
	want, ok := rc.written[path]
	if ok && want == vault.Hash(content) {
		delete(rc.written, path)
		return true
	}
	return false
}

// Push applies a client's change set, which was computed against base.
func (rc *Reconciler) Push(base, device string, changes []Change) (string, []Result, error) {
	rc.mu.Lock()
	defer rc.mu.Unlock()

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

	results := make([]Result, 0, len(changes))
	for _, ch := range changes {
		res, err := rc.applyOne(base, device, head, moved, ch)
		if err != nil {
			return "", nil, err
		}
		results = append(results, res)
	}

	newHead, err := rc.r.Commit(fmt.Sprintf("sync from %s", device))
	if err != nil {
		return "", nil, err
	}
	return newHead, results, nil
}

func (rc *Reconciler) applyOne(base, device, head string, moved map[string]bool, ch Change) (Result, error) {
	res := Result{Path: ch.Path, Status: StatusApplied}

	// An escaping path is a broken or hostile client. Fail the whole push
	// loudly rather than quietly refusing one entry -- there is no benign
	// reason for a client to send one, so continuing would hide a real problem.
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
			return res, rc.write(ch.Path, content)
		}
		return rc.resolve(base, device, ch.Path, content)

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
		return res, rc.write(path, theirs)
	}

	// Binary never merges. A merged binary is corrupt, and conflict markers in
	// a PNG are worse than either version alone.
	if merge.IsBinary(ours) || merge.IsBinary(theirs) {
		return rc.keepBoth(device, path, theirs)
	}

	baseContent, err := rc.r.ReadAt(base, path)
	if err != nil {
		// No common ancestor for this path -- both sides created it
		// independently. There is nothing to merge against.
		return rc.keepBoth(device, path, theirs)
	}

	merged, conflict, err := merge.Merge(baseContent, ours, theirs)
	if err != nil {
		return res, err
	}
	if conflict {
		return rc.keepBoth(device, path, theirs)
	}
	res.Status = StatusMerged
	return res, rc.write(path, merged)
}

// keepBoth leaves the server's version in place and parks the client's copy
// beside it. Deliberately the dumbest possible resolution: a conflict file is
// an ordinary note, so it syncs everywhere and can be resolved on a phone by
// editing and deleting it.
func (rc *Reconciler) keepBoth(device, path string, theirs []byte) (Result, error) {
	cp := conflictPath(path, device)
	res := Result{Path: path, Status: StatusConflict, ConflictPath: cp}
	return res, rc.write(cp, theirs)
}

func (rc *Reconciler) write(path string, content []byte) error {
	if err := rc.v.Write(path, content); err != nil {
		return err
	}
	rc.noteWrite(path, content)
	return nil
}

// conflictPath keeps the original extension so editors still recognise the
// file: notes/idea.md -> notes/idea.conflict-mac-20260816T093012.md
func conflictPath(path, device string) string {
	stamp := time.Now().UTC().Format("20060102T150405")
	dot := strings.LastIndex(path, ".")
	slash := strings.LastIndex(path, "/")
	suffix := fmt.Sprintf(".conflict-%s-%s", sanitise(device), stamp)
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
func (rc *Reconciler) Scan(msg string) (string, error) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.r.Commit(msg)
}

// Head is the current commit.
func (rc *Reconciler) Head() (string, error) { return rc.r.Head() }
