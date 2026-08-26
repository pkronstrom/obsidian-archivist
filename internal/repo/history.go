package repo

import (
	"errors"
	"fmt"
	"sort"

	"strings"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/utils/diff"
	"github.com/pkronstrom/obsidian-archivist/internal/vault"
	"github.com/pkronstrom/obsidian-archivist/protocol"
	"github.com/sergi/go-diff/diffmatchpatch"
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

// MaxHistoryLimit caps one page at the HTTP boundary. The log walk is over the
// WHOLE repository history filtered by path, so an uncapped limit lets one
// request traverse every commit in the vault. In-process callers are trusted
// and may ask for everything -- the pin resolver must, since it has to see the
// first appearance of every id, and a cap would silently lose the oldest pins.
const MaxHistoryLimit = 200

// HistoryPage lists one page of a path's revisions, newest first.
//
// before is an exclusive cursor: the page starts at the revision AFTER that
// commit. Paging is by commit rather than by offset because the history grows
// at the newest end -- an offset would shift under the caller between pages and
// silently skip or repeat a revision.
func (r *Repo) HistoryPage(path string, limit int, before string) ([]protocol.Revision, bool, error) {
	// limit <= 0 means every revision. The route never passes that; it applies
	// its own default and cap before calling.
	unlimited := limit <= 0
	// Start the walk AT the cursor rather than at HEAD. Re-seeking from HEAD
	// on every page makes a full listing quadratic in the number of commits,
	// which on a drafting-heavy vault is the difference between paging and
	// hanging.
	opts := &git.LogOptions{FileName: &path}
	if before != "" {
		h := plumbing.NewHash(before)
		if h.IsZero() {
			return nil, false, fmt.Errorf("%w: %q", ErrUnknownCursor, before)
		}
		if _, err := r.git.CommitObject(h); err != nil {
			return nil, false, fmt.Errorf("%w: %q", ErrUnknownCursor, before)
		}
		opts.From = h
	}
	iter, err := r.git.Log(opts)
	if err != nil {
		return nil, false, err
	}
	defer iter.Close()

	out := []protocol.Revision{}
	seeking := before != ""
	found := before == ""
	more := false
	err = iter.ForEach(func(c *object.Commit) error {
		if seeking {
			// The cursor is exclusive, so its own commit is skipped -- and it
			// must be the FIRST thing this filtered walk yields. A commit that
			// exists but never touched this path is not a position in this
			// path's history, and silently starting below it would hand back a
			// page from the middle of a listing the caller never asked for.
			seeking = false
			if c.Hash.String() != before {
				return object.ErrCanceled
			}
			found = true
			return nil
		}
		if !unlimited && len(out) >= limit {
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
	if !found {
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
	rev.Device, rev.Via = originOf(c.Message)

	t, err := c.Tree()
	if err != nil {
		return rev
	}
	f, err := t.File(path)
	if err != nil {
		rev.Deleted = true
		return rev
	}
	rev.Size, rev.Hash = f.Size, f.Hash.String()
	rev.Added, rev.Removed, rev.Created = lineDelta(c, f, path)
	return rev
}

// maxDiffBytes bounds what gets line-counted. A revision list is a browsing
// aid, not a diff viewer, and reading two multi-megabyte blobs per row to
// render "+3 -1" is a bad trade -- so past this size the counts are simply
// absent rather than slow.
const maxDiffBytes = 1 << 20

// lineDelta counts lines added and removed against the previous revision of
// this path, and reports whether this revision created it.
//
// Zero/zero is ambiguous on its own -- it means "no textual change", "binary",
// "too large" and "first revision" alike -- which is why created is returned
// separately. A caller showing "+0 -0" on a file's first commit would be
// stating something false.
func lineDelta(c *object.Commit, f *object.File, path string) (added, removed int, created bool) {
	if f.Size > maxDiffBytes {
		return 0, 0, false
	}
	parent, err := c.Parent(0)
	if err != nil {
		// No parent at all: this commit created the file.
		return 0, 0, true
	}
	pt, err := parent.Tree()
	if err != nil {
		return 0, 0, false
	}
	pf, err := pt.File(path)
	if err != nil {
		// Absent from the parent, so this revision introduced it.
		return 0, 0, true
	}
	if pf.Hash == f.Hash {
		// Committed alongside other paths without changing this one.
		return 0, 0, false
	}
	if pf.Size > maxDiffBytes {
		return 0, 0, false
	}

	before, err := pf.Contents()
	if err != nil {
		return 0, 0, false
	}
	after, err := f.Contents()
	if err != nil {
		return 0, 0, false
	}
	if isBinary([]byte(before)) || isBinary([]byte(after)) {
		return 0, 0, false
	}

	for _, d := range diff.Do(before, after) {
		switch d.Type {
		case diffmatchpatch.DiffInsert:
			added += countLines(d.Text)
		case diffmatchpatch.DiffDelete:
			removed += countLines(d.Text)
		}
	}
	return added, removed, false
}

// countLines counts the lines a diff chunk spans. A chunk that changes part of
// one line still touched a line, so an empty count would under-report every
// small edit -- which is the most common kind.
func countLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// originOf reads who made a change out of a commit message.
//
// The first line carries the device ("sync from work-mac"), and a Via trailer
// names an intermediary when one was involved. Both are best-effort: messages
// written before the convention simply yield nothing, which is why the fields
// are omitempty rather than defaulted to a guess.
func originOf(msg string) (device, via string) {
	first := firstLine(msg)
	switch {
	case strings.HasPrefix(first, "sync from "):
		device = strings.TrimSpace(strings.TrimPrefix(first, "sync from "))
	case strings.HasPrefix(first, "append from "):
		device = strings.TrimSpace(strings.TrimPrefix(first, "append from "))
	case strings.HasPrefix(first, "edit from "):
		device = strings.TrimSpace(strings.TrimPrefix(first, "edit from "))
	case strings.HasPrefix(first, "local edit"), strings.HasPrefix(first, "startup scan"),
		strings.HasPrefix(first, "pin:"):
		// Made in the vault directory rather than pushed by a client.
		device = "server"
	}
	for _, line := range strings.Split(msg, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "Via: "); ok {
			via = strings.TrimSpace(v)
		}
	}
	return device, via
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

	// A path that is tracked in HEAD but excluded by today's rules is
	// STRANDED: Commit ignores an excluded path in both directions, so its
	// edits and even its deletion are refused, and it can never leave history
	// through ordinary use. git status cannot reveal it -- an unmodified
	// tracked file produces no status entry at all -- so it is computed from
	// the snapshot instead. Zero on a healthy vault; anything else needs a
	// deliberate removal commit.
	for path := range tracked {
		if !r.syncable(path) {
			rep.Stranded = append(rep.Stranded, path)
		}
	}
	sort.Strings(rep.Stranded)

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
			// Excluded by design, so not drift -- but counted, not ignored.
			// Local-only files are invisible to the watcher, to Commit and to
			// every other report, so without this a loop that materialises
			// revisions server-side could leave thousands of files that
			// nothing mentions while every status scan pays to walk them.
			if vault.LocalOnly(path) {
				rep.LocalOnly++
			}
			continue
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
	// Stranded counts against OK: it is a real inconsistency between what
	// history holds and what the rules allow, and it will not resolve itself.
	rep.OK = len(rep.Missing) == 0 && len(rep.Extra) == 0 && len(rep.Differing) == 0 &&
		len(rep.Stranded) == 0
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
