package auth

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The watcher exists so that minting is not an outage. A token added by the
// subcommand must resolve in the running server without a restart.
func TestWatchPicksUpATokenMintedAfterStartup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.json")

	seed := New()
	if _, err := seed.Mint(Principal{Label: "first", Vaults: []string{"personal"},
		Scopes: []string{ScopeRead}}); err != nil {
		t.Fatal(err)
	}
	if err := seed.Save(path); err != nil {
		t.Fatal(err)
	}

	live, err := Load(path, "")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := live.Watch(ctx, path, log); err != nil {
		t.Fatal(err)
	}

	// Mint into a fresh set loaded from the same file, exactly as the CLI does.
	minter, err := Load(path, "")
	if err != nil {
		t.Fatal(err)
	}
	token, err := minter.Mint(Principal{Label: "second", Vaults: []string{"personal"},
		Scopes: []string{ScopeRead, ScopeWrite}})
	if err != nil {
		t.Fatal(err)
	}
	if err := minter.Save(path); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(5 * time.Second)
	for {
		if _, ok := live.Lookup(token); ok {
			return // reloaded
		}
		select {
		case <-deadline:
			t.Fatal("the new token never resolved; the watcher did not reload")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// A file that briefly parses badly must not empty the live table. The server
// keeps serving what it had.
func TestWatchKeepsTheOldTableWhenTheNewFileIsBroken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.json")

	seed := New()
	token, _ := seed.Mint(Principal{Label: "first", Vaults: []string{"personal"},
		Scopes: []string{ScopeRead}})
	if err := seed.Save(path); err != nil {
		t.Fatal(err)
	}
	live, err := Load(path, "")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := live.Watch(ctx, path, log); err != nil {
		t.Fatal(err)
	}

	if err := writeAtomicForTest(path, "{ this is not json"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)

	if _, ok := live.Lookup(token); !ok {
		t.Error("a broken file emptied the live table; the server would have locked itself out")
	}
}

func writeAtomicForTest(path, body string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
