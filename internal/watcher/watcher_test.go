package watcher

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pkronstrom/obsidian-archivist/internal/reconcile"
	"github.com/pkronstrom/obsidian-archivist/internal/repo"
	"github.com/pkronstrom/obsidian-archivist/internal/vault"
)

const debounce = 60 * time.Millisecond

func start(t *testing.T) (*vault.Vault, *repo.Repo, *reconcile.Reconciler, context.CancelFunc) {
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
	rc := reconcile.New(v, r)
	w := New(v, rc, debounce, slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("watcher did not stop")
		}
		v.Close()
	})
	// Let Run finish its startup scan and register watches.
	time.Sleep(150 * time.Millisecond)
	return v, r, rc, cancel
}

func settle() { time.Sleep(debounce + 400*time.Millisecond) }

func headOf(t *testing.T, r *repo.Repo) string {
	t.Helper()
	h, err := r.Head()
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// Startup must reconcile whatever changed while the process was not running.
// fsnotify replays nothing, so without this those edits are lost forever.
func TestStartupScanPicksUpOfflineEdits(t *testing.T) {
	base := t.TempDir()
	work := filepath.Join(base, "vault")
	v, err := vault.New(work)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	r, err := repo.Open(work, filepath.Join(base, "git"))
	if err != nil {
		t.Fatal(err)
	}
	// Written with no watcher running at all.
	v.Write("offline.md", []byte("written while stopped\n"))

	rc := reconcile.New(v, r)
	w := New(v, rc, debounce, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	go w.Run(ctx)
	time.Sleep(200 * time.Millisecond)
	cancel()

	snap, err := r.Snapshot(headOf(t, r))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := snap["offline.md"]; !ok {
		t.Errorf("snapshot = %+v, want offline.md", snap)
	}
}

func TestNewFileIsCommittedOnce(t *testing.T) {
	v, r, _, _ := start(t)
	before := headOf(t, r)

	v.Write("a.md", []byte("hello\n"))
	settle()

	after := headOf(t, r)
	if after == before {
		t.Fatal("no commit for a new file")
	}
	snap, _ := r.Snapshot(after)
	if _, ok := snap["a.md"]; !ok {
		t.Errorf("a.md missing from the tree")
	}
}

// A burst of writes must debounce into a single commit, not one per write.
func TestBurstOfWritesBecomesOneCommit(t *testing.T) {
	v, r, _, _ := start(t)
	v.Write("seed.md", []byte("seed\n"))
	settle()
	before := headOf(t, r)

	for i := 0; i < 5; i++ {
		v.Write("burst.md", []byte{byte('a' + i), '\n'})
		time.Sleep(5 * time.Millisecond)
	}
	settle()

	after := headOf(t, r)
	if after == before {
		t.Fatal("no commit for the burst")
	}
	changes, err := r.Changes(before, after)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 {
		t.Errorf("changes = %+v, want exactly one commit's worth", changes)
	}
}

// The server writes to the tree when applying a push; the watcher must not
// treat that as a new local edit, or the two loop forever.
func TestServerOwnWriteIsNotCommittedAgain(t *testing.T) {
	_, r, rc, _ := start(t)
	content := []byte("from a client\n")
	hash, err := r.WriteBlob(content)
	if err != nil {
		t.Fatal(err)
	}
	head, _, err := rc.Push("", "mac", []reconcile.Change{
		{Path: "pushed.md", Op: "put", Hash: hash},
	})
	if err != nil {
		t.Fatal(err)
	}
	settle()

	if got := headOf(t, r); got != head {
		t.Errorf("watcher committed its own echo: %q -> %q", head, got)
	}
}

func TestDeleteIsCommitted(t *testing.T) {
	v, r, _, _ := start(t)
	v.Write("gone.md", []byte("here\n"))
	settle()

	v.Remove("gone.md")
	settle()

	snap, _ := r.Snapshot(headOf(t, r))
	if _, ok := snap["gone.md"]; ok {
		t.Error("deleted file is still in the tree")
	}
}

// fsnotify is not recursive: a directory created after startup needs its own
// watch, or edits inside it are invisible.
func TestDirectoryCreatedAfterStartupIsWatched(t *testing.T) {
	v, r, _, _ := start(t)
	v.Write("seed.md", []byte("seed\n"))
	settle()

	if err := os.MkdirAll(filepath.Join(v.Dir(), "later/deeper"), 0o755); err != nil {
		t.Fatal(err)
	}
	settle()
	v.Write("later/deeper/new.md", []byte("inside a new dir\n"))
	settle()

	snap, _ := r.Snapshot(headOf(t, r))
	if _, ok := snap["later/deeper/new.md"]; !ok {
		t.Errorf("file in a newly created directory was missed: %+v", snap)
	}
}

// Moving a populated tree in hits the race between mkdir and watch
// registration, which is why the implementation scans a directory right after
// adding its watch.
func TestPopulatedDirectoryMovedInIsPickedUp(t *testing.T) {
	v, r, _, _ := start(t)
	v.Write("seed.md", []byte("seed\n"))
	settle()

	staging := t.TempDir()
	inner := filepath.Join(staging, "imported")
	os.MkdirAll(filepath.Join(inner, "sub"), 0o755)
	os.WriteFile(filepath.Join(inner, "one.md"), []byte("one\n"), 0o644)
	os.WriteFile(filepath.Join(inner, "sub", "two.md"), []byte("two\n"), 0o644)

	if err := os.Rename(inner, filepath.Join(v.Dir(), "imported")); err != nil {
		t.Skipf("cross-device rename unavailable: %v", err)
	}
	settle()

	snap, _ := r.Snapshot(headOf(t, r))
	for _, want := range []string{"imported/one.md", "imported/sub/two.md"} {
		if _, ok := snap[want]; !ok {
			t.Errorf("%s missing after a wholesale directory move: %+v", want, snap)
		}
	}
}

func TestDotfilesAreIgnored(t *testing.T) {
	v, r, _, _ := start(t)
	v.Write("seed.md", []byte("seed\n"))
	settle()
	before := headOf(t, r)

	os.MkdirAll(filepath.Join(v.Dir(), ".obsidian"), 0o755)
	os.WriteFile(filepath.Join(v.Dir(), ".obsidian/app.json"), []byte("{}"), 0o644)
	settle()

	if got := headOf(t, r); got != before {
		t.Error("a dotfile change produced a commit")
	}
}

// A plain debounce re-arms on every event, so sustained writing postpones the
// commit forever. Found by a backup-consistency test: 60 writes at 80ms with a
// 200ms debounce produced zero commits until the writing stopped.
func TestSustainedWritesStillCommit(t *testing.T) {
	v, r, _, _ := start(t)
	v.Write("seed.md", []byte("seed\n"))
	settle()
	before := headOf(t, r)

	// Keep writing for the WHOLE test, so the plain debounce can never fire --
	// every write re-arms it. Only the max-delay cap can produce a commit here.
	//
	// Polling while the writer runs, rather than checking once after it stops,
	// is deliberate: a fixed deadline is too tight under -race, where a
	// commit can take longer than the margin, and stopping the writer first
	// would let the debounce commit and prove nothing.
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			v.Write("busy.md", []byte{byte('a' + i%26), '\n'})
			time.Sleep(20 * time.Millisecond)
		}
	}()
	defer func() { close(stop); <-done }()

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if headOf(t, r) != before {
			return // the cap fired while writes were still arriving
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Error("sustained writes never committed; the max-delay cap did not fire")
}
