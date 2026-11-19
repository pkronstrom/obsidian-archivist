package repo

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestEncryptedIsOffByDefault(t *testing.T) {
	r, _, _ := newRepo(t)
	if on, err := r.Encrypted(); err != nil || on {
		t.Fatalf("a new vault must be ordinary: on=%v err=%v", on, err)
	}
}

// The kind has to survive a restart, and it has to be in `config`: that is the
// file Archive ships, so it is what makes a restored vault encrypted again.
func TestSetEncryptedPersistsInTheGitConfig(t *testing.T) {
	work, gitDir := t.TempDir(), filepath.Join(t.TempDir(), "git")
	r, err := Open(work, gitDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SetEncrypted(); err != nil {
		t.Fatal(err)
	}

	again, err := Open(work, gitDir)
	if err != nil {
		t.Fatal(err)
	}
	if on, err := again.Encrypted(); err != nil || !on {
		t.Fatalf("the kind did not survive reopening: on=%v err=%v", on, err)
	}
	if _, err := os.Stat(filepath.Join(gitDir, "config")); err != nil {
		t.Fatalf("the kind must live in the git config: %v", err)
	}
}

func TestSetEncryptedRefusesAVaultWithHistory(t *testing.T) {
	r, _, gitDir := newRepo(t)
	work := filepath.Join(filepath.Dir(gitDir), "vault")
	if err := os.WriteFile(filepath.Join(work, "note.md"), []byte("plain"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit("first"); err != nil {
		t.Fatal(err)
	}
	if err := r.SetEncrypted(); !errors.Is(err, ErrHasHistory) {
		t.Fatalf("want ErrHasHistory, got %v", err)
	}
	if on, _ := r.Encrypted(); on {
		t.Fatal("a refused mark must not be written")
	}
}
