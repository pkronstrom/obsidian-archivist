package repo

import (
	"errors"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// PruneResult reports what a rewrite did.
type PruneResult struct {
	OldHead   string   `json:"old_head"`
	NewHead   string   `json:"new_head"`
	Paths     []string `json:"paths"`
	Rewrote   int      `json:"commits_rewritten"`
	Reclaimed int64    `json:"bytes_reclaimed"`
}

// Prune rewrites history to remove the named paths entirely, then collects the
// objects that no longer belong to anything.
//
// Only paths absent at HEAD may be pruned; passing a live path is an error
// rather than a silent no-op, because removing one would destroy content the
// vault still serves.
//
// The rewrite is the reason this exists: deleting a file reclaims nothing,
// since every revision of it stays reachable from the commits it appeared in.
// Removing it from those commits is the only way the bytes ever go away.
//
// This is offline surgery. The server must not be running: it holds the
// working tree and would commit against a history that is being replaced
// underneath it.
func (r *Repo) Prune(paths []string) (*PruneResult, error) {
	if len(paths) == 0 {
		return nil, fmt.Errorf("repo: prune: no paths given")
	}

	oldHead, err := r.Head()
	if err != nil {
		return nil, err
	}
	if oldHead == "" {
		return nil, fmt.Errorf("repo: prune: nothing committed yet")
	}

	live, err := r.liveTreePaths()
	if err != nil {
		return nil, err
	}
	drop := map[string]bool{}
	for _, p := range paths {
		if live[p] {
			return nil, fmt.Errorf("repo: prune: %q still exists at HEAD; "+
				"only deleted paths can be pruned", p)
		}
		drop[p] = true
	}

	before, err := r.looseSize()
	if err != nil {
		return nil, err
	}

	commits, err := r.commitsOldestFirst()
	if err != nil {
		return nil, err
	}

	// mapped carries every old commit hash to its rewritten replacement, so a
	// commit's parents can be re-pointed as the walk moves forward. It is
	// discarded afterwards -- only the head pair is durable, in prune-map.
	mapped := map[plumbing.Hash]plumbing.Hash{}
	// safe carries the subset of mapped that is sound to TRANSLATE, which is
	// not the same set. Translating old->new is only correct when the two have
	// identical trees: a client whose base moved to a commit with different
	// content would diff against a tree it never had, and silently push back
	// whatever the rewrite removed.
	//
	// Prune only alters the tree of a commit that actually contained dropped
	// content. Every other rewritten commit is a pure re-parenting with a
	// byte-identical tree -- which is exactly the property the head pair has
	// always relied on, just checked per commit instead of assumed for one.
	safe := map[plumbing.Hash]plumbing.Hash{}
	var newHead plumbing.Hash

	for _, c := range commits {
		files, err := r.flatten(c, drop)
		if err != nil {
			return nil, err
		}
		treeHash, err := r.buildTree(files)
		if err != nil {
			return nil, err
		}

		parents := make([]plumbing.Hash, 0, len(c.ParentHashes))
		for _, p := range c.ParentHashes {
			if np, ok := mapped[p]; ok {
				parents = append(parents, np)
			}
		}

		nc := &object.Commit{
			Author:       c.Author,
			Committer:    c.Committer,
			Message:      c.Message,
			TreeHash:     treeHash,
			ParentHashes: parents,
		}
		obj := r.git.Storer.NewEncodedObject()
		if err := nc.Encode(obj); err != nil {
			return nil, err
		}
		h, err := r.git.Storer.SetEncodedObject(obj)
		if err != nil {
			return nil, err
		}
		mapped[c.Hash] = h
		if treeHash == c.TreeHash {
			safe[c.Hash] = h
		}
		newHead = h
	}

	if err := r.pointHeadAt(newHead); err != nil {
		return nil, err
	}

	// Every OTHER ref moves through the same mapping, which is what
	// git filter-repo does and what makes a rewrite honest.
	//
	// Without it a branch or tag left anywhere in this repository keeps its
	// own commit and the entire ancestry behind it reachable, so collect()
	// drops nothing under it. Two consequences, and the second is the serious
	// one: reclaim would report success while recovering no space, and a purge
	// -- the reason this exists, for a leaked token or an accidental 200 MB
	// file -- would leave that content reachable under the stale ref while
	// reporting that it was removed.
	if err := r.repointRefs(mapped); err != nil {
		return nil, err
	}

	// Update the prune-map BEFORE collecting, so a crash between the two
	// leaves a map that is merely stale rather than one pointing at commits
	// that have just been deleted.
	//
	// Existing entries are re-pointed through this rewrite, not merely
	// appended to. A second prune destroys the commits the first prune's
	// entries pointed AT, so an append-only map would send a device that
	// missed both prunes to a commit that no longer exists -- which is worse
	// than not translating at all, because it looks like it worked. Rewriting
	// the targets keeps every entry one hop from something live.
	if err := r.updatePruneMap(oldHead, newHead.String(), mapped, safe); err != nil {
		return nil, err
	}

	if err := r.collect(); err != nil {
		return nil, err
	}

	after, err := r.looseSize()
	if err != nil {
		return nil, err
	}
	reclaimed := before - after
	if reclaimed < 0 {
		reclaimed = 0
	}

	sorted := append([]string(nil), paths...)
	sort.Strings(sorted)
	return &PruneResult{
		OldHead:   oldHead,
		NewHead:   newHead.String(),
		Paths:     sorted,
		Rewrote:   len(commits),
		Reclaimed: reclaimed,
	}, nil
}

// flatten lists a commit's files as path -> entry, minus the dropped paths.
func (r *Repo) flatten(c *object.Commit, drop map[string]bool) (map[string]object.TreeEntry, error) {
	tree, err := c.Tree()
	if err != nil {
		return nil, err
	}
	out := map[string]object.TreeEntry{}
	w := object.NewTreeWalker(tree, true, nil)
	defer w.Close()
	for {
		name, entry, err := w.Next()
		if err != nil {
			break
		}
		if !entry.Mode.IsFile() || drop[name] {
			continue
		}
		out[name] = entry
	}
	return out, nil
}

// buildTree rebuilds a nested tree from a flat path -> entry map and returns
// its hash.
//
// Git trees are one object per directory, so a flat list has to be regrouped
// bottom-up: every directory's hash depends on its children's, and a directory
// that loses its last file must disappear rather than persist as an empty
// tree, which git has no representation for.
func (r *Repo) buildTree(files map[string]object.TreeEntry) (plumbing.Hash, error) {
	// children[dir] = set of immediate child directory names
	children := map[string]map[string]bool{}
	direct := map[string][]object.TreeEntry{}

	ensure := func(dir string) {
		for {
			if _, ok := children[dir]; !ok {
				children[dir] = map[string]bool{}
			}
			if dir == "" {
				return
			}
			parent := path.Dir(dir)
			if parent == "." {
				parent = ""
			}
			if _, ok := children[parent]; !ok {
				children[parent] = map[string]bool{}
			}
			children[parent][path.Base(dir)] = true
			dir = parent
		}
	}

	for name, entry := range files {
		dir := path.Dir(name)
		if dir == "." {
			dir = ""
		}
		ensure(dir)
		e := entry
		e.Name = path.Base(name)
		direct[dir] = append(direct[dir], e)
	}
	ensure("")

	var write func(dir string) (plumbing.Hash, bool, error)
	write = func(dir string) (plumbing.Hash, bool, error) {
		entries := append([]object.TreeEntry(nil), direct[dir]...)

		names := make([]string, 0, len(children[dir]))
		for child := range children[dir] {
			names = append(names, child)
		}
		sort.Strings(names)
		for _, child := range names {
			sub := child
			if dir != "" {
				sub = dir + "/" + child
			}
			h, nonEmpty, err := write(sub)
			if err != nil {
				return plumbing.ZeroHash, false, err
			}
			// A directory with nothing left in it is omitted. Git cannot
			// represent an empty tree inside another tree, and carrying one
			// would resurrect a folder the prune was meant to remove.
			if !nonEmpty {
				continue
			}
			entries = append(entries, object.TreeEntry{
				Name: child,
				Mode: filemode.Dir,
				Hash: h,
			})
		}

		if len(entries) == 0 && dir != "" {
			return plumbing.ZeroHash, false, nil
		}

		// Git requires entries sorted by name.
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })

		t := &object.Tree{Entries: entries}
		obj := r.git.Storer.NewEncodedObject()
		if err := t.Encode(obj); err != nil {
			return plumbing.ZeroHash, false, err
		}
		h, err := r.git.Storer.SetEncodedObject(obj)
		if err != nil {
			return plumbing.ZeroHash, false, err
		}
		return h, true, nil
	}

	h, _, err := write("")
	return h, err
}

