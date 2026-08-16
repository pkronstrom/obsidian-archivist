// Package repo is the entire state store.
//
// There is no database. Git already provides everything the sync protocol
// needs, so using anything else would mean maintaining a second copy of the
// same facts:
//
//	snapshot        the tree at a commit  -> path, hash, size
//	changes         a diff between commits
//	content store   the object database, content-addressed by construction
//	history         commits
//	the cursor      a commit hash
//
// The git directory lives OUTSIDE the working tree, so no `.git` ever appears
// in the vault and no client carries history.
//
// Nothing here shells out. go-git cannot merge (fast-forward only), but we
// never ask it to: merging happens in internal/merge and the result is
// committed as an ordinary change. The repository written here is a standard
// git repository that the real git CLI can read -- an interop guarantee, not a
// runtime dependency.
package repo

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"

	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/filesystem"
	"github.com/pkronstrom/obsidian-archivist/protocol"
)

// ErrUnknownBase means the client's cursor is not a commit we know: it is from
// another repository, or it predates a history rewrite. Either way the client
// must re-bootstrap from /snapshot rather than receive a bad diff.
var ErrUnknownBase = errors.New("repo: unknown base commit")

func IsUnknownBase(err error) bool { return errors.Is(err, ErrUnknownBase) }

// Entry and Change are the wire types. Aliased rather than redefined so there
// is exactly one definition of each shape in the codebase -- repo, reconcile
// and api previously carried near-identical copies that could drift.
type Entry = protocol.Entry

type Change = protocol.Change

// describe fills in Ext and Kind. Kind is sniffed from the bytes rather than
// guessed from the name, so an agent can trust it to decide what it can read.
func (r *Repo) describe(c Change) Change {
	c.Ext = strings.ToLower(strings.TrimPrefix(path.Ext(c.Path), "."))
	c.Kind = protocol.KindText
	if c.Op == protocol.OpDel {
		return c
	}
	if content, err := r.ReadBlob(c.Hash); err == nil && isBinary(content) {
		c.Kind = protocol.KindBinary
	}
	return c
}

// isBinary is git's own heuristic: a NUL byte in the first few KB. Duplicated
// from internal/merge rather than imported, to keep repo free of that
// dependency; the rule is three lines and will not drift.
func isBinary(b []byte) bool {
	const sniff = 8000
	if len(b) > sniff {
		b = b[:sniff]
	}
	return bytes.IndexByte(b, 0) >= 0
}

type Repo struct {
	git      *git.Repository
	workTree string
	gitDir   string

	// syncable decides what may enter git. Injected rather than imported so
	// this package stays independent of the vault package's policy.
	syncable func(path string) bool
}

// Open initialises the repository if absent and opens it otherwise.
func Open(workTree, gitDir string) (*Repo, error) {
	wt := osfs.New(workTree)
	dot := osfs.New(gitDir)
	st := filesystem.NewStorage(dot, cache.NewObjectLRUDefault())

	r, err := git.Open(st, wt)
	if errors.Is(err, git.ErrRepositoryNotExists) {
		r, err = git.Init(st, wt)
	}
	if err != nil {
		return nil, fmt.Errorf("repo: open %s: %w", gitDir, err)
	}
	return &Repo{
		git: r, workTree: workTree, gitDir: gitDir,
		syncable: func(string) bool { return true },
	}, nil
}

// SetSyncable installs the predicate deciding which paths may be committed.
// Without it every path is eligible, which is only correct in tests.
func (r *Repo) SetSyncable(fn func(path string) bool) { r.syncable = fn }

// Head is the current commit hash, or "" when nothing has been committed yet.
func (r *Repo) Head() (string, error) {
	ref, err := r.git.Head()
	if err != nil {
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			return "", nil
		}
		return "", err
	}
	return ref.Hash().String(), nil
}

// Commit stages the whole working tree and commits it. It returns the new head,
// or the unchanged head when there was nothing to commit -- an empty commit
// carries no information and would make every no-op watcher wakeup produce
// history.
func (r *Repo) Commit(msg string) (string, error) {
	wt, err := r.git.Worktree()
	if err != nil {
		return "", err
	}
	// Stage only paths the caller says are syncable.
	//
	// AddWithOptions{All:true} was wrong and measurably so: it stages the WHOLE
	// working tree, so .obsidian/ landed in git and would have been pushed to
	// every device -- despite the watcher, the plugin and every document saying
	// dotfiles are excluded. The exclusion has to be enforced where files enter
	// git, not only where events are observed.
	status, err := wt.Status()
	if err != nil {
		return "", err
	}
	staged := false
	for path, st := range status {
		if st.Worktree == git.Unmodified && st.Staging == git.Unmodified {
			continue
		}
		if !r.syncable(path) {
			continue
		}
		// Add stages deletions too when the file is gone; go-git needs the
		// explicit path either way.
		if _, err := wt.Add(path); err != nil {
			return "", fmt.Errorf("repo: stage %s: %w", path, err)
		}
		staged = true
	}
	if !staged {
		return r.Head()
	}
	h, err := wt.Commit(msg, &git.CommitOptions{
		// An explicit signature so nothing depends on ambient git config,
		// which does not exist in a scratch container.
		Author: signature(),
	})
	if err != nil {
		return "", fmt.Errorf("repo: commit: %w", err)
	}
	return h.String(), nil
}

