package repo

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/pkronstrom/obsidian-archivist/internal/vault"
)

func newRepo(t *testing.T) (*Repo, *vault.Vault, string) {
	t.Helper()
	base := t.TempDir()
	work := filepath.Join(base, "vault")
	gitdir := filepath.Join(base, "git")
	v, err := vault.New(work)
	if err != nil {
		t.Fatalf("vault.New: %v", err)
	}
	r, err := Open(work, gitdir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { v.Close() })
	return r, v, gitdir
}

func TestOpenIsIdempotentAndKeepsHistoryOutOfTheVault(t *testing.T) {
	r, v, gitdir := newRepo(t)

	// go-git writes a `.git` POINTER FILE into the worktree -- the same
	// mechanism git uses for linked worktrees and submodules. That is fine and
	// mildly useful: plain `git log` works in the vault directory without
	// --git-dir. What matters is that no history lives there, and that the
	// pointer never syncs.
	fi, err := os.Lstat(filepath.Join(v.Dir(), ".git"))
	if err != nil {
		t.Fatalf(".git pointer missing: %v", err)
	}
	if fi.IsDir() {
		t.Error(".git is a directory inside the vault; history must live outside")
	}
	if fi.Size() > 1024 {
		t.Errorf(".git pointer is %d bytes, expected a small gitdir reference", fi.Size())
	}
	if !vault.Skip(".git") {
		t.Error("the .git pointer is not excluded from sync")
	}
	if _, err := os.Stat(filepath.Join(gitdir, "HEAD")); err != nil {
		t.Errorf("git dir not initialised: %v", err)
	}
	// Re-opening an existing repo must not wipe it.
	if _, err := Open(v.Dir(), gitdir); err != nil {
		t.Errorf("second Open: %v", err)
	}
	_ = r
}

func TestHeadIsEmptyBeforeAnyCommit(t *testing.T) {
	r, _, _ := newRepo(t)
	h, err := r.Head()
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if h != "" {
		t.Errorf("Head = %q, want empty on a fresh repo", h)
	}
}

func TestCommitAdvancesHead(t *testing.T) {
	r, v, _ := newRepo(t)
	v.Write("notes/a.md", []byte("hello\n"))
	h1, err := r.Commit("first")
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if h1 == "" {
		t.Fatal("Commit returned an empty hash")
	}
	v.Write("notes/a.md", []byte("hello v2\n"))
	h2, err := r.Commit("second")
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if h1 == h2 {
		t.Error("head did not advance")
	}
}

func TestCommitWithNoChangesIsANoOp(t *testing.T) {
	r, v, _ := newRepo(t)
	v.Write("a.md", []byte("x"))
	h1, _ := r.Commit("first")
	h2, err := r.Commit("nothing changed")
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if h2 != h1 {
		t.Errorf("empty commit created: %q -> %q", h1, h2)
	}
}

func TestSnapshotListsPathsHashesAndSizes(t *testing.T) {
	r, v, _ := newRepo(t)
	v.Write("notes/a.md", []byte("hello\n"))
	v.Write("notes/sub/b.md", []byte("world!\n"))
	head, _ := r.Commit("first")

	snap, err := r.Snapshot(head)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(snap) != 2 {
		t.Fatalf("Snapshot has %d entries, want 2: %+v", len(snap), snap)
	}
	a, ok := snap["notes/a.md"]
	if !ok {
		t.Fatal("notes/a.md missing from snapshot")
	}
	if a.Size != 6 {
		t.Errorf("size = %d, want 6", a.Size)
	}
	if a.Hash == "" {
		t.Error("empty hash in snapshot")
	}
}

func TestSnapshotOfEmptyHeadIsEmpty(t *testing.T) {
	r, _, _ := newRepo(t)
	snap, err := r.Snapshot("")
	if err != nil {
		t.Fatalf("Snapshot(\"\"): %v", err)
	}
	if len(snap) != 0 {
		t.Errorf("Snapshot = %+v, want empty", snap)
	}
}

func TestChangesReportsModifyDeleteInsert(t *testing.T) {
	r, v, _ := newRepo(t)
	v.Write("a.md", []byte("one\n"))
	v.Write("b.md", []byte("two\n"))
	c1, _ := r.Commit("first")

	v.Write("a.md", []byte("one v2\n"))
	v.Remove("b.md")
	v.Write("c.md", []byte("three\n"))
	c2, _ := r.Commit("second")

	changes, err := r.Changes(c1, c2)
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	got := map[string]string{}
	for _, ch := range changes {
		got[ch.Path] = ch.Op
	}
	want := map[string]string{"a.md": "put", "b.md": "del", "c.md": "put"}
	if len(got) != len(want) {
		t.Fatalf("Changes = %+v, want %+v", got, want)
	}
	for p, op := range want {
		if got[p] != op {
			t.Errorf("%s: op = %q, want %q", p, got[p], op)
		}
	}
}

// A client bootstrapping from nothing must receive the whole tree as puts.
func TestChangesFromEmptyIsAFullListing(t *testing.T) {
	r, v, _ := newRepo(t)
	v.Write("a.md", []byte("one\n"))
	v.Write("b.md", []byte("two\n"))
	head, _ := r.Commit("first")

	changes, err := r.Changes("", head)
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(changes) != 2 {
		t.Fatalf("Changes = %+v, want 2 puts", changes)
	}
}

