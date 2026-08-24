package repo

import (
	"context"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/utils/merkletrie"
	"github.com/pkronstrom/obsidian-archivist/protocol"
)

// Deleted lists paths that history holds but HEAD does not.
//
// Nothing is ever really gone: a deletion is just a commit where the path
// stops existing, so every version remains in the pack. That has been true
// since the first release -- this only makes it VISIBLE. Recovering is the
// existing restore command, which is why this returns the revision to hand it.
//
// A path deleted and later re-created is not listed: it exists at HEAD, so
// there is nothing to recover.
func (r *Repo) Deleted() ([]protocol.DeletedPath, error) {
	head, err := r.Head()
	if err != nil {
		// A vault with no commits yet has deleted nothing. Reporting that as a
		// server error would make a brand-new vault look broken on the one
		// screen whose whole job is reassurance.
		return []protocol.DeletedPath{}, nil
	}
	if head == "" {
		return []protocol.DeletedPath{}, nil
	}
	live, err := r.Snapshot(head)
	if err != nil {
		return nil, err
	}

	iter, err := r.git.Log(&git.LogOptions{})
	if err != nil {
		return nil, err
	}
	defer iter.Close()

	// Newest-first, and the FIRST deletion seen for a path is the one that
	// matters: an older one was undone by a re-creation that a newer deletion
	// then removed again, and the newest is what the user actually lost.
	seen := map[string]bool{}
	// Paths seen leaving via a rename. Walking newest-first means a move is
	// recorded before any older deletion of the same path is reached, so this
	// suppresses the stale one too.
	moved := map[string]bool{}
	out := []protocol.DeletedPath{}

	err = iter.ForEach(func(c *object.Commit) error {
		parent, perr := c.Parent(0)
		if perr != nil {
			return nil // the root commit deletes nothing
		}
		ct, err := c.Tree()
		if err != nil {
			return nil
		}
		pt, err := parent.Tree()
		if err != nil {
			return nil
		}
		// Rename detection matters more than it sounds. Archivist records a
		// move as a delete plus a put, so a plain tree diff cannot tell "I
		// lost this note" from "I reorganised my vault" -- and offering a
		// moved note for restore is worse than noise: accepting would write a
		// SECOND copy of a note that already exists under its new name.
		//
		// A split, where one note's content is dispersed into others by hand,
		// is not mechanically detectable and will still be listed. That is the
		// honest limit: the old path really is gone, and only the author knows
		// its contents live on elsewhere.
		changes, err := object.DiffTreeWithOptions(context.Background(), pt, ct,
			&object.DiffTreeOptions{DetectRenames: true, RenameScore: 60})
		if err != nil {
			return nil
		}
		for _, ch := range changes {
			// A detected rename carries both ends. Its Action reports Insert or
			// Modify rather than Delete, but check the names too: the pair is
			// what makes it a move, and relying on the action alone would make
			// this depend on a detail of go-git's classification.
			if ch.From.Name != "" && ch.To.Name != "" && ch.From.Name != ch.To.Name {
				moved[ch.From.Name] = true
				continue
			}
			action, err := ch.Action()
			if err != nil || action != merkletrie.Delete {
				continue
			}
			path := ch.From.Name
			if path == "" || live[path].Hash != "" || seen[path] || moved[path] {
				continue
			}
			if !r.syncable(path) {
				continue // excluded paths were never really in the synced set
			}
			seen[path] = true
			device, _ := originOf(c.Message)
			out = append(out, protocol.DeletedPath{
				Path: path,
				When: c.Author.When,
				// The PARENT still has the content: the deleting commit is
				// where it stopped existing. Handing back the deletion itself
				// would give restore a revision the file is absent from.
				Revision:  parent.Hash.String(),
				Short:     parent.Hash.String()[:8],
				DeletedIn: c.Hash.String()[:8],
				Device:    device,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
