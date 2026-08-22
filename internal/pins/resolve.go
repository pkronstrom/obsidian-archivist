package pins

import (
	"errors"
	"sync"

	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/pkronstrom/obsidian-archivist/protocol"
)

// Source is the slice of the repository the resolver needs.
type Source interface {
	Head() (string, error)
	History(path string, limit int) ([]protocol.Revision, error)
	ReadAt(rev, path string) ([]byte, error)
}

// IsNotFound reports the "path absent from this tree" case, as distinct from a
// damaged repository. The distinction matters because the two need opposite
// answers: an absent pins file means an unpinned vault, while an unreadable
// one means missing objects, and reporting the second as the first shows data
// loss as "you have no restore points".
func IsNotFound(err error) bool {
	// Deliberately NOT plumbing.ErrObjectNotFound: that means a tree or blob
	// object is missing from the store, which is damage, not absence. Treating
	// it as "no pins" would report a broken repository as an empty one -- the
	// single answer indistinguishable from data loss.
	return errors.Is(err, object.ErrFileNotFound) ||
		errors.Is(err, object.ErrEntryNotFound)
}

// Resolved is an entry plus what could be worked out about it.
type Resolved struct {
	Entry
	// Commit is the revision this pin names: the commit that first introduced
	// its id. Empty if the id was never found, which should not happen.
	Commit string `json:"commit"`
	// Duplicate marks an entry whose id already appeared. Its snapshot cannot
	// be determined -- first appearance identifies exactly one line -- so it
	// is listed as broken rather than silently resolved to the other pin's
	// commit.
	Duplicate bool `json:"duplicate,omitempty"`
	// Available reports that the pin can actually be opened -- its snapshot
	// exists and, for a file pin, still contains the path. A prune can drop
	// the blobs of a deleted path while the entry survives, and a pin that
	// silently 404s on click is worse than one shown as unavailable.
	Available bool `json:"available"`
}

// Resolver maps pin ids to the commits that introduced them.
//
// The mapping cannot come from history alone: History reports commit metadata,
// not line diffs, so working out WHICH pin a commit added means reading the
// file at each revision. That is why this exists at all, and why it caches --
// the walk is proportional to the number of pin commits, and the modal asks on
// every open.
type Resolver struct {
	src Source

	mu     sync.Mutex
	atHead string
	cached map[string]string // id -> commit
}

func NewResolver(src Source) *Resolver { return &Resolver{src: src} }

// List returns every pin at HEAD, resolved. path filters to one file's pins;
// "*" asks for vault-wide pins only; empty returns everything.
func (rv *Resolver) List(path string) ([]Resolved, string, error) {
	head, err := rv.src.Head()
	if err != nil {
		return nil, "", err
	}
	if head == "" {
		// An empty repository: no commits, so no pins, and no read to attempt.
		return []Resolved{}, head, nil
	}
	content, err := rv.src.ReadAt(head, File)
	if err != nil {
		// Only "the file is not in this tree" means an unpinned vault. Any
		// other failure is repository damage, and answering 200 with an empty
		// list would present missing restore points as "you have none" --
		// the one report that looks identical to data loss.
		if IsNotFound(err) {
			return []Resolved{}, head, nil
		}
		return nil, head, err
	}
	index, err := rv.index(head)
	if err != nil {
		return nil, head, err
	}

	entries := Parse(content)

	// Duplicate ids are decided over the WHOLE file, before any filtering.
	// Deciding inside the filtered loop would miss the case that matters: if
	// the original line belongs to Other.md and the copy to Note.md, listing
	// Note.md never sees the original, so the copy looks canonical and
	// silently resolves to the other note's commit.
	//
	// A duplicated id -- a hand copy-paste of a line -- cannot be resolved at
	// all: first appearance identifies exactly one line. Keep the first and
	// mark the rest rather than inventing a meaning for the ambiguity.
	canonical := map[string]int{}
	for i, e := range entries {
		if _, seen := canonical[e.ID]; !seen {
			canonical[e.ID] = i
		}
	}

	out := []Resolved{}
	for i, e := range entries {
		switch {
		case path == "*" && e.Path != "":
			continue
		case path != "" && path != "*" && e.Path != path:
			continue
		}
		r := Resolved{Entry: e, Commit: index[e.ID]}
		if canonical[e.ID] != i {
			r.Duplicate = true
			r.Commit = ""
		}
		if r.Commit != "" {
			if e.Path == "" {
				// A vault-wide pin needs only its snapshot to exist.
				_, err := rv.src.ReadAt(r.Commit, File)
				r.Available = err == nil
			} else {
				_, err := rv.src.ReadAt(r.Commit, e.Path)
				r.Available = err == nil
			}
		}
		out = append(out, r)
	}
	return out, head, nil
}

// index builds id -> first-appearance commit, cached per head.
func (rv *Resolver) index(head string) (map[string]string, error) {
	rv.mu.Lock()
	defer rv.mu.Unlock()
	if rv.atHead == head && rv.cached != nil {
		return rv.cached, nil
	}

	revs, err := rv.src.History(File, 0)
	if err != nil {
		return nil, err
	}

	// History is newest-first; walk it backwards so the FIRST commit to
	// contain an id is the one recorded. A later commit that still contains
	// the line must not overwrite that -- every commit after the pin was
	// created still carries it, so taking the newest would resolve every pin
	// to the most recent pin commit.
	idx := map[string]string{}
	for i := len(revs) - 1; i >= 0; i-- {
		rev := revs[i]
		if rev.Deleted {
			continue
		}
		content, err := rv.src.ReadAt(rev.Commit, File)
		if err != nil {
			// History said the path was present at this commit, so a read
			// failure here is a damaged object, not an absence. Skipping it
			// would move every pin introduced in that commit to a later one.
			if IsNotFound(err) {
				continue
			}
			return nil, err
		}
		for _, e := range Parse(content) {
			if _, seen := idx[e.ID]; !seen {
				idx[e.ID] = rev.Commit
			}
		}
	}

	rv.atHead, rv.cached = head, idx
	return idx, nil
}