// liveTreePaths is every file path present at HEAD.
func (r *Repo) liveTreePaths() (map[string]bool, error) {
	out := map[string]bool{}
	head, err := r.Head()
	if err != nil || head == "" {
		return out, nil
	}
	c, err := r.git.CommitObject(plumbing.NewHash(head))
	if err != nil {
		return out, nil
	}
	tree, err := c.Tree()
	if err != nil {
		return nil, err
	}
	w := object.NewTreeWalker(tree, true, nil)
	defer w.Close()
	for {
		name, entry, err := w.Next()
		if err != nil {
			break
		}
		if entry.Mode.IsFile() {
			out[name] = true
		}
	}
	return out, nil
}

// pointHeadAt moves whatever branch HEAD tracks to h.
// repointRefs moves every branch and tag onto its rewritten commit.
//
// A ref whose target was NOT rewritten is left alone: it already points at
// something the rewrite did not touch. A ref pointing at a commit that the
// rewrite dropped entirely is deleted rather than left dangling, because a ref
// to a collected object is a repository that fails to open, not a repository
// that has lost one branch.
func (r *Repo) repointRefs(mapped map[plumbing.Hash]plumbing.Hash) error {
	iter, err := r.git.References()
	if err != nil {
		return err
	}
	defer iter.Close()

	type move struct {
		name plumbing.ReferenceName
		to   plumbing.Hash
		drop bool
	}
	var moves []move

	err = iter.ForEach(func(ref *plumbing.Reference) error {
		// HEAD is handled by pointHeadAt, and a symbolic ref follows whatever
		// it targets rather than holding a hash of its own.
		if ref.Type() != plumbing.HashReference || ref.Name() == plumbing.HEAD {
			return nil
		}
		next, rewritten := mapped[ref.Hash()]
		if !rewritten {
			return nil
		}
		moves = append(moves, move{name: ref.Name(), to: next})
		return nil
	})
	if err != nil {
		return err
	}

	// Collected outside the iteration: mutating the ref store while walking it
	// is undefined, and go-git's iterators are not required to tolerate it.
	for _, m := range moves {
		if m.drop {
			if err := r.git.Storer.RemoveReference(m.name); err != nil {
				return fmt.Errorf("repo: drop ref %s: %w", m.name, err)
			}
			continue
		}
		if err := r.git.Storer.SetReference(plumbing.NewHashReference(m.name, m.to)); err != nil {
			return fmt.Errorf("repo: repoint ref %s: %w", m.name, err)
		}
	}
	return nil
}

