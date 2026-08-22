package reconcile

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/pkronstrom/obsidian-archivist/internal/pins"
)

var (
	// ErrPinHeadMismatch means the vault moved between the caller reading HEAD
	// and asking to pin it. The caller wanted to pin what it was looking at,
	// and that is no longer the tip.
	ErrPinHeadMismatch = errors.New("head moved since the pin was requested")
	// ErrPinPathMissing means a file pin named a path that does not exist at
	// HEAD. Creating it would mint a pin that can never be opened.
	ErrPinPathMissing = errors.New("no such path at head")
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
	if err := rc.write(pins.File, next); err != nil {
		return pins.Entry{}, head, err
	}

	newHead, err := rc.r.Commit("pin: " + entry.ID)
	if err != nil {
		return pins.Entry{}, head, err
	}
	rc.notify(head, newHead)
	return entry, newHead, nil
}
