package reconcile

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/pkronstrom/vaultsync/internal/repo"
	"github.com/pkronstrom/vaultsync/internal/vault"
)

func newRec(t *testing.T) (*Reconciler, *vault.Vault, *repo.Repo) {
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
	return New(v, r), v, r
}

// put uploads content and returns the change a client would send.
func put(t *testing.T, r *repo.Repo, path, content string) Change {
	t.Helper()
	h, err := r.WriteBlob([]byte(content))
	if err != nil {
		t.Fatal(err)
	}
	return Change{Path: path, Op: "put", Hash: h}
}

func TestPushOnEmptyRepoApplies(t *testing.T) {
	rc, v, r := newRec(t)
	head, results, err := rc.Push("", "mac", []Change{put(t, r, "a.md", "hello\n")})
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if head == "" {
		t.Fatal("head is empty after a push")
	}
	if len(results) != 1 || results[0].Status != StatusApplied {
		t.Fatalf("results = %+v, want one applied", results)
	}
	got, err := v.Read("a.md")
	if err != nil || string(got) != "hello\n" {
		t.Errorf("vault content = %q, %v", got, err)
	}
}

func TestPushAtHeadFastForwards(t *testing.T) {
	rc, _, r := newRec(t)
	head, _, _ := rc.Push("", "mac", []Change{put(t, r, "a.md", "one\n")})
	_, results, err := rc.Push(head, "mac", []Change{put(t, r, "b.md", "two\n")})
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if results[0].Status != StatusApplied {
		t.Errorf("status = %q, want applied", results[0].Status)
	}
}

// Two devices editing different files is the common case and must never merge.
func TestStalePushWithNoOverlapApplies(t *testing.T) {
	rc, _, r := newRec(t)
	base, _, _ := rc.Push("", "mac", []Change{put(t, r, "a.md", "one\n")})
	// Another device moves head forward on a different path.
	rc.Push(base, "phone", []Change{put(t, r, "b.md", "two\n")})
	// The first device pushes against the now-stale base, but on its own path.
	_, results, err := rc.Push(base, "mac", []Change{put(t, r, "a.md", "one edited\n")})
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if results[0].Status != StatusApplied {
		t.Errorf("status = %q, want applied (no overlap means no merge)", results[0].Status)
	}
}

func TestOverlappingDisjointEditsMerge(t *testing.T) {
	rc, v, r := newRec(t)
	base, _, _ := rc.Push("", "mac", []Change{put(t, r, "a.md", "one\ntwo\nthree\n")})
	// The phone changes the last line and lands first.
	rc.Push(base, "phone", []Change{put(t, r, "a.md", "one\ntwo\nTHREE\n")})
	// The Mac changes the first line, from the same base.
	_, results, err := rc.Push(base, "mac", []Change{put(t, r, "a.md", "ONE\ntwo\nthree\n")})
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if results[0].Status != StatusMerged {
		t.Fatalf("status = %q, want merged; results=%+v", results[0].Status, results)
	}
	got, _ := v.Read("a.md")
	if string(got) != "ONE\ntwo\nTHREE\n" {
		t.Errorf("merged content = %q, want both edits", got)
	}
}

func TestOverlappingSameLineProducesAConflictFile(t *testing.T) {
	rc, v, r := newRec(t)
	base, _, _ := rc.Push("", "mac", []Change{put(t, r, "a.md", "one\n")})
	rc.Push(base, "phone", []Change{put(t, r, "a.md", "from phone\n")})
	_, results, err := rc.Push(base, "mac", []Change{put(t, r, "a.md", "from mac\n")})
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if results[0].Status != StatusConflict {
		t.Fatalf("status = %q, want conflict", results[0].Status)
	}
	// The server's version stays put -- the authority does not yield.
	got, _ := v.Read("a.md")
	if string(got) != "from phone\n" {
		t.Errorf("a.md = %q, want the server version preserved", got)
	}
	// The client's version is preserved beside it, never discarded.
	if results[0].ConflictPath == "" {
		t.Fatal("no conflict path reported")
	}
	side, err := v.Read(results[0].ConflictPath)
	if err != nil {
		t.Fatalf("conflict file unreadable: %v", err)
	}
	if string(side) != "from mac\n" {
		t.Errorf("conflict file = %q, want the client version", side)
	}
	if !strings.Contains(results[0].ConflictPath, "mac") {
		t.Errorf("conflict path %q does not name the device", results[0].ConflictPath)
	}
}

