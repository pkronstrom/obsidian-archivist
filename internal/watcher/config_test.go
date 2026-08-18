package watcher

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pkronstrom/obsidian-archivist/internal/reconcile"
	"github.com/pkronstrom/obsidian-archivist/internal/repo"
	"github.com/pkronstrom/obsidian-archivist/internal/vault"
)

func configHarness(t *testing.T) (*vault.Vault, *repo.Repo, *reconcile.Reconciler, string) {
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

// A local edit to an allowlisted config file must produce a commit, the same
// way a local edit to a note does. Anything else means the local write path
// does not carry config at all.
func TestWatcherCommitsLocalConfigEdits(t *testing.T) {
	v, r, rc, vaultDir := configHarness(t)

	// The directory exists BEFORE the watcher starts, so this proves addTree
	// descends rather than proving the create-event path works.
	if err := os.MkdirAll(filepath.Join(vaultDir, ".obsidian", "snippets"), 0o755); err != nil {
		t.Fatal(err)
	}

	w := New(v, rc, 50*time.Millisecond, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Run(ctx) }()
	time.Sleep(400 * time.Millisecond)

	if err := os.WriteFile(filepath.Join(vaultDir, ".obsidian", "snippets", "dark.css"),
		[]byte("body { color: red }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	awaitTracked(t, r, ".obsidian/snippets/dark.css",
		"a local config edit never reached history: the watcher is not watching .obsidian")
}

// A config directory created AFTER the watcher started must be watched too.
// addTree alone only covers directories present at startup, and the first
// remote config push is what creates .obsidian/ on the server.
func TestWatcherPicksUpAConfigDirCreatedAfterStartup(t *testing.T) {
	v, r, rc, vaultDir := configHarness(t)

	w := New(v, rc, 50*time.Millisecond, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Run(ctx) }()
	time.Sleep(400 * time.Millisecond)

	if err := os.MkdirAll(filepath.Join(vaultDir, ".obsidian", "snippets"), 0o755); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(vaultDir, ".obsidian", "snippets", "late.css"),
		[]byte("body { color: green }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	awaitTracked(t, r, ".obsidian/snippets/late.css",
		"a config directory created after startup is never watched")
}

// The refused half of the same directory must still produce nothing.
func TestWatcherIgnoresRefusedConfigPaths(t *testing.T) {
	v, r, rc, vaultDir := configHarness(t)

	if err := os.MkdirAll(filepath.Join(vaultDir, ".obsidian"), 0o755); err != nil {
		t.Fatal(err)
	}

	w := New(v, rc, 50*time.Millisecond, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Run(ctx) }()
	time.Sleep(400 * time.Millisecond)

	if err := os.WriteFile(filepath.Join(vaultDir, ".obsidian", "workspace.json"),
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
	if _, ok := snap[".obsidian/workspace.json"]; ok {
		t.Error("workspace.json reached history via the watcher")
	}
}
