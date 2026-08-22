package reconcile

import (
	"errors"
	"testing"

	"github.com/pkronstrom/obsidian-archivist/internal/pins"
	"github.com/pkronstrom/obsidian-archivist/internal/repo"
	"github.com/pkronstrom/obsidian-archivist/internal/vault"
)

// newPinFixture is newRec plus one committed note, so a file pin has something
// real to name.
func newPinFixture(t *testing.T) (*Reconciler, *vault.Vault, *repo.Repo) {
	t.Helper()
	rc, v, r := newRec(t)
	if err := v.Write("Note.md", []byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit("first"); err != nil {
		t.Fatal(err)
	}
	return rc, v, r
}

func TestPinAppendsAndCommits(t *testing.T) {
	rc, v, r := newPinFixture(t)

	head, _ := r.Head()
	entry, newHead, err := rc.Pin(head, "before reorg", "")
	if err != nil {
		t.Fatal(err)
	}
	if newHead == head {
		t.Fatal("pin did not produce a commit")
	}
	content, err := v.Read(pins.File)
	if err != nil {
		t.Fatal(err)
	}
	got := pins.Parse(content)
	if len(got) != 1 || got[0].ID != entry.ID {
		t.Fatalf("pins file holds %+v", got)
	}
}

// The precondition is the whole point of expectedHead: "pin this version" is a
// claim about a specific tree, and with the plugin's sync debounce a caller
// that skipped the check would silently pin an older state than the note on
// the user's screen.
func TestPinRefusesStaleHead(t *testing.T) {
	rc, v, r := newPinFixture(t)

	stale, _ := r.Head()
	v.Write("Other.md", []byte("moved on\n"))
	if _, err := r.Commit("someone else committed"); err != nil {
		t.Fatal(err)
	}

	_, head, err := rc.Pin(stale, "too late", "")
	if !errors.Is(err, ErrPinHeadMismatch) {
		t.Fatalf("got %v, want ErrPinHeadMismatch", err)
	}
	if current, _ := r.Head(); head != current {
		t.Errorf("mismatch returned head %q, want the current %q so the caller can retry", head, current)
	}
	if _, err := v.Read(pins.File); err == nil {
		t.Error("a refused pin still wrote the file")
	}
}

// A file pin naming a path that does not exist would mint a pin that can never
// be opened. Caught under the lock, so the answer cannot go stale between the
// check and the commit.
func TestPinRefusesMissingPath(t *testing.T) {
	rc, _, r := newPinFixture(t)

	head, _ := r.Head()
	if _, _, err := rc.Pin(head, "ghost", "NoSuchNote.md"); !errors.Is(err, ErrPinPathMissing) {
		t.Fatalf("got %v, want ErrPinPathMissing", err)
	}
}

// Two pins in a row must both survive: the second appends to the first rather
// than replacing the file.
func TestPinAccumulates(t *testing.T) {
	rc, v, r := newPinFixture(t)

	head, _ := r.Head()
	first, head, err := rc.Pin(head, "one", "Note.md")
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := rc.Pin(head, "two", "Note.md")
	if err != nil {
		t.Fatal(err)
	}
	content, _ := v.Read(pins.File)
	got := pins.Parse(content)
	if len(got) != 2 || got[0].ID != first.ID || got[1].ID != second.ID {
		t.Fatalf("want both pins in order, got %+v", got)
	}
}