func (r *Repo) tree(rev string) (*object.Tree, error) {
	if rev == "" {
		return nil, nil
	}
	h := plumbing.NewHash(rev)
	if h.IsZero() {
		return nil, fmt.Errorf("%w: %q", ErrUnknownBase, rev)
	}
	c, err := r.git.CommitObject(h)
	if err != nil {
		return nil, fmt.Errorf("%w: %q", ErrUnknownBase, rev)
	}
	return c.Tree()
}

// Snapshot lists every file at a revision. rev == "" yields an empty snapshot,
// which is the correct answer for a repository with no commits.
func (r *Repo) Snapshot(rev string) (map[string]Entry, error) {
	out := map[string]Entry{}
	t, err := r.tree(rev)
	if err != nil || t == nil {
		return out, err
	}
	err = t.Files().ForEach(func(f *object.File) error {
		out[f.Name] = protocol.Entry{Hash: f.Hash.String(), Size: f.Size}
		return nil
	})
	return out, err
}

// Changes reports the path-level differences between two revisions. from == ""
// means "from nothing", i.e. the full listing at `to`.
func (r *Repo) Changes(from, to string) ([]Change, error) {
	fromTree, err := r.tree(from)
	if err != nil {
		return nil, err
	}
	toTree, err := r.tree(to)
	if err != nil {
		return nil, err
	}
	if toTree == nil {
		return nil, nil
	}

	diff, err := object.DiffTree(fromTree, toTree)
	if err != nil {
		return nil, err
	}

	var out []Change
	for _, ch := range diff {
		action, err := ch.Action()
		if err != nil {
			return nil, err
		}
		switch action.String() {
		case "Delete":
			out = append(out, protocol.Change{Path: ch.From.Name, Op: protocol.OpDel})
		default: // Insert, Modify
			name := ch.To.Name
			_, toFile, err := ch.Files()
			if err != nil {
				return nil, err
			}
			c := protocol.Change{Path: name, Op: protocol.OpPut}
			if toFile != nil {
				c.Hash = toFile.Hash.String()
				c.Size = toFile.Size
			}
			out = append(out, c)
		}
	}
	for i := range out {
		out[i] = r.describe(out[i])
	}
	// Deterministic order makes responses diffable and tests stable.
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// ReadAt returns a file's content at a revision. This is the merge base.
func (r *Repo) ReadAt(rev, path string) ([]byte, error) {
	t, err := r.tree(rev)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, plumbing.ErrObjectNotFound
	}
	f, err := t.File(path)
	if err != nil {
		return nil, err
	}
	rc, err := f.Blob.Reader()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// WriteBlob stores content in the object database and returns its git object
// hash. Uploads land here before a push references them; anything never
// referenced is unreachable and `git gc` collects it, so there is no separate
// pending-upload lifecycle to manage.
func (r *Repo) WriteBlob(content []byte) (string, error) {
	obj := r.git.Storer.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	obj.SetSize(int64(len(content)))
	w, err := obj.Writer()
	if err != nil {
		return "", err
	}
	if _, err := w.Write(content); err != nil {
		w.Close()
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	h, err := r.git.Storer.SetEncodedObject(obj)
	if err != nil {
		return "", err
	}
	return h.String(), nil
}

func (r *Repo) ReadBlob(hash string) ([]byte, error) {
	h := plumbing.NewHash(hash)
	if h.IsZero() && hash != strings.Repeat("0", 40) {
		return nil, plumbing.ErrObjectNotFound
	}
	blob, err := r.git.BlobObject(h)
	if err != nil {
		return nil, err
	}
	rc, err := blob.Reader()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// HashContent computes the git object hash for content without storing it, so
// the API can verify an upload's claimed address before writing anything.
func HashContent(content []byte) (string, error) {
	obj := &plumbing.MemoryObject{}
	obj.SetType(plumbing.BlobObject)
	obj.SetSize(int64(len(content)))
	w, err := obj.Writer()
	if err != nil {
		return "", err
	}
	if _, err := w.Write(content); err != nil {
		w.Close()
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	return obj.Hash().String(), nil
}

func (r *Repo) HasBlob(hash string) bool {
	_, err := r.git.BlobObject(plumbing.NewHash(hash))
	return err == nil
}
