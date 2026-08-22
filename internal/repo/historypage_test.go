package repo

import (
	"errors"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/pkronstrom/obsidian-archivist/internal/vault"
)

// Paging by commit rather than by offset is the point: the history grows at
// the newest end, so an offset cursor would shift under a caller between pages
// and silently skip or repeat a revision.
func TestHistoryPageWalksWithoutSkippingOrRepeating(t *testing.T) {
	r, v, _ := newRepo(t)
	for i := 0; i < 7; i++ {
		v.Write("Note.md", []byte{byte('a' + i), '\n'})
		r.Commit("edit")
	}

	var seen []string
	before := ""
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("paging did not terminate")
		}
		revs, more, err := r.HistoryPage("Note.md", 3, before)
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		for _, rev := range revs {
			seen = append(seen, rev.Commit)
		}
		if !more {
			break
		}
		before = revs[len(revs)-1].Commit
	}

	if len(seen) != 7 {
		t.Fatalf("paged %d revisions, want 7", len(seen))
	}
	uniq := map[string]bool{}
	for _, c := range seen {
		if uniq[c] {
			t.Fatalf("commit %s returned twice", c)
		}
		uniq[c] = true
	}
}

// An unknown cursor must be an error rather than an empty page: an empty page
// reads as "that is all of them" and would silently end a listing that broke.
func TestHistoryPageRejectsUnknownCursor(t *testing.T) {
	r, v, _ := newRepo(t)
	v.Write("Note.md", []byte("one\n"))
	r.Commit("first")

	_, _, err := r.HistoryPage("Note.md", 10, "0000000000000000000000000000000000000000")
	if !errors.Is(err, ErrUnknownCursor) {
		t.Fatalf("got %v, want ErrUnknownCursor", err)
	}
}

// Without HasMore a caller cannot tell a complete list from a truncated one.
func TestHistoryPageReportsMore(t *testing.T) {
	r, v, _ := newRepo(t)
	for i := 0; i < 3; i++ {
		v.Write("Note.md", []byte{byte('a' + i), '\n'})
		r.Commit("edit")
	}

	revs, more, err := r.HistoryPage("Note.md", 2, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(revs) != 2 || !more {
		t.Fatalf("got %d revs more=%v, want 2 and true", len(revs), more)
	}

	revs, more, err = r.HistoryPage("Note.md", 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(revs) != 3 || more {
		t.Fatalf("got %d revs more=%v, want 3 and false", len(revs), more)
	}
}

// The walk is over the whole repository history filtered by path, so an
// uncapped limit lets one request traverse every commit in the vault.
func TestHistoryPageCapsLimit(t *testing.T) {
	r, v, _ := newRepo(t)
	v.Write("Note.md", []byte("one\n"))
	r.Commit("first")

	if _, _, err := r.HistoryPage("Note.md", 1_000_000, ""); err != nil {
		t.Fatal(err)
	}
}

// A path tracked in HEAD that today's rules exclude is stranded: Commit
// refuses its edits AND its deletion, so it can never leave history through
// ordinary use. git status shows nothing for an unmodified tracked file, which
// is exactly why this is computed from the snapshot instead -- the failure is
// invisible otherwise.
func TestCheckReportsStrandedTrackedPaths(t *testing.T) {
	r, v, _ := newRepo(t)
	// The production wiring; a bare repo.Open allows everything.
	r.SetSyncable(func(p string) bool { return !vault.Skip(p) })
	v.Write("Note.md", []byte("ordinary\n"))
	v.Write("Scratch.local.md", []byte("should never have been tracked\n"))
	// Commit through go-git directly, bypassing the exclusion, to reproduce a
	// file that predates the rule.
	wt, err := r.git.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := wt.AddGlob("."); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Commit("before the rule existed", &git.CommitOptions{Author: signature()}); err != nil {
		t.Fatal(err)
	}

	rep, err := r.Check()
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Stranded) != 1 || rep.Stranded[0] != "Scratch.local.md" {
		t.Fatalf("Stranded = %v, want [Scratch.local.md]", rep.Stranded)
	}
	if rep.OK {
		t.Error("a stranded path must not report OK: nothing will ever remove it")
	}
}

// A commit that exists but never touched this path is not a position in this
// path's history. Accepting it would hand back a page from the middle of a
// listing the caller never asked for.
func TestHistoryPageRejectsCursorFromAnotherPath(t *testing.T) {
	r, v, _ := newRepo(t)
	v.Write("Note.md", []byte("a\n"))
	r.Commit("note")
	v.Write("Other.md", []byte("x\n"))
	elsewhere, err := r.Commit("unrelated commit that never touched Note.md")
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := r.HistoryPage("Note.md", 10, elsewhere); !errors.Is(err, ErrUnknownCursor) {
		t.Fatalf("got %v, want ErrUnknownCursor", err)
	}
}
