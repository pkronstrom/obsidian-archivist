package repo

import (
	"errors"
	"fmt"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/pkronstrom/obsidian-archivist/protocol"
)

// Revision is one point in a file's history.
type Revision = protocol.Revision

// History lists the revisions in which a path changed, newest first.
//
// Only commits that actually touched the path are returned -- a vault commits
// on every edit anywhere, so an unfiltered log would be almost entirely noise.
func (r *Repo) History(path string, limit int) ([]protocol.Revision, error) {
	revs, _, err := r.HistoryPage(path, limit, "")
	return revs, err
}

// ErrUnknownCursor is returned when a history cursor names a commit that does
// not appear in the path's history. Reporting it beats returning an empty page:
// an empty page reads as "no more revisions" and would silently end a listing
// that actually broke.
var ErrUnknownCursor = errors.New("unknown history cursor")

// MaxHistoryLimit caps one page. The log walk is over the WHOLE repository
// history filtered by path, so an uncapped limit lets one request traverse
// every commit in the vault.
const MaxHistoryLimit = 200

// HistoryPage lists one page of a path's revisions, newest first.
//
// before is an exclusive cursor: the page starts at the revision AFTER that
// commit. Paging is by commit rather than by offset because the history grows
// at the newest end -- an offset would shift under the caller between pages and
// silently skip or repeat a revision.
func (r *Repo) HistoryPage(path string, limit int, before string) ([]protocol.Revision, bool, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > MaxHistoryLimit {
		limit = MaxHistoryLimit
	}
	iter, err := r.git.Log(&git.LogOptions{FileName: &path})
	if err != nil {
		return nil, false, err
	}
	defer iter.Close()

	out := []protocol.Revision{}
	seeking := before != ""
	found := false
	more := false
	err = iter.ForEach(func(c *object.Commit) error {
		if seeking {
			if c.Hash.String() == before {
				seeking, found = false, true
			}
			// Skip the cursor commit itself as well: the cursor is exclusive,
			// so the caller already has it.
			return nil
		}
		if len(out) >= limit {
			// One past the page proves there is another page, without walking
			// the rest of the history to count it.
			more = true
			return object.ErrCanceled
		}
		out = append(out, revisionAt(c, path))
		return nil
	})
	if err != nil && err != object.ErrCanceled {
		return nil, false, err
	}
	if before != "" && !found {
		return nil, false, fmt.Errorf("%w: %q", ErrUnknownCursor, before)
	}
	return out, more, nil
}

// revisionAt builds a Revision for one commit, marking it deleted when the
// path is absent from that commit's tree. A deleted revision cannot be read
// back through /v1/at, which is why callers that pick a representative for a
// group must skip them.
func revisionAt(c *object.Commit, path string) protocol.Revision {
	rev := protocol.Revision{
		Commit:  c.Hash.String(),
		Short:   c.Hash.String()[:8],
		When:    c.Author.When,
		Message: firstLine(c.Message),
	}
	if t, err := c.Tree(); err == nil {
		if f, err := t.File(path); err == nil {
			rev.Size, rev.Hash = f.Size, f.Hash.String()
		} else {
			rev.Deleted = true
		}
	}
	return rev
}

func firstLine(s string) string {
	for i, c := range s {
		if c == '\n' {
			return s[:i]
		}
	}
	return s
}

// CheckReport is the result of a consistency check.
type CheckReport = protocol.CheckResponse

// Check compares the working tree against HEAD.
//
// Anything reported here means the watcher missed something -- a dropped
// inotify event, a crash between writing and committing, or an edit made while
// the process was stopped and never scanned. Extra and Differing are usually
// benign and resolve on the next commit; Missing is the one to look at, because
// it means git holds a file the vault does not.
func (r *Repo) Check() (*protocol.CheckResponse, error) {
	head, err := r.Head()
	if err != nil {
		return nil, err
	}
	rep := &protocol.CheckResponse{Head: head}

	tracked, err := r.Snapshot(head)
	if err != nil {
		return nil, err
	}
	rep.Files = len(tracked)

	wt, err := r.git.Worktree()
	if err != nil {
		return nil, err
	}
	status, err := wt.Status()
	if err != nil {
		return nil, err
	}
	for path, st := range status {
		if !r.syncable(path) {
			continue // excluded by design; not drift
		}
		switch {
		case st.Worktree == git.Deleted:
			rep.Missing = append(rep.Missing, path)
		case st.Worktree == git.Untracked:
			rep.Extra = append(rep.Extra, path)
		case st.Worktree != git.Unmodified:
			rep.Differing = append(rep.Differing, path)
		}
	}
	rep.OK = len(rep.Missing) == 0 && len(rep.Extra) == 0 && len(rep.Differing) == 0
	return rep, nil
}

// Resolve turns a user-supplied revision into a commit hash. "" and "head" mean
// the current head, so callers can accept a friendly argument.
func (r *Repo) Resolve(rev string) (string, error) {
	if rev == "" || rev == "head" || rev == "HEAD" {
		return r.Head()
	}
	if len(rev) < 40 {
		// A short hash: scan history rather than fail, because a short hash is
		// what `history` prints and what a person will paste back.
		iter, err := r.git.Log(&git.LogOptions{})
		if err != nil {
			return "", err
		}
		defer iter.Close()
		var found string
		_ = iter.ForEach(func(c *object.Commit) error {
			if found == "" && len(c.Hash.String()) >= len(rev) && c.Hash.String()[:len(rev)] == rev {
				found = c.Hash.String()
				return object.ErrCanceled
			}
			return nil
		})
		if found == "" {
			return "", fmt.Errorf("%w: %q", ErrUnknownBase, rev)
		}
		return found, nil
	}
	return rev, nil
}
