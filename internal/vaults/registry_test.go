package vaults

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pkronstrom/obsidian-archivist/internal/guard"
	"github.com/pkronstrom/obsidian-archivist/protocol"
)

func testOptions() Options {
	return Options{
		MaxVaults: 5,
		Limits:    guard.Limits{},
		Log:       slog.New(slog.DiscardHandler),
		Now:       time.Now,
	}
}

func TestOpenDiscoveredVaults(t *testing.T) {
	root := seedRoot(t, "personal", "work")
	reg, err := NewRegistry(Layout{Root: root}, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()

	names, err := reg.Names()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 {
		t.Fatalf("Names = %v, want personal and work", names)
	}
	for _, n := range names {
		v, err := reg.Get(n)
		if err != nil {
			t.Fatalf("Get(%q): %v", n, err)
		}
		if v.Repo == nil || v.Reconciler == nil {
			t.Fatalf("vault %q was opened without a repository or a reconciler", n)
		}
	}
}

func TestGetAnUnknownVaultIsNotFound(t *testing.T) {
	reg, err := NewRegistry(Layout{Root: seedRoot(t, "personal")}, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()

	if _, err := reg.Get("nope"); !IsNotFound(err) {
		t.Errorf("Get on an unknown vault = %v, want a not-found error", err)
	}
}

// A vault rsynced in while the server runs must appear without a restart.
func TestRescanFindsAVaultAddedAtRuntime(t *testing.T) {
	root := seedRoot(t, "personal")
	opts := testOptions()
	reg, err := NewRegistry(Layout{Root: root}, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()

	if err := os.MkdirAll(filepath.Join(root, "vaults", "work"), 0o755); err != nil {
		t.Fatal(err)
	}
	reg.InvalidateCache() // the cache is a rate limit, not a correctness boundary

	names, err := reg.Names()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 {
		t.Fatalf("Names = %v after a vault was added at runtime", names)
	}
	if _, err := reg.Get("work"); err != nil {
		t.Fatalf("the new vault could not be opened: %v", err)
	}
}

// Discovery over the limit warns; it must never refuse to serve what is there.
func TestDiscoveryPastTheLimitStillServes(t *testing.T) {
	root := seedRoot(t, "a", "b", "c")
	opts := testOptions()
	opts.MaxVaults = 2
	reg, err := NewRegistry(Layout{Root: root}, opts)
	if err != nil {
		t.Fatalf("discovery past the limit must not fail: %v", err)
	}
	defer reg.Close()

	names, err := reg.Names()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 3 {
		t.Errorf("Names = %v, want all three served; raising the limit must always be a way out", names)
	}
}

func TestCreateRefusesPastTheLimit(t *testing.T) {
	opts := testOptions()
	opts.MaxVaults = 2
	reg, err := NewRegistry(Layout{Root: seedRoot(t, "a", "b")}, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()

	if err := reg.Create("c"); err == nil {
		t.Fatal("creation past ARCHIVIST_MAX_VAULTS must be refused")
	}
}

func TestCreateRefusesAnNFCCollision(t *testing.T) {
	reg, err := NewRegistry(Layout{Root: seedRoot(t, "tyo\u0308")}, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()

	if err := reg.Create("ty\u00f6"); err == nil {
		t.Fatal("a name colliding after NFC normalisation must be refused")
	}
}

func TestCreateMakesBothDirectoriesAndOpensIt(t *testing.T) {
	root := seedRoot(t)
	reg, err := NewRegistry(Layout{Root: root}, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()

	if err := reg.Create("work"); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(root, "vaults", "work")); err != nil || !fi.IsDir() {
		t.Error("the vault directory was not created")
	}
	if _, err := reg.Get("work"); err != nil {
		t.Errorf("a created vault must be immediately usable: %v", err)
	}
}

// BOTH write paths, per vault. Remote first.
func TestRemoteWritePathIsPerVault(t *testing.T) {
	reg, err := NewRegistry(Layout{Root: seedRoot(t, "personal", "work")}, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()

	personal, _ := reg.Get("personal")
	work, _ := reg.Get("work")

	h, err := personal.Repo.WriteBlob([]byte("only in personal\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := personal.Reconciler.Push("", "mac", []protocol.Change{
		{Path: "note.md", Op: protocol.OpPut, Hash: h},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := work.Vault.Read("note.md"); err == nil {
		t.Error("a push to personal reached work: the vaults are not isolated")
	}
}

// And the local one: a second vault must have a watcher, or it gets working
// remote sync and silently no local sync.
func TestLocalWritePathIsPerVault(t *testing.T) {
	root := seedRoot(t, "personal", "work")
	opts := testOptions()
	opts.Debounce = 50 * time.Millisecond
	reg, err := NewRegistry(Layout{Root: root}, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := reg.StartWatchers(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)

	if err := os.WriteFile(filepath.Join(root, "vaults", "work", "local.md"),
		[]byte("written on the host\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	work, _ := reg.Get("work")
	deadline := time.Now().Add(5 * time.Second)
	for {
		head, err := work.Repo.Head()
		if err != nil {
			t.Fatal(err)
		}
		if head != "" {
			snap, err := work.Repo.Snapshot(head)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := snap["local.md"]; ok {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("a local edit in the SECOND vault never reached history: it has no watcher")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