func (r *Repo) pointHeadAt(h plumbing.Hash) error {
	ref, err := r.git.Reference(plumbing.HEAD, false)
	if err != nil {
		return err
	}
	if ref.Type() == plumbing.SymbolicReference {
		return r.git.Storer.SetReference(plumbing.NewHashReference(ref.Target(), h))
	}
	return r.git.Storer.SetReference(plumbing.NewHashReference(plumbing.HEAD, h))
}

// collect drops unreferenced objects and packs what remains.
//
// Order matters and the reason is not obvious. Prune only deletes LOOSE
// objects, so it must run while the orphans are still loose. Repacking first
// would be safe anyway -- createNewObjectPack walks refs and packs only
// reachable objects -- but prune-then-repack is cheaper and ends with one pack
// and no loose garbage.
//
// This is the only garbage collection the repository ever gets: go-git never
// runs gc, and the image has no git binary for gc.auto to be part of.
func (r *Repo) collect() error {
	// Handler is REQUIRED. go-git calls it for every unreferenced object
	// without a nil check, so PruneOptions{} panics rather than doing nothing
	// -- and DeleteObject is what it is meant to be, per go-git's own comment
	// that "the type conveniently matches PruneHandler".
	if err := r.git.Prune(git.PruneOptions{Handler: r.git.DeleteObject}); err != nil {
		return fmt.Errorf("repo: prune objects: %w", err)
	}
	if err := r.git.RepackObjects(&git.RepackConfig{}); err != nil {
		return fmt.Errorf("repo: repack: %w", err)
	}

	// Reopen. RepackObjects DELETES the old packfiles, but this storer still
	// holds the handles it opened them with, so every later read fails with
	// "packfile not found".
	//
	// A first prune hides this completely: a repository go-git has only ever
	// written to has no packs at all, so there is nothing to delete and
	// nothing to invalidate. The failure appears on the SECOND prune, which
	// deletes the pack the first one created -- and it appears as a
	// repository that reports a valid head and then cannot read a single
	// commit behind it.
	return r.reopen()
}

