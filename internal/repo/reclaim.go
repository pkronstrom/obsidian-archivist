package repo

import (
	"sort"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// Reclaimable is one path that no longer exists at HEAD, with what its
// history still costs.
type Reclaimable struct {
	Path string `json:"path"`
	// Bytes is the total size of this path's blob versions that nothing live
	// still references.
	Bytes int64 `json:"bytes"`
	// Versions is how many distinct blobs those are.
	Versions int `json:"versions"`
	// Added is when the path first appeared, Deleted when it last went away.
	Added   time.Time `json:"added"`
	Deleted time.Time `json:"deleted"`
}

// GoneFor reports how long the path has been deleted, as of now.
func (rc Reclaimable) GoneFor(now time.Time) time.Duration { return now.Sub(rc.Deleted) }

// Reclaimable lists paths deleted at HEAD whose blobs still occupy the
// repository, newest deletion last.
//
// Selection is by "deleted at HEAD", not by folder or extension. An
// attachments directory is a convention that will drift; the property that
// matters is large-and-gone, wherever the file lives.
//
// A blob still referenced by any live path is never counted. Git stores
// content once, so a deleted file whose bytes are identical to a live one
// costs nothing and pruning it would reclaim nothing.
func (r *Repo) Reclaimable() ([]Reclaimable, error) {
	live, err := r.liveBlobs()
	if err != nil {
		return nil, err
	}

	type acc struct {
		blobs    map[plumbing.Hash]int64
		added    time.Time
		deleted  time.Time
		lastSeen time.Time
		alive    bool
	}
	paths := map[string]*acc{}

	commits, err := r.commitsOldestFirst()
	if err != nil {
		return nil, err
	}

	for _, c := range commits {
		tree, err := c.Tree()
		if err != nil {
			return nil, err
		}
		seen := map[string]bool{}
		w := object.NewTreeWalker(tree, true, nil)
		for {
			name, entry, err := w.Next()
			if err != nil {
				break
			}
			if entry.Mode.IsFile() {
				seen[name] = true
				a := paths[name]
				if a == nil {
					a = &acc{blobs: map[plumbing.Hash]int64{}, added: c.Author.When}
					paths[name] = a
				}
				if _, known := a.blobs[entry.Hash]; !known {
					sz, err := r.git.Storer.EncodedObjectSize(entry.Hash)
					if err != nil {
						sz = 0
					}
					a.blobs[entry.Hash] = sz
				}
				a.alive = true
				a.lastSeen = c.Author.When
			}
		}
		w.Close()

		// A path present before and absent now was deleted by this commit.
		for name, a := range paths {
			if a.alive && !seen[name] {
				a.alive = false
				a.deleted = c.Author.When
			}
		}
	}

	out := []Reclaimable{}
	for name, a := range paths {
		if a.alive {
			continue // still present at HEAD
		}
		var total int64
		var versions int
		for h, sz := range a.blobs {
			if live[h] {
				continue // identical content is still referenced; free
			}
			total += sz
			versions++
		}
		if versions == 0 {
			continue
		}
		out = append(out, Reclaimable{
			Path:     name,
			Bytes:    total,
			Versions: versions,
			Added:    a.added,
			Deleted:  a.deleted,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Bytes != out[j].Bytes {
			return out[i].Bytes > out[j].Bytes
		}
		return out[i].Path < out[j].Path
	})
	return out, nil
}

// liveBlobs is every blob reachable from HEAD's tree.
func (r *Repo) liveBlobs() (map[plumbing.Hash]bool, error) {
	live := map[plumbing.Hash]bool{}
	head, err := r.Head()
	if err != nil || head == "" {
		return live, nil
	}
	c, err := r.git.CommitObject(plumbing.NewHash(head))
	if err != nil {
		return live, nil
	}
	tree, err := c.Tree()
	if err != nil {
		return nil, err
	}
	w := object.NewTreeWalker(tree, true, nil)
	defer w.Close()
	for {
		_, entry, err := w.Next()
		if err != nil {
			break
		}
		if entry.Mode.IsFile() {
			live[entry.Hash] = true
		}
	}
	return live, nil
}

// commitsOldestFirst returns every commit on HEAD's ancestry, oldest first.
//
// Order matters: the scan decides a path was deleted by noticing it present in
// one commit and absent in the next, which is only meaningful walking forward.
func (r *Repo) commitsOldestFirst() ([]*object.Commit, error) {
	head, err := r.Head()
	if err != nil || head == "" {
		return nil, nil
	}
	iter, err := r.git.Log(&git.LogOptions{From: plumbing.NewHash(head)})
	if err != nil {
		return nil, err
	}
	defer iter.Close()

	var commits []*object.Commit
	if err := iter.ForEach(func(c *object.Commit) error {
		commits = append(commits, c)
		return nil
	}); err != nil {
		return nil, err
	}
	for i, j := 0, len(commits)-1; i < j; i, j = i+1, j-1 {
		commits[i], commits[j] = commits[j], commits[i]
	}
	return commits, nil
}
