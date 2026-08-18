package repo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
)

// treeOf returns the tree hash at a commit.
func treeOf(t *testing.T, r *Repo, commit string) string {
	t.Helper()
	c, err := r.git.CommitObject(plumbing.NewHash(commit))
	if err != nil {
		t.Fatalf("commit %s: %v", commit, err)
	}
	return c.TreeHash.String()
}

// THE load-bearing property. Pruned paths are already absent at HEAD, so
// removing them from history cannot change what HEAD contains -- only the
// commit hashes above it. If this ever fails, head translation is unsound and
// every device would silently receive a wrong diff.
func TestPruneLeavesTheHeadTreeIdentical(t *testing.T) {
	r := reclaimRepo(t)
	writeCommit(t, r, "keep.md", "kept", "add keep")
	writeCommit(t, r, "attach/big.pdf", "big content here", "add big")
	writeCommit(t, r, "keep.md", "kept, edited", "edit keep")
	removeCommit(t, r, "attach/big.pdf", "delete big")

	oldHead, _ := r.Head()
	treeBefore := treeOf(t, r, oldHead)

	res, err := r.Prune([]string{"attach/big.pdf"})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	treeAfter := treeOf(t, r, res.NewHead)

	if treeBefore != treeAfter {
		t.Fatalf("HEAD tree changed: %s -> %s", treeBefore, treeAfter)
	}
	if res.OldHead != oldHead {
		t.Errorf("OldHead = %s, want %s", res.OldHead, oldHead)
	}
	if res.NewHead == oldHead {
		t.Error("NewHead equals OldHead; nothing was rewritten")
	}
}

// The point of the exercise: the blob is gone from every commit.
func TestPruneRemovesTheBlobFromHistory(t *testing.T) {
	r := reclaimRepo(t)
	writeCommit(t, r, "keep.md", "kept", "add keep")
	writeCommit(t, r, "big.bin", "the bytes we want back", "add big")
	removeCommit(t, r, "big.bin", "delete big")

	// Capture the blob hash while it is still reachable.
	before, err := r.Reclaimable()
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 1 {
		t.Fatalf("expected one reclaimable, got %+v", before)
	}

	if _, err := r.Prune([]string{"big.bin"}); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	after, err := r.Reclaimable()
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 0 {
		t.Fatalf("still reclaimable after prune: %+v", after)
	}
}

// Live content must survive untouched. This is the test that would catch a
// rewrite that dropped the wrong path.
func TestPrunePreservesLiveFiles(t *testing.T) {
	r := reclaimRepo(t)
	writeCommit(t, r, "keep.md", "v1", "add")
	writeCommit(t, r, "sub/also-keep.md", "sub v1", "add sub")
	writeCommit(t, r, "gone.md", "doomed", "add doomed")
	writeCommit(t, r, "keep.md", "v2", "edit")
	removeCommit(t, r, "gone.md", "delete doomed")

	res, err := r.Prune([]string{"gone.md"})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}

	snap, err := r.Snapshot(res.NewHead)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := snap["keep.md"]; !ok {
		t.Error("keep.md vanished")
	}
	if _, ok := snap["sub/also-keep.md"]; !ok {
		t.Error("sub/also-keep.md vanished")
	}
	if _, ok := snap["gone.md"]; ok {
		t.Error("gone.md survived the prune")
	}

	body, err := r.ReadAt(res.NewHead, "keep.md")
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "v2" {
		t.Errorf("keep.md = %q, want v2", body)
	}
}

// Pruning a path that still exists would destroy live content, so it is
// refused rather than silently skipped.
func TestPruneRefusesALivePath(t *testing.T) {
	r := reclaimRepo(t)
	writeCommit(t, r, "alive.md", "here", "add")

	_, err := r.Prune([]string{"alive.md"})
	if err == nil {
		t.Fatal("Prune accepted a live path")
	}
	if !strings.Contains(err.Error(), "still exists at HEAD") {
		t.Errorf("unhelpful error: %v", err)
	}
}

// A directory that loses its last file must disappear. Git has no
// representation for an empty tree inside another tree, and leaving one would
// resurrect the folder the prune was meant to remove.
func TestPruneRemovesNowEmptyDirectories(t *testing.T) {
	r := reclaimRepo(t)
	writeCommit(t, r, "keep.md", "kept", "add keep")
	writeCommit(t, r, "attach/only.pdf", "the only file in here", "add attach")
	removeCommit(t, r, "attach/only.pdf", "delete attach")

	res, err := r.Prune([]string{"attach/only.pdf"})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	snap, err := r.Snapshot(res.NewHead)
	if err != nil {
		t.Fatal(err)
	}
	for p := range snap {
		if strings.HasPrefix(p, "attach/") {
			t.Fatalf("attach/ survived as %q", p)
		}
	}
}

// After a prune the repository must be collected: no loose garbage, and one
// pack. A regression that skips collection leaves the bytes on disk and the
// whole operation reclaims nothing.
func TestPruneCollectsObjects(t *testing.T) {
	r := reclaimRepo(t)
	writeCommit(t, r, "keep.md", "kept", "add")
	writeCommit(t, r, "big.bin", strings.Repeat("x", 4096), "add big")
	removeCommit(t, r, "big.bin", "delete")

	if _, err := r.Prune([]string{"big.bin"}); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	entries, err := os.ReadDir(filepath.Join(r.GitDir(), "objects", "pack"))
	if err != nil {
		t.Fatalf("no pack directory after prune: %v", err)
	}
	packs := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".pack") {
			packs++
		}
	}
	if packs == 0 {
		t.Fatal("prune did not repack; objects were left loose")
	}
}

// History for the surviving files must still be readable. A rewrite that
// mangles parent links would leave a repository that looks fine at HEAD and
// has lost everything behind it.
func TestPrunePreservesHistoryOfSurvivors(t *testing.T) {
	r := reclaimRepo(t)
	writeCommit(t, r, "keep.md", "v1", "one")
	writeCommit(t, r, "keep.md", "v2", "two")
	writeCommit(t, r, "gone.md", "doomed", "three")
	writeCommit(t, r, "keep.md", "v3", "four")
	removeCommit(t, r, "gone.md", "five")

	if _, err := r.Prune([]string{"gone.md"}); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	revs, err := r.History("keep.md", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(revs) < 3 {
		t.Fatalf("keep.md has %d revisions after prune, want at least 3", len(revs))
	}
}

func TestPruneMapRoundTrip(t *testing.T) {
	dir := t.TempDir()

	got, err := ReadPruneMap(dir)
	if err != nil {
		t.Fatalf("reading a missing prune-map must not error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("missing prune-map yielded %d entries", len(got))
	}

	if err := AppendPruneMap(dir, "aaa", "bbb"); err != nil {
		t.Fatal(err)
	}
	if err := AppendPruneMap(dir, "bbb", "ccc"); err != nil {
		t.Fatal(err)
	}

	got, err = ReadPruneMap(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got["aaa"] != "bbb" || got["bbb"] != "ccc" {
		t.Fatalf("prune-map = %+v", got)
	}
}
