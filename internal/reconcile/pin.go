package reconcile

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/pkronstrom/obsidian-archivist/internal/pins"
)

// MaxPinName bounds a pin's name. Without a cap a write token can push an
// arbitrarily large string into a committed file -- and a line over the
// parser's 1 MiB scanner limit becomes silently unreadable, so the API would
// report success for a pin that listing can never show.
const MaxPinName = 200

var (
	// ErrPinHeadMismatch means the vault moved between the caller reading HEAD
	// and asking to pin it. The caller wanted to pin what it was looking at,
	// and that is no longer the tip.
	ErrPinHeadMismatch = errors.New("head moved since the pin was requested")
	// ErrPinPathMissing means a file pin named a path that does not exist at
	// HEAD. Creating it would mint a pin that can never be opened.
	ErrPinPathMissing = errors.New("no such path at head")
	// ErrPinName means the name is empty or beyond MaxPinName.
	ErrPinName = errors.New("invalid pin name")
)

// Pin appends a pin and commits it, atomically with respect to every other
// writer.
//
// This lives on the Reconciler rather than in the API handler for two reasons:
// the commit lock is not reachable from outside this package, and Repo.Commit
// alone would race an in-flight push -- the pin would land in a commit whose
// tree includes a half-applied change set.
//
// expectedHead is REQUIRED. "Pin this version" is a claim about a specific
// tree; with the plugin's sync debounce, a caller that skipped the check could
// silently pin an older server state than the note on the user's screen. A
// mismatch is the caller's cue to flush and retry, not an error to swallow.
func (rc *Reconciler) Pin(expectedHead, name, path string) (pins.Entry, string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > MaxPinName {
		return pins.Entry{}, "", fmt.Errorf("%w: %d bytes", ErrPinName, len(name))
	}

	rc.mu.Lock()
	defer rc.mu.Unlock()

	head, err := rc.r.Head()
	if err != nil {
		return pins.Entry{}, "", err
	}
	if expectedHead != head {
		return pins.Entry{}, head, fmt.Errorf("%w: have %s, expected %s", ErrPinHeadMismatch, head, expectedHead)
	}

	// A file pin must name something that exists in the tree it pins.
	// Validated under the lock, so the answer cannot go stale between the
	// check and the commit.
	if path != "" {
		if _, err := rc.r.ReadAt(head, path); err != nil {
			return pins.Entry{}, head, fmt.Errorf("%w: %s", ErrPinPathMissing, path)
		}
	}

	existing, err := rc.v.Read(pins.File)
	if err != nil && !os.IsNotExist(err) {
		return pins.Entry{}, head, err
	}

	entry := pins.Entry{ID: pins.NewID(), Name: name, Path: path, Created: time.Now().UTC()}
	next, err := pins.Append(existing, entry)
	if err != nil {
		return pins.Entry{}, head, err
	}

	// The write guards see every other route; this one would otherwise be a
	// way to commit unmetered, including while pins.jsonl is quarantined.
	if err := rc.admit(pins.File, int64(len(next)-len(existing))); err != nil {
		return pins.Entry{}, head, err
	}

	if err := rc.write(pins.File, next); err != nil {
		return pins.Entry{}, head, err
	}

	newHead, err := rc.r.Commit("pin: " + entry.ID)
	if err != nil {
		// Roll the working tree back, still holding the lock. Left in place,
		// the orphaned line would be swept up by the next unrelated commit --
		// and the resolver would then read THAT commit as the pin's snapshot,
		// quietly naming a tree the user never asked for.
		var rollback error
		if len(existing) == 0 {
			rollback = rc.v.Remove(pins.File)
		} else {
			rollback = rc.write(pins.File, existing)
		}
		if rollback != nil {
			// Both failed: say so rather than reporting only the commit
			// error, because the vault is now holding a line that no commit
			// claims and the operator needs to know which file to look at.
			return pins.Entry{}, head, fmt.Errorf("pin commit failed (%w) and %s could not be restored: %v", err, pins.File, rollback)
		}
		return pins.Entry{}, head, err
	}
	rc.notify(head, newHead)
	return entry, newHead, nil
}
