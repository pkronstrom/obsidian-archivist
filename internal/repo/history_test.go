package repo

import "testing"

func TestHistoryListsOnlyCommitsTouchingThePath(t *testing.T) {
	r, v, _ := newRepo(t)
	v.Write("a.md", []byte("a1\n"))
	r.Commit("first")
	// Two commits that do not touch a.md at all.
	v.Write("other.md", []byte("x\n"))
	r.Commit("unrelated one")
	v.Write("other.md", []byte("y\n"))
	r.Commit("unrelated two")
	v.Write("a.md", []byte("a2\n"))
	r.Commit("second edit of a")

	revs, err := r.History("a.md", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(revs) != 2 {
		t.Fatalf("History = %d revisions, want 2 (unrelated commits must not appear): %+v", len(revs), revs)
	}
	if revs[0].Message != "second edit of a" {
		t.Errorf("newest first expected, got %q", revs[0].Message)
	}
	if revs[0].Short == "" || len(revs[0].Short) != 8 {
		t.Errorf("short hash = %q", revs[0].Short)
	}
}

func TestHistoryRespectsLimit(t *testing.T) {
	r, v, _ := newRepo(t)
	for i := 0; i < 6; i++ {
		v.Write("a.md", []byte{byte('a' + i), '\n'})
		r.Commit("edit")
	}
	revs, err := r.History("a.md", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(revs) != 3 {
		t.Errorf("got %d, want 3", len(revs))
	}
}

func TestReadAtGivesTheHistoricalContent(t *testing.T) {
	r, v, _ := newRepo(t)
	v.Write("a.md", []byte("original\n"))
	r.Commit("first")
	v.Write("a.md", []byte("rewritten\n"))
	r.Commit("second")

	revs, _ := r.History("a.md", 10)
	old, err := r.ReadAt(revs[1].Commit, "a.md")
	if err != nil {
		t.Fatal(err)
	}
	if string(old) != "original\n" {
		t.Errorf("ReadAt = %q, want the old content", old)
	}
	// Inspecting must not touch the working tree.
	cur, _ := v.Read("a.md")
	if string(cur) != "rewritten\n" {
		t.Errorf("inspecting a revision modified the vault: %q", cur)
	}
}

func TestResolveAcceptsShortHashesAndHead(t *testing.T) {
	r, v, _ := newRepo(t)
	v.Write("a.md", []byte("x\n"))
	full, _ := r.Commit("first")

	for _, in := range []string{"", "head", "HEAD", full, full[:8]} {
		got, err := r.Resolve(in)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", in, err)
		}
		if got != full {
			t.Errorf("Resolve(%q) = %s, want %s", in, got, full)
		}
	}
	if _, err := r.Resolve("deadbee"); err == nil {
		t.Error("Resolve accepted a hash that does not exist")
	}
}

func TestCheckIsCleanAfterCommitAndNoticesDrift(t *testing.T) {
	r, v, _ := newRepo(t)
	v.Write("a.md", []byte("x\n"))
	r.Commit("first")

	rep, err := r.Check()
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK {
		t.Errorf("Check not OK on a freshly committed tree: %+v", rep)
	}
	if rep.Files != 1 {
		t.Errorf("Files = %d, want 1", rep.Files)
	}

	// Delete behind the server's back: git holds a file the vault does not.
	v.Remove("a.md")
	rep, _ = r.Check()
	if rep.OK {
		t.Error("Check reported OK after a file vanished from the vault")
	}
	if len(rep.Missing) != 1 || rep.Missing[0] != "a.md" {
		t.Errorf("Missing = %v, want [a.md]", rep.Missing)
	}
}
