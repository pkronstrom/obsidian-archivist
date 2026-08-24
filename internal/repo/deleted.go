package repo

import (
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
		return nil, err
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
		changes, err := object.DiffTree(pt, ct)
		if err != nil {
			return nil
		}
		for _, ch := range changes {
			action, err := ch.Action()
			if err != nil || action != merkletrie.Delete {
				continue
			}
			path := ch.From.Name
			if path == "" || live[path].Hash != "" || seen[path] {
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
