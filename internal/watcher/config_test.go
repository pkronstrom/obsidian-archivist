package watcher

import (
	"context"
	"fmt"
	"github.com/fsnotify/fsnotify"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pkronstrom/obsidian-archivist/internal/reconcile"
	"github.com/pkronstrom/obsidian-archivist/internal/repo"
	"github.com/pkronstrom/obsidian-archivist/internal/vault"
)

func inventoryHarness(t *testing.T) (*vault.Vault, *repo.Repo, *reconcile.Reconciler, string) {
	t.Helper()
	dir := t.TempDir()
	vaultDir := filepath.Join(dir, "vault")
	v, err := vault.New(vaultDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { v.Close() })
	r, err := repo.Open(vaultDir, filepath.Join(dir, "git"))
	if err != nil {
		t.Fatal(err)
	}
	r.SetSyncable(func(p string) bool { return !vault.Skip(p) })
	return v, r, reconcile.New(v, r), vaultDir
}

func awaitTracked(t *testing.T, r *repo.Repo, path string, why string) {
	t.Helper()
	deadline := time.Now().Add(6 * time.Second)
	for {
		if head, err := r.Head(); err == nil && head != "" {
			snap, err := r.Snapshot(head)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := snap[path]; ok {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal(why)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Inventory edits on the server enter history through existing directory watches.
func TestWatcherCommitsLocalInventoryEdits(t *testing.T) {
	v, r, rc, vaultDir := inventoryHarness(t)

	// The directory exists BEFORE the watcher starts, so this proves addTree
	// descends rather than proving the create-event path works.
	if err := os.MkdirAll(filepath.Join(vaultDir, ".archivist", "plugin-inventory"), 0o755); err != nil {
		t.Fatal(err)
	}

	w := New(v, rc, 50*time.Millisecond, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Run(ctx) }()
	time.Sleep(400 * time.Millisecond)

	if err := os.WriteFile(filepath.Join(vaultDir, ".archivist", "plugin-inventory", "01234567-89ab-cdef-0123-456789abcdef.json"),
		[]byte("body { color: red }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	awaitTracked(t, r, ".archivist/plugin-inventory/01234567-89ab-cdef-0123-456789abcdef.json",
		"a local inventory edit never reached history")
}

// Inventory ancestors created after startup also receive recursive watches.
func TestWatcherPicksUpInventoryDirCreatedAfterStartup(t *testing.T) {
	v, r, rc, vaultDir := inventoryHarness(t)

	w := New(v, rc, 50*time.Millisecond, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Run(ctx) }()
	time.Sleep(400 * time.Millisecond)

	if err := os.MkdirAll(filepath.Join(vaultDir, ".archivist", "plugin-inventory"), 0o755); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(vaultDir, ".archivist", "plugin-inventory", "01234567-89ab-cdef-0123-456789abcdef.json"),
		[]byte("body { color: green }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	awaitTracked(t, r, ".archivist/plugin-inventory/01234567-89ab-cdef-0123-456789abcdef.json",
		"an inventory directory created after startup is never watched")
}

// Previously allowlisted config must no longer enter history.
func TestWatcherIgnoresRefusedConfigPaths(t *testing.T) {
	v, r, rc, vaultDir := inventoryHarness(t)

	if err := os.MkdirAll(filepath.Join(vaultDir, ".obsidian"), 0o755); err != nil {
		t.Fatal(err)
	}

	w := New(v, rc, 50*time.Millisecond, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Run(ctx) }()
	time.Sleep(400 * time.Millisecond)

	if err := os.WriteFile(filepath.Join(vaultDir, ".obsidian", "appearance.json"),
		[]byte(`{"left":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)

	head, err := r.Head()
	if err != nil {
		t.Fatal(err)
	}
	if head == "" {
		return // nothing committed at all, which is correct
	}
	snap, err := r.Snapshot(head)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := snap[".obsidian/appearance.json"]; ok {
		t.Error("appearance.json reached history via the watcher")
	}
}

// Inspect registered watches as well as commits: a refused directory created
// after startup must not restart the retired config event stream.
func TestWatcherDirectoryPolicyAtStartupAndCreation(t *testing.T) {
	for _, startup := range []bool{true, false} {
		t.Run(fmt.Sprint("startup=", startup), func(t *testing.T) {
			v, _, rc, root := inventoryHarness(t)
			w := New(v, rc, time.Hour, slog.New(slog.DiscardHandler))
			fsw, err := fsnotify.NewWatcher()
			if err != nil {
				t.Fatal(err)
			}
			defer fsw.Close()
			w.fsw = fsw
			paths := []string{".archivist", ".archivist/plugin-inventory", ".archivist/other", ".archivist/plugin-inventory/nested", ".obsidian", ".obsidian/plugins", ".hidden", "notes.local", "notes.local/child", "notes"}
			for _, p := range paths {
				abs := filepath.Join(root, p)
				if err := os.MkdirAll(abs, 0o755); err != nil {
					t.Fatal(err)
				}
				if !startup {
					w.handle(fsnotify.Event{Name: abs, Op: fsnotify.Create})
				}
			}
			if startup {
				if err := w.addTree(root); err != nil {
					t.Fatal(err)
				}
			}
			defer func() {
				if w.timer != nil {
					w.timer.Stop()
				}
			}()
			watched := map[string]bool{}
			for _, abs := range fsw.WatchList() {
				rel, _ := filepath.Rel(root, abs)
				watched[filepath.ToSlash(rel)] = true
			}
			for _, p := range paths {
				want := p == ".archivist" || p == ".archivist/plugin-inventory" || p == "notes"
				if watched[p] != want {
					t.Errorf("watch %s = %v, want %v", p, watched[p], want)
				}
			}
		})
	}
}

func TestWatcherCommitsInventoryAncestorRemoval(t *testing.T) {
	for _, ancestor := range []string{".archivist", ".archivist/plugin-inventory"} {
		for _, event := range []fsnotify.Op{fsnotify.Rename, fsnotify.Remove} {
			t.Run(fmt.Sprintf("%s/%s", ancestor, event), func(t *testing.T) {
				v, r, rc, root := inventoryHarness(t)
				path := ".archivist/plugin-inventory/01234567-89ab-cdef-0123-456789abcdef.json"
				if err := v.Write(path, []byte("{}")); err != nil {
					t.Fatal(err)
				}
				base, err := rc.Scan("inventory")
				if err != nil {
					t.Fatal(err)
				}
				abs := filepath.Join(root, ancestor)
				if event == fsnotify.Rename {
					err = os.Rename(abs, filepath.Join(t.TempDir(), "removed"))
				} else {
					err = os.RemoveAll(abs)
				}
				if err != nil {
					t.Fatal(err)
				}
				w := New(v, rc, time.Hour, slog.New(slog.DiscardHandler))
				w.handle(fsnotify.Event{Name: abs, Op: event})
				defer w.cancelTimer()
				w.flush()
				head, err := r.Head()
				if err != nil || head == base {
					t.Fatalf("ancestor removal did not commit: %s %v", head, err)
				}
				snap, err := r.Snapshot(head)
				if err != nil {
					t.Fatal(err)
				}
				if _, ok := snap[path]; ok {
					t.Fatal("removed inventory remains advertised")
				}
			})
		}
	}
}
