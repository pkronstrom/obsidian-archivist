package repo

import (
	"testing"

	"github.com/pkronstrom/obsidian-archivist/internal/vault"
)

func TestDeletedListsWhatHeadNoLongerHolds(t *testing.T) {
	r, v, _ := newRepo(t)
	r.SetSyncable(func(p string) bool { return !vault.Skip(p) })

	v.Write("Keep.md", []byte("still here\n"))
	v.Write("Gone.md", []byte("the content that must come back\n"))
	v.Write("Notes/Nested.md", []byte("nested\n"))
	if _, err := r.Commit("sync from work-mac"); err != nil {
		t.Fatal(err)
	}
	v.Remove("Gone.md")
	v.Remove("Notes/Nested.md")
	if _, err := r.Commit("sync from iPhone"); err != nil {
		t.Fatal(err)
	}

	gone, err := r.Deleted()
	if err != nil {
		t.Fatal(err)
	}
	if len(gone) != 2 {
		t.Fatalf("got %d deleted paths, want 2: %+v", len(gone), gone)
	}

	byPath := map[string]int{}
	for i, d := range gone {
		byPath[d.Path] = i
	}
	d, ok := byPath["Gone.md"]
	if !ok {
		t.Fatalf("Gone.md missing from %+v", gone)
	}
	if gone[d].Device != "iPhone" {
		t.Errorf("device = %q, want iPhone (who deleted it)", gone[d].Device)
	}

	// The whole point: the revision handed back must still HOLD the content.
	// Returning the deleting commit would give the caller a revision the file
	// is by definition absent from, and every restore would 404.
	content, err := r.ReadAt(gone[d].Revision, "Gone.md")
	if err != nil {
		t.Fatalf("the revision offered for restore cannot be read: %v", err)
	}
	if string(content) != "the content that must come back\n" {
		t.Errorf("restored content = %q", content)
	}
}

// A path deleted and later re-created is not lost, so listing it would send
// someone to recover a file they already have.
func TestDeletedSkipsPathsThatCameBack(t *testing.T) {
	r, v, _ := newRepo(t)
	r.SetSyncable(func(p string) bool { return !vault.Skip(p) })

	v.Write("Cycle.md", []byte("one\n"))
	r.Commit("first")
	v.Remove("Cycle.md")
	r.Commit("deleted")
	v.Write("Cycle.md", []byte("two\n"))
	r.Commit("recreated")

	gone, err := r.Deleted()
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range gone {
		if d.Path == "Cycle.md" {
			t.Fatalf("a path that exists at HEAD must not be listed as deleted: %+v", d)
		}
	}
}

// Excluded paths were never in the synced set, so offering to "restore" one
// would offer a file the server refuses to accept back.
func TestDeletedIgnoresExcludedPaths(t *testing.T) {
	r, v, _ := newRepo(t)
	v.Write("Scratch.local.md", []byte("local only\n"))
	v.Write("Real.md", []byte("real\n"))
	// Commit with everything allowed, so the excluded path gets into history
	// the way a pre-rule file would have.
	r.Commit("before the rule")
	v.Remove("Scratch.local.md")
	r.Commit("removed both")

	// Now apply the production predicate.
	r.SetSyncable(func(p string) bool { return !vault.Skip(p) })
	gone, err := r.Deleted()
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range gone {
		if d.Path == "Scratch.local.md" {
			t.Fatal("an excluded path must not be offered for restore")
		}
	}
}
