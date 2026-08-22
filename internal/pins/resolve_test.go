package pins

import (
	"path/filepath"
	"testing"

	"github.com/pkronstrom/obsidian-archivist/internal/repo"
	"github.com/pkronstrom/obsidian-archivist/internal/vault"
)

func fixture(t *testing.T) (*vault.Vault, *repo.Repo, *Resolver) {
	t.Helper()
	base := t.TempDir()
	work := filepath.Join(base, "vault")
	v, err := vault.New(work)
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Open(work, filepath.Join(base, "git"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { v.Close() })
	return v, r, NewResolver(r)
}

// addPin appends an entry and commits, the way Reconciler.Pin does.
func addPin(t *testing.T, v *vault.Vault, r *repo.Repo, e Entry) string {
	t.Helper()
	existing, _ := v.Read(File)
	next, err := Append(existing, e)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Write(File, next); err != nil {
		t.Fatal(err)
	}
	head, err := r.Commit("pin: " + e.ID)
	if err != nil {
		t.Fatal(err)
	}
	return head
}

// The core claim of the whole design: a pin's snapshot is the commit that
// FIRST introduced its id. Every later commit still contains the line, so
// taking the newest match would collapse every pin onto the most recent pin
// commit -- and each pin would silently name the wrong tree.
func TestResolvesEachPinToItsOwnCommit(t *testing.T) {
	v, r, rv := fixture(t)
	v.Write("Note.md", []byte("v1\n"))
	r.Commit("first")

	firstCommit := addPin(t, v, r, Entry{ID: "a1", Name: "one", Path: "Note.md"})

	v.Write("Note.md", []byte("v2\n"))
	r.Commit("edit")
	secondCommit := addPin(t, v, r, Entry{ID: "b2", Name: "two", Path: "Note.md"})

	got, _, err := rv.List("Note.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d pins, want 2", len(got))
	}
	if got[0].Commit != firstCommit {
		t.Errorf("first pin resolved to %s, want its own commit %s", got[0].Commit, firstCommit)
	}
	if got[1].Commit != secondCommit {
		t.Errorf("second pin resolved to %s, want %s", got[1].Commit, secondCommit)
	}
	if got[0].Commit == got[1].Commit {
		t.Fatal("both pins resolved to the same commit")
	}
	// And the snapshot each names must hold the content of its own moment.
	content, err := r.ReadAt(got[0].Commit, "Note.md")
	if err != nil || string(content) != "v1\n" {
		t.Errorf("first pin's snapshot = %q, %v; want v1", content, err)
	}
}

// Two pins added in one commit is legal -- a hand edit can do it -- so the
// mapping is id to commit, never commit to id.
func TestManyPinsMayShareOneCommit(t *testing.T) {
	v, r, rv := fixture(t)
	v.Write("Note.md", []byte("v1\n"))
	r.Commit("first")

	existing, _ := v.Read(File)
	next, _ := Append(existing, Entry{ID: "a1", Name: "one"})
	next, _ = Append(next, Entry{ID: "b2", Name: "two"})
	v.Write(File, next)
	shared, _ := r.Commit("two pins at once")

	got, _, err := rv.List("")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Commit != shared || got[1].Commit != shared {
		t.Fatalf("both pins should resolve to %s: %+v", shared, got)
	}
}

// A pin whose path was deleted and pruned survives as a record but cannot be
// opened. Reporting that beats a 404 on click.
func TestUnavailableWhenPathMissingAtSnapshot(t *testing.T) {
	v, r, rv := fixture(t)
	v.Write("Note.md", []byte("v1\n"))
	r.Commit("first")
	addPin(t, v, r, Entry{ID: "a1", Name: "real", Path: "Note.md"})
	// A pin naming a path that never existed at its snapshot: the same shape
	// the UI must handle after a prune drops the blobs of a deleted path.
	addPin(t, v, r, Entry{ID: "b2", Name: "ghost", Path: "Gone.md"})

	got, _, err := rv.List("")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d", len(got))
	}
	if !got[0].Available {
		t.Error("a pin whose path exists should be available")
	}
	if got[1].Available {
		t.Error("a pin whose path is absent at its snapshot must not be available")
	}
}

// path filters to one file; "*" asks for vault-wide pins only. The file modal
// must not show vault-wide restore points -- they are not about that note.
func TestListFilters(t *testing.T) {
	v, r, rv := fixture(t)
	v.Write("Note.md", []byte("v1\n"))
	v.Write("Other.md", []byte("x\n"))
	r.Commit("first")
	addPin(t, v, r, Entry{ID: "a1", Name: "file pin", Path: "Note.md"})
	addPin(t, v, r, Entry{ID: "b2", Name: "vault pin"})
	addPin(t, v, r, Entry{ID: "c3", Name: "other file", Path: "Other.md"})

	byFile, _, _ := rv.List("Note.md")
	if len(byFile) != 1 || byFile[0].ID != "a1" {
		t.Fatalf("file filter returned %+v", byFile)
	}
	vaultWide, _, _ := rv.List("*")
	if len(vaultWide) != 1 || vaultWide[0].ID != "b2" {
		t.Fatalf("vault-wide filter returned %+v", vaultWide)
	}
	all, _, _ := rv.List("")
	if len(all) != 3 {
		t.Fatalf("unfiltered returned %d, want 3", len(all))
	}
}

// The cache is keyed by head: a pin added after a listing must appear without
// a restart, or the modal would show a stale set for the rest of the session.
func TestCacheInvalidatesWhenHeadMoves(t *testing.T) {
	v, r, rv := fixture(t)
	v.Write("Note.md", []byte("v1\n"))
	r.Commit("first")
	addPin(t, v, r, Entry{ID: "a1", Name: "one", Path: "Note.md"})

	if got, _, _ := rv.List("Note.md"); len(got) != 1 {
		t.Fatalf("got %d", len(got))
	}
	addPin(t, v, r, Entry{ID: "b2", Name: "two", Path: "Note.md"})
	got, _, _ := rv.List("Note.md")
	if len(got) != 2 {
		t.Fatalf("cache went stale: got %d pins after adding a second", len(got))
	}
	if got[1].Commit == "" {
		t.Error("the new pin resolved to no commit")
	}
}

// An empty vault has no pins file. That is a vault nobody has pinned in, not
// an error.
func TestNoPinsFileIsEmptyNotAnError(t *testing.T) {
	v, r, rv := fixture(t)
	v.Write("Note.md", []byte("v1\n"))
	r.Commit("first")

	got, head, err := rv.List("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 || head == "" {
		t.Fatalf("got %d pins at head %q", len(got), head)
	}
}
