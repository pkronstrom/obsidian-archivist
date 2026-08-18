package repo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

	if err := WritePruneMap(dir, map[string]string{"aaa": "bbb", "bbb": "ccc"}); err != nil {
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

// The payoff for keeping the map: a device that synced to the old head asks
// for changes since it and gets an empty diff, not a re-bootstrap.
func TestTranslatedBaseYieldsAnEmptyDiff(t *testing.T) {
	r := reclaimRepo(t)
	writeCommit(t, r, "keep.md", "kept", "add keep")
	writeCommit(t, r, "big.bin", "bytes to reclaim", "add big")
	removeCommit(t, r, "big.bin", "delete big")

	oldHead, _ := r.Head()
	res, err := r.Prune([]string{"big.bin"})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}

	// Without the map the old head is simply gone.
	if _, err := r.Changes(oldHead, res.NewHead); err == nil {
		t.Fatal("the old head resolved before the map was installed")
	}

	r.SetPruneMap(map[string]string{res.OldHead: res.NewHead})

	changes, err := r.Changes(oldHead, res.NewHead)
	if err != nil {
		t.Fatalf("translated base still failed: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("translated base produced %d changes, want 0: %+v", len(changes), changes)
	}
}

// A repository pruned twice has two hops. A device offline across both must
// land on the current head, not the intermediate one, which no longer exists.
func TestTranslationFollowsChainedPrunes(t *testing.T) {
	r := reclaimRepo(t)
	writeCommit(t, r, "keep.md", "kept", "add keep")
	writeCommit(t, r, "a.bin", "first doomed file", "add a")
	removeCommit(t, r, "a.bin", "delete a")

	veryOld, _ := r.Head()
	first, err := r.Prune([]string{"a.bin"})
	if err != nil {
		t.Fatal(err)
	}

	writeCommit(t, r, "b.bin", "second doomed file", "add b")
	removeCommit(t, r, "b.bin", "delete b")
	second, err := r.Prune([]string{"b.bin"})
	if err != nil {
		t.Fatal(err)
	}

	// The map on disk is the authority: the second prune re-pointed the first
	// prune's entry, because the commit it named was destroyed by that second
	// rewrite. An append-only map would have left veryOld translating to a
	// commit that no longer exists -- worse than not translating, since it
	// looks like it worked.
	onDisk, err := ReadPruneMap(r.GitDir())
	if err != nil {
		t.Fatal(err)
	}
	r.SetPruneMap(onDisk)

	// The only property that matters: the target still exists.
	//
	// It is NOT necessarily the current head, and it may legitimately be
	// unchanged. A commit that predates the file the second prune removed has
	// the same tree and the same parents afterwards, so it rewrites to its own
	// hash. What the re-pointing prevents is the other case -- an entry naming
	// a commit that the second rewrite replaced, which resolves to nothing.
	target := onDisk[veryOld]
	if target == "" {
		t.Fatal("veryOld is not in the prune map at all")
	}
	if _, err := r.Snapshot(target); err != nil {
		t.Fatalf("prune-map points at a commit that does not exist: %v", err)
	}
	if _, err := r.Snapshot(r.translate(veryOld)); err != nil {
		t.Fatalf("translation landed on a missing commit: %v", err)
	}
	_ = first

	// And the whole point: an empty diff rather than a re-bootstrap.
	changes, err := r.Changes(veryOld, second.NewHead)
	if err != nil {
		t.Fatalf("translated base failed: %v", err)
	}
	for _, c := range changes {
		if c.Path == "a.bin" || c.Path == "b.bin" {
			t.Errorf("a pruned path appeared in the diff: %+v", c)
		}
	}
}

// A circular or malformed map must not hang the server.
func TestTranslationTerminatesOnACycle(t *testing.T) {
	r := reclaimRepo(t)
	r.SetPruneMap(map[string]string{"a": "b", "b": "a"})

	done := make(chan string, 1)
	go func() { done <- r.translate("a") }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("translate did not terminate on a cyclic map")
	}
}

// A base nobody pruned is returned unchanged, so the common path is untouched.
func TestTranslationLeavesUnknownBasesAlone(t *testing.T) {
	r := reclaimRepo(t)
	if got := r.translate("deadbeef"); got != "deadbeef" {
		t.Fatalf("translate rewrote an unrelated base to %q", got)
	}
}

// A second prune deletes the packfile the first one created. The storer holds
// handles to it, so without reopening, the repository reports a valid head and
// then cannot read a single commit behind it. A single-prune test cannot catch
// this: a repository go-git has only written to has no packs to invalidate.
func TestSecondPruneLeavesTheRepositoryReadable(t *testing.T) {
	r := reclaimRepo(t)
	writeCommit(t, r, "keep.md", "kept", "add keep")
	writeCommit(t, r, "a.bin", "first doomed", "add a")
	removeCommit(t, r, "a.bin", "delete a")
	if _, err := r.Prune([]string{"a.bin"}); err != nil {
		t.Fatalf("first prune: %v", err)
	}

	writeCommit(t, r, "b.bin", "second doomed", "add b")
	removeCommit(t, r, "b.bin", "delete b")
	second, err := r.Prune([]string{"b.bin"})
	if err != nil {
		t.Fatalf("second prune: %v", err)
	}

	commits, err := r.commitsOldestFirst()
	if err != nil {
		t.Fatalf("history unreadable after a second prune: %v", err)
	}
	if len(commits) == 0 {
		t.Fatal("no commits readable after a second prune")
	}
	snap, err := r.Snapshot(second.NewHead)
	if err != nil {
		t.Fatalf("snapshot unreadable after a second prune: %v", err)
	}
	if _, ok := snap["keep.md"]; !ok {
		t.Error("keep.md lost across two prunes")
	}
}