func TestBinaryOverlapKeepsBothWithoutMerging(t *testing.T) {
	rc, v, r := newRec(t)
	base, _, _ := rc.Push("", "mac", []Change{put(t, r, "img.png", "\x00\x01binary one")})
	rc.Push(base, "phone", []Change{put(t, r, "img.png", "\x00\x01binary phone")})
	_, results, err := rc.Push(base, "mac", []Change{put(t, r, "img.png", "\x00\x01binary mac")})
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if results[0].Status != StatusConflict {
		t.Fatalf("status = %q, want conflict", results[0].Status)
	}
	got, _ := v.Read("img.png")
	if string(got) != "\x00\x01binary phone" {
		t.Errorf("server binary was modified: %q", got)
	}
	side, err := v.Read(results[0].ConflictPath)
	if err != nil || string(side) != "\x00\x01binary mac" {
		t.Errorf("conflict copy = %q, %v", side, err)
	}
	// A merged binary would contain conflict markers. It must not.
	if strings.Contains(string(got), "<<<<<<<") {
		t.Error("binary content was merged")
	}
}

func TestDeleteIsApplied(t *testing.T) {
	rc, v, r := newRec(t)
	head, _, _ := rc.Push("", "mac", []Change{put(t, r, "a.md", "x\n")})
	_, results, err := rc.Push(head, "mac", []Change{{Path: "a.md", Op: "del"}})
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if results[0].Status != StatusApplied {
		t.Errorf("status = %q, want applied", results[0].Status)
	}
	if exists(v, "a.md") {
		t.Error("file still present after a delete")
	}
}

// The most dangerous case in the whole design, and the exact bug LiveSync
// shipped in 1.0.12: a client that has no base has no idea what exists, so any
// deletion it implies is unfounded. It must never delete.
func TestClientWithNoBaseCannotDelete(t *testing.T) {
	rc, v, r := newRec(t)
	rc.Push("", "mac", []Change{put(t, r, "keep.md", "important\n")})
	_, results, err := rc.Push("", "fresh-device", []Change{{Path: "keep.md", Op: "del"}})
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if !exists(v, "keep.md") {
		t.Fatal("a client with no base deleted a file")
	}
	if results[0].Status != StatusRefused {
		t.Errorf("status = %q, want refused", results[0].Status)
	}
}

func TestUnknownBaseIsReported(t *testing.T) {
	rc, _, r := newRec(t)
	rc.Push("", "mac", []Change{put(t, r, "a.md", "x\n")})
	_, _, err := rc.Push("0123456789012345678901234567890123456789", "mac",
		[]Change{put(t, r, "a.md", "y\n")})
	if err == nil {
		t.Fatal("want an error for an unknown base")
	}
	if !repo.IsUnknownBase(err) {
		t.Errorf("err = %v, want IsUnknownBase", err)
	}
}

func TestMissingContentIsAnError(t *testing.T) {
	rc, _, _ := newRec(t)
	_, _, err := rc.Push("", "mac", []Change{
		{Path: "a.md", Op: "put", Hash: "0123456789012345678901234567890123456789"},
	})
	if err == nil {
		t.Fatal("want an error when the referenced content was never uploaded")
	}
}

func TestPathEscapeIsRejected(t *testing.T) {
	rc, _, r := newRec(t)
	c := put(t, r, "../escape.md", "pwned\n")
	if _, _, err := rc.Push("", "mac", []Change{c}); err == nil {
		t.Fatal("push accepted a path outside the vault")
	}
}

// Scan is what makes edits during downtime visible; fsnotify replays nothing.
func TestScanPicksUpOfflineEdits(t *testing.T) {
	rc, v, r := newRec(t)
	rc.Push("", "mac", []Change{put(t, r, "a.md", "one\n"), put(t, r, "b.md", "two\n")})

	// Simulate edits made while the server was stopped.
	v.Write("a.md", []byte("edited offline\n"))
	v.Remove("b.md")
	v.Write("c.md", []byte("created offline\n"))

	head, err := rc.Scan("offline changes")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	snap, err := r.Snapshot(head)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := snap["b.md"]; ok {
		t.Error("b.md still in the tree after an offline delete")
	}
	if _, ok := snap["c.md"]; !ok {
		t.Error("c.md missing after an offline create")
	}
	content, _ := r.ReadAt(head, "a.md")
	if string(content) != "edited offline\n" {
		t.Errorf("a.md = %q, want the offline edit", content)
	}
}

func TestScanWithNoChangesDoesNotCommit(t *testing.T) {
	rc, _, r := newRec(t)
	head, _, _ := rc.Push("", "mac", []Change{put(t, r, "a.md", "one\n")})
	again, err := rc.Scan("nothing happened")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if again != head {
		t.Errorf("Scan created an empty commit: %q -> %q", head, again)
	}
}

// exists is a test helper; production code has no need for it, so Vault does
// not carry one.
func exists(v *vault.Vault, path string) bool {
	_, err := v.Stat(path)
	return err == nil
}