// A cursor git has never heard of must be a distinguishable error, so the API
// can answer 409 and tell the client to re-bootstrap rather than 500.
func TestChangesFromUnknownCursorIsErrUnknownBase(t *testing.T) {
	r, v, _ := newRepo(t)
	v.Write("a.md", []byte("one\n"))
	head, _ := r.Commit("first")
	_, err := r.Changes("0123456789012345678901234567890123456789", head)
	if err == nil {
		t.Fatal("want an error for an unknown base, got nil")
	}
	if !IsUnknownBase(err) {
		t.Errorf("err = %v, want it to satisfy IsUnknownBase", err)
	}
}

func TestBlobRoundTripAndHave(t *testing.T) {
	r, _, _ := newRepo(t)
	content := []byte("some uploaded content\n")

	hash, err := r.WriteBlob(content)
	if err != nil {
		t.Fatalf("WriteBlob: %v", err)
	}
	if !r.HasBlob(hash) {
		t.Error("HasBlob = false right after WriteBlob")
	}
	got, err := r.ReadBlob(hash)
	if err != nil {
		t.Fatalf("ReadBlob: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("ReadBlob = %q, want %q", got, content)
	}
	// Content-addressed: writing the same bytes twice yields the same address.
	again, _ := r.WriteBlob(content)
	if again != hash {
		t.Errorf("WriteBlob not idempotent: %q vs %q", hash, again)
	}
	if r.HasBlob("0123456789012345678901234567890123456789") {
		t.Error("HasBlob = true for an absent object")
	}
}

func TestReadFileAtRevision(t *testing.T) {
	r, v, _ := newRepo(t)
	v.Write("a.md", []byte("first\n"))
	c1, _ := r.Commit("first")
	v.Write("a.md", []byte("second\n"))
	r.Commit("second")

	old, err := r.ReadAt(c1, "a.md")
	if err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if string(old) != "first\n" {
		t.Errorf("ReadAt = %q, want the historical content", old)
	}
}

// Not a dependency -- an interop guarantee. A human debugging this must be able
// to point real git at the repo even though nothing at runtime needs git.
func TestGitCLIInterop(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	r, v, gitdir := newRepo(t)
	v.Write("a.md", []byte("hello\n"))
	if _, err := r.Commit("written by go-git"); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("git", "--git-dir="+gitdir, "--work-tree="+v.Dir(),
		"log", "--oneline").CombinedOutput()
	if err != nil {
		t.Fatalf("git log failed: %v: %s", err, out)
	}
	if len(out) == 0 {
		t.Error("git log produced nothing")
	}
	t.Logf("git log: %s", out)
}

// AddWithOptions{All:true} staged the entire working tree, so .obsidian/ landed
// in git and would have reached every device -- while the watcher, the plugin
// and every document claimed dotfiles were excluded. Verified against a running
// server before the fix: the snapshot contained .obsidian/appearance.json.
func TestCommitStagesOnlySyncablePaths(t *testing.T) {
	r, v, _ := newRepo(t)
	r.SetSyncable(func(p string) bool { return !vault.Skip(p) })

	if err := os.MkdirAll(filepath.Join(v.Dir(), ".obsidian"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(v.Dir(), ".obsidian/appearance.json"), []byte("{}"), 0o644)
	v.Write("note.md", []byte("real content\n"))

	head, err := r.Commit("mixed")
	if err != nil {
		t.Fatal(err)
	}
	snap, err := r.Snapshot(head)
	if err != nil {
		t.Fatal(err)
	}
	if _, leaked := snap[".obsidian/appearance.json"]; leaked {
		t.Errorf("dotfile entered git: %+v", snap)
	}
	if _, ok := snap["note.md"]; !ok {
		t.Errorf("real note missing: %+v", snap)
	}
}

// Staging per path must still record deletions.
func TestCommitStagesDeletions(t *testing.T) {
	r, v, _ := newRepo(t)
	r.SetSyncable(func(p string) bool { return !vault.Skip(p) })
	v.Write("gone.md", []byte("x\n"))
	r.Commit("add")
	v.Remove("gone.md")
	head, err := r.Commit("remove")
	if err != nil {
		t.Fatal(err)
	}
	snap, _ := r.Snapshot(head)
	if _, still := snap["gone.md"]; still {
		t.Errorf("deletion was not staged: %+v", snap)
	}
}

// The event stream and /v1/changes must describe a change identically, or an
// agent written against one breaks on the other. Caught by running the
// reference worker: a .md file arriving through catch-up had no `ext` and was
// skipped as "not text".
func TestChangesCarryTriageMetadata(t *testing.T) {
	r, v, _ := newRepo(t)
	r.SetSyncable(func(p string) bool { return !vault.Skip(p) })
	v.Write("notes/a.md", []byte("# a note\n"))
	v.Write("att/scan.pdf", []byte("\x00\x01not text at all"))
	head, err := r.Commit("first")
	if err != nil {
		t.Fatal(err)
	}

	changes, err := r.Changes("", head)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]Change{}
	for _, c := range changes {
		byPath[c.Path] = c
	}

	md := byPath["notes/a.md"]
	if md.Ext != "md" || md.Kind != "text" {
		t.Errorf("markdown described as ext=%q kind=%q", md.Ext, md.Kind)
	}
	pdf := byPath["att/scan.pdf"]
	if pdf.Ext != "pdf" || pdf.Kind != "binary" {
		t.Errorf("binary described as ext=%q kind=%q", pdf.Ext, pdf.Kind)
	}
}
