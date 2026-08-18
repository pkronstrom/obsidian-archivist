package repo

import (
	"os"
	"path/filepath"
	"testing"
)

// writeCommit writes a file and commits, returning the new head.
func writeCommit(t *testing.T, r *Repo, path, content, msg string) string {
	t.Helper()
	full := filepath.Join(r.WorkTree(), path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := r.Commit(msg)
	if err != nil {
		t.Fatalf("commit %q: %v", msg, err)
	}
	return h
}

func removeCommit(t *testing.T, r *Repo, path, msg string) string {
	t.Helper()
	if err := os.Remove(filepath.Join(r.WorkTree(), path)); err != nil {
		t.Fatal(err)
	}
	h, err := r.Commit(msg)
	if err != nil {
		t.Fatalf("commit %q: %v", msg, err)
	}
	return h
}

// reclaimRepo is newRepo narrowed to what these tests need. newRepo already
// exists in this package and returns three values; wrapping it keeps one
// definition of the fixture.
func reclaimRepo(t *testing.T) *Repo {
	t.Helper()
	r, _, _ := newRepo(t)
	return r
}

func TestReclaimableFindsADeletedFile(t *testing.T) {
	r := reclaimRepo(t)
	writeCommit(t, r, "keep.md", "kept forever", "add keep")
	writeCommit(t, r, "attach/big.pdf", "pretend this is nine megabytes", "add big")
	removeCommit(t, r, "attach/big.pdf", "delete big")

	got, err := r.Reclaimable()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d reclaimable, want 1: %+v", len(got), got)
	}
	if got[0].Path != "attach/big.pdf" {
		t.Errorf("Path = %q", got[0].Path)
	}
	if got[0].Bytes <= 0 {
		t.Errorf("Bytes = %d, want > 0", got[0].Bytes)
	}
	if got[0].Deleted.IsZero() {
		t.Error("Deleted was not recorded")
	}
	if got[0].Added.After(got[0].Deleted) {
		t.Error("Added is after Deleted")
	}
}

// A file still present at HEAD costs nothing to reclaim, however many times it
// has been rewritten: pruning it would destroy live content.
func TestLivePathsAreNeverReclaimable(t *testing.T) {
	r := reclaimRepo(t)
	writeCommit(t, r, "note.md", "v1", "one")
	writeCommit(t, r, "note.md", "v2", "two")
	writeCommit(t, r, "note.md", "v3", "three")

	got, err := r.Reclaimable()
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range got {
		if g.Path == "note.md" {
			t.Fatalf("a live path was listed as reclaimable: %+v", g)
		}
	}
}

// Every revision of a deleted file counts, not just the last one. This is the
// case the whole feature exists for: a big attachment rewritten repeatedly and
// then deleted.
func TestAllVersionsOfADeletedFileCount(t *testing.T) {
	r := reclaimRepo(t)
	writeCommit(t, r, "big.bin", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "v1")
	writeCommit(t, r, "big.bin", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "v2")
	writeCommit(t, r, "big.bin", "cccccccccccccccccccccccccccccc", "v3")
	removeCommit(t, r, "big.bin", "gone")

	got, err := r.Reclaimable()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d, want 1: %+v", len(got), got)
	}
	if got[0].Versions != 3 {
		t.Errorf("Versions = %d, want 3", got[0].Versions)
	}
}

// Git stores content once. A deleted file whose bytes are identical to a live
// file costs nothing, and reporting it would promise space that pruning cannot
// deliver.
func TestBlobsSharedWithLivePathsAreNotCounted(t *testing.T) {
	r := reclaimRepo(t)
	const shared = "identical content in both files"
	writeCommit(t, r, "live.md", shared, "add live")
	writeCommit(t, r, "copy.md", shared, "add copy")
	removeCommit(t, r, "copy.md", "delete copy")

	got, err := r.Reclaimable()
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range got {
		if g.Path == "copy.md" {
			t.Fatalf("a blob still referenced by a live path was counted: %+v", g)
		}
	}
}

// A path deleted and later re-created is live again, so it is not reclaimable.
func TestRecreatedPathIsNotReclaimable(t *testing.T) {
	r := reclaimRepo(t)
	writeCommit(t, r, "note.md", "first life", "add")
	removeCommit(t, r, "note.md", "delete")
	writeCommit(t, r, "note.md", "second life", "re-add")

	got, err := r.Reclaimable()
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range got {
		if g.Path == "note.md" {
			t.Fatalf("a re-created path was listed as reclaimable: %+v", g)
		}
	}
}

func TestEmptyRepoReportsNothing(t *testing.T) {
	r := reclaimRepo(t)
	got, err := r.Reclaimable()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d reclaimable in an empty repo", len(got))
	}
}

// Biggest first: the report exists to answer "is this worth doing", and the
// answer lives in the top few rows.
func TestReclaimableIsSortedBySizeDescending(t *testing.T) {
	r := reclaimRepo(t)
	writeCommit(t, r, "small.bin", "tiny", "add small")
	writeCommit(t, r, "large.bin", "this content is considerably longer than the other one", "add large")
	removeCommit(t, r, "small.bin", "del small")
	removeCommit(t, r, "large.bin", "del large")

	got, err := r.Reclaimable()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d, want 2: %+v", len(got), got)
	}
	if got[0].Path != "large.bin" {
		t.Errorf("first row is %q, want large.bin", got[0].Path)
	}
}
