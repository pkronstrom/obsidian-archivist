package reconcile

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/pkronstrom/obsidian-archivist/internal/repo"
	"github.com/pkronstrom/obsidian-archivist/internal/vault"
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
	// The conflict file now carries the three-way merge with markers, not the
	// bare losing version: with only one side you have to diff it against the
	// note by hand to discover what actually differs.
	if !strings.Contains(string(side), "from mac") {
		t.Errorf("conflict file lost the client version: %q", side)
	}
	if !strings.Contains(string(side), "from phone") {
		t.Errorf("conflict file does not show the server side: %q", side)
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

// A batch with a bad entry must leave the working tree untouched. Before the
// validation pass, applyOne mutated as it iterated: the caller was told the
// push failed while earlier entries sat written-but-uncommitted, and the next
// watcher scan committed exactly what the client believed was rejected.
func TestBadEntryLeavesTheWholeBatchUnapplied(t *testing.T) {
	rc, v, r := newRec(t)
	good := put(t, r, "good.md", "should not be written\n")
	bad := Change{Path: "missing.md", Op: "put", Hash: "0123456789012345678901234567890123456789"}

	if _, _, err := rc.Push("", "mac", []Change{good, bad}); err == nil {
		t.Fatal("want an error for the missing blob")
	}
	if exists(v, "good.md") {
		t.Error("an earlier entry was written despite the batch failing")
	}
	head, _ := r.Head()
	if head != "" {
		t.Errorf("a commit was created for a failed push: %s", head)
	}
}

func TestDuplicatePathInOnePushIsRejected(t *testing.T) {
	rc, _, r := newRec(t)
	a := put(t, r, "same.md", "first\n")
	b := put(t, r, "same.md", "second\n")
	if _, _, err := rc.Push("", "mac", []Change{a, b}); err == nil {
		t.Fatal("want an error when one push names the same path twice")
	}
}

// Two conflicts on one path from one device inside a second must not collide.
func TestConflictPathsAreUniquePerContent(t *testing.T) {
	a := conflictPath("notes/idea.md", "relay", []byte("version one\n"))
	b := conflictPath("notes/idea.md", "relay", []byte("version two\n"))
	if a == b {
		t.Errorf("both conflicts landed on %s; the first would be overwritten", a)
	}
	if !strings.HasSuffix(a, ".md") {
		t.Errorf("conflict path lost its extension: %s", a)
	}
	// Identical content collapsing to one path is correct, not a collision.
	if conflictPath("notes/idea.md", "relay", []byte("same\n")) !=
		conflictPath("notes/idea.md", "relay", []byte("same\n")) {
		t.Error("identical content produced two different conflict paths")
	}
}

// Every result must say what the server now holds, so a client repairing one
// path does not have to download the whole snapshot to find out.
func TestResultsReportTheResultingHash(t *testing.T) {
	rc, _, r := newRec(t)
	base, results, _ := rc.Push("", "mac", []Change{put(t, r, "a.md", "one\ntwo\nthree\n")})
	if results[0].Hash == "" || results[0].Size == 0 {
		t.Errorf("applied result carries no hash/size: %+v", results[0])
	}

	// Merge: the server holds bytes the client never sent.
	rc.Push(base, "phone", []Change{put(t, r, "a.md", "one\ntwo\nTHREE\n")})
	_, results, err := rc.Push(base, "mac", []Change{put(t, r, "a.md", "ONE\ntwo\nthree\n")})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Status != StatusMerged {
		t.Fatalf("status = %s", results[0].Status)
	}
	merged, _ := repo.HashContent([]byte("ONE\ntwo\nTHREE\n"))
	if results[0].Hash != merged {
		t.Errorf("merged result hash = %q, want the merged content %q", results[0].Hash, merged)
	}

	// Conflict: both sides addressable.
	base2, _ := r.Head()
	rc.Push(base2, "phone", []Change{put(t, r, "c.md", "phone\n")})
	_, results, _ = rc.Push(base2, "mac", []Change{put(t, r, "c.md", "mac\n")})
	if results[0].Status != StatusConflict {
		t.Fatalf("status = %s", results[0].Status)
	}
	if results[0].Hash == "" || results[0].ConflictHash == "" {
		t.Errorf("conflict result missing a hash: %+v", results[0])
	}
}

// The conflict file shows both sides against their common ancestor, with the
// sides named. "ours" and "theirs" are useless to someone opening this on a
// phone the next day.
func TestConflictFileCarriesGitStyleMarkers(t *testing.T) {
	rc, v, r := newRec(t)
	base, _, _ := rc.Push("", "seed", []Change{put(t, r, "n.md", "line1\nline2\nline3\n")})
	rc.Push(base, "phone", []Change{put(t, r, "n.md", "line1\nSERVER EDIT\nline3\n")})
	_, results, err := rc.Push(base, "work-mac", []Change{put(t, r, "n.md", "line1\nCLIENT EDIT\nline3\n")})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Status != StatusConflict {
		t.Fatalf("status = %q, want conflict", results[0].Status)
	}

	body, err := v.Read(results[0].ConflictPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	for _, want := range []string{"<<<<<<<", "=======", ">>>>>>>", "SERVER EDIT", "CLIENT EDIT", "server", "work-mac"} {
		if !strings.Contains(got, want) {
			t.Errorf("conflict file lacks %q:\n%s", want, got)
		}
	}
	// Unconflicted context survives, so it still reads as a note.
	if !strings.Contains(got, "line1") || !strings.Contains(got, "line3") {
		t.Errorf("surrounding lines were dropped:\n%s", got)
	}
}

// The note itself must stay clean. Markers there would render as literal text
// in every preview on every device until someone resolved them.
func TestTheNoteItselfNeverGetsMarkers(t *testing.T) {
	rc, v, r := newRec(t)
	base, _, _ := rc.Push("", "seed", []Change{put(t, r, "n.md", "line1\nline2\nline3\n")})
	rc.Push(base, "phone", []Change{put(t, r, "n.md", "line1\nSERVER EDIT\nline3\n")})
	rc.Push(base, "work-mac", []Change{put(t, r, "n.md", "line1\nCLIENT EDIT\nline3\n")})

	body, _ := v.Read("n.md")
	if strings.Contains(string(body), "<<<<<<<") {
		t.Errorf("the note carries conflict markers:\n%s", body)
	}
	if !strings.Contains(string(body), "SERVER EDIT") {
		t.Errorf("the note is not the server version:\n%s", body)
	}
}

// Conflict markers inside a binary are worse than either version alone, so a
// binary conflict keeps the client's exact bytes and skips the merge entirely.
//
// The NUL byte is load-bearing: IsBinary uses git's heuristic, so content
// without one is treated as text no matter what the extension says. A synthetic
// 8-byte PNG header has no NUL and merges happily, which this test asserted the
// opposite of on the first attempt.
func TestBinaryConflictFileHasNoMarkersAndExactBytes(t *testing.T) {
	rc, v, r := newRec(t)
	base, _, _ := rc.Push("", "seed", []Change{put(t, r, "i.png", "\x00\x01PNG base")})
	rc.Push(base, "phone", []Change{put(t, r, "i.png", "\x00\x01PNG server")})
	_, results, err := rc.Push(base, "work-mac", []Change{put(t, r, "i.png", "\x00\x01PNG client")})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Status != StatusConflict {
		t.Fatalf("status = %q, want conflict", results[0].Status)
	}
	body, err := v.Read(results[0].ConflictPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "<<<<<<<") {
		t.Errorf("binary conflict file got markers: %q", body)
	}
	if string(body) != "\x00\x01PNG client" {
		t.Errorf("binary conflict file is not the client's exact bytes: %q", body)
	}
}

// Git markers are markdown syntax, so an unfenced conflict renders as nested
// blockquotes and phantom headings in every preview. Inside a fence it is
// literal and reads the same on a phone as in source mode.
func TestConflictFileIsFencedSoItRendersLiterally(t *testing.T) {
	rc, v, r := newRec(t)
	base, _, _ := rc.Push("", "seed", []Change{put(t, r, "n.md", "one\ntwo\nthree\n")})
	rc.Push(base, "phone", []Change{put(t, r, "n.md", "one\nSERVER\nthree\n")})
	_, results, _ := rc.Push(base, "work-mac", []Change{put(t, r, "n.md", "one\nCLIENT\nthree\n")})

	body, err := v.Read(results[0].ConflictPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if !strings.Contains(got, "```text\n") {
		t.Errorf("conflict body is not fenced:\n%s", got)
	}
	if !strings.Contains(got, "<<<<<<< server") || !strings.Contains(got, ">>>>>>> work-mac") {
		t.Errorf("markers missing or not git width:\n%s", got)
	}
	// The header has to say what to do; a bare fence explains nothing.
	if !strings.Contains(got, "Conflict:") {
		t.Errorf("no explanation for the reader:\n%s", got)
	}
}

// A note containing its own code block would terminate a three-backtick fence
// early, and everything after it would render as markdown again.
func TestFenceWidensPastCodeBlocksInTheNote(t *testing.T) {
	rc, v, r := newRec(t)
	withCode := "one\n```js\nlet x = 1\n```\ntwo\n"
	base, _, _ := rc.Push("", "seed", []Change{put(t, r, "n.md", withCode)})
	rc.Push(base, "phone", []Change{put(t, r, "n.md", strings.Replace(withCode, "two", "SERVER", 1))})
	_, results, _ := rc.Push(base, "work-mac", []Change{put(t, r, "n.md", strings.Replace(withCode, "two", "CLIENT", 1))})

	body, _ := v.Read(results[0].ConflictPath)
	got := string(body)
	if !strings.Contains(got, "````text\n") {
		t.Errorf("fence did not widen past the note's own code block:\n%s", got)
	}
	if !strings.Contains(got, "````\n") {
		t.Errorf("closing fence did not widen to match:\n%s", got)
	}
}
