package pins

import (
	"sync"

	"github.com/pkronstrom/obsidian-archivist/protocol"
)

// Source is the slice of the repository the resolver needs.
type Source interface {
	Head() (string, error)
	History(path string, limit int) ([]protocol.Revision, error)
	ReadAt(rev, path string) ([]byte, error)
}

// Resolved is an entry plus what could be worked out about it.
type Resolved struct {
	Entry
	// Commit is the revision this pin names: the commit that first introduced
	// its id. Empty if the id was never found, which should not happen.
	Commit string `json:"commit"`
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
	content, err := rv.src.ReadAt(head, File)
	if err != nil {
		// No pins file yet is not an error: it is a vault nobody has pinned in.
		return []Resolved{}, head, nil
	}
	index, err := rv.index(head)
	if err != nil {
		return nil, head, err
	}

	out := []Resolved{}
	for _, e := range Parse(content) {
		switch {
		case path == "*" && e.Path != "":
			continue
		case path != "" && path != "*" && e.Path != path:
			continue
		}
		r := Resolved{Entry: e, Commit: index[e.ID]}
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
			continue
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