// reopen rebuilds the underlying storer against the same directories.
func (r *Repo) reopen() error {
	fresh, err := Open(r.workTree, r.gitDir)
	if err != nil {
		return fmt.Errorf("repo: reopen after repack: %w", err)
	}
	r.git = fresh.git
	return nil
}

// looseSize totals the objects the storer still holds loose, used to report
// how much a prune actually reclaimed rather than how much it hoped to.
func (r *Repo) looseSize() (int64, error) {
	var total int64
	iter, err := r.git.Storer.IterEncodedObjects(plumbing.AnyObject)
	if err != nil {
		return 0, err
	}
	defer iter.Close()
	err = iter.ForEach(func(o plumbing.EncodedObject) error {
		total += o.Size()
		return nil
	})
	return total, err
}

// updatePruneMap re-points every existing entry through this rewrite and adds
// the new head pair.
func (r *Repo) updatePruneMap(oldHead, newHead string, mapped, safe map[plumbing.Hash]plumbing.Hash) error {
	existing, err := ReadPruneMap(r.gitDir)
	if err != nil {
		return err
	}
	out := make(map[string]string, len(existing)+len(safe)+1)
	for from, to := range existing {
		if next, ok := mapped[plumbing.NewHash(to)]; ok {
			out[from] = next.String()
			continue
		}
		out[from] = to
	}
	// Every commit whose tree survived the rewrite untouched, not just the
	// head. This is what spares a device that was BEHIND when the prune ran:
	// its base is not the old head, so a head-only map leaves it with an
	// unknown base and a full re-bootstrap, even though its exact content is
	// still reachable under a new hash.
	for from, to := range safe {
		out[from.String()] = to.String()
	}
	out[oldHead] = newHead
	return WritePruneMap(r.gitDir, out)
}

// PruneMapName is the file recording head translations, beside the git dir.
const PruneMapName = "prune-map"

// WritePruneMap replaces the map on disk.
//
// It is what spares every device a full re-bootstrap: pruned paths are already
// absent at HEAD, so the rewritten HEAD's tree is byte-identical to the old
// one and only the commit hash changed. A device arriving with a recorded old
// head is told the new one and computes an empty diff.
//
// Written whole rather than appended because a prune re-points existing
// entries as well as adding one.
func WritePruneMap(dir string, m map[string]string) error {
	froms := make([]string, 0, len(m))
	for from := range m {
		froms = append(froms, from)
	}
	sort.Strings(froms)

	var b strings.Builder
	for _, from := range froms {
		b.WriteString(from + " " + m[from] + "\n")
	}
	tmp := path.Join(dir, PruneMapName+".tmp")
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return err
	}
	// Rename so a reader never sees a half-written map.
	return os.Rename(tmp, path.Join(dir, PruneMapName))
}

// ReadPruneMap loads old-head -> new-head pairs. A missing file is not an
// error: most repositories have never been pruned.
func ReadPruneMap(dir string) (map[string]string, error) {
	out := map[string]string{}
	data, err := readFileIfExists(path.Join(dir, PruneMapName))
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 {
			out[f[0]] = f[1]
		}
	}
	return out, nil
}

func readFileIfExists(file string) ([]byte, error) {
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return data, err
}
