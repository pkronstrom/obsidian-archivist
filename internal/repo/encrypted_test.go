package repo

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
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

func TestKeyCheckIsWriteOnce(t *testing.T) {
	r, _, _ := newRepo(t)
	if got, err := r.KeyCheck(); err != nil || got != "" {
		t.Fatalf("a new vault has no key check: %q %v", got, err)
	}
	if err := r.SetKeyCheck("aa"); err != nil {
		t.Fatal(err)
	}
	if err := r.SetKeyCheck("bb"); !errors.Is(err, ErrKeyCheckSet) {
		t.Fatalf("want ErrKeyCheckSet, got %v", err)
	}
	if got, _ := r.KeyCheck(); got != "aa" {
		t.Fatalf("the first check must stand, got %q", got)
	}
}

// A restored export must come back encrypted. If the kind were lost, the vault
// would be served as an ordinary one and accept plaintext.
func TestArchiveCarriesTheKindAndKeyCheck(t *testing.T) {
	r, v, _ := newRepo(t)
	if err := r.SetEncrypted(); err != nil {
		t.Fatal(err)
	}
	if err := r.SetKeyCheck("aa"); err != nil {
		t.Fatal(err)
	}
	v.Write("mfrgg", []byte("archivist-vault/v1\nopaque"))
	if _, err := r.Commit("first"); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := r.Archive(&buf, false); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	tr := tar.NewReader(&buf)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dest, hdr.Name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(f, tr); err != nil {
			t.Fatal(err)
		}
		f.Close()
	}

	restored, err := Open(t.TempDir(), dest)
	if err != nil {
		t.Fatal(err)
	}
	if on, err := restored.Encrypted(); err != nil || !on {
		t.Fatalf("the restored vault is not encrypted: on=%v err=%v", on, err)
	}
	if got, _ := restored.KeyCheck(); got != "aa" {
		t.Fatalf("the restored vault lost its key check: %q", got)
	}
}
