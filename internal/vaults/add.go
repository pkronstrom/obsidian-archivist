package vaults

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/pkronstrom/obsidian-archivist/internal/repo"
)

// Add creates a vault on disk without a running server's help, for the
// `vault add` subcommand. It is the documented way to make an encrypted vault:
// `mkdir` cannot record a kind, and a directory added by hand is always an
// ordinary one.
//
// Safe against a running server, because of the order. The server discovers
// vaults by scanning vaults/, and reads a vault's kind once, when it opens it.
// So the vault is built under a dot-name, which discovery skips, and renamed
// into place only when its history directory and kind are complete: there is
// no moment at which the server can find the vault and open it as an ordinary
// one.
func Add(l Layout, name string, encrypted bool) error {
	if err := ValidName(name); err != nil {
		return err
	}
	existing, err := l.Discover()
	if err != nil {
		return err
	}
	if clash := Collides(name, existing); clash != "" {
		return fmt.Errorf("vault %q collides with the existing %q", name, clash)
	}
	for _, dir := range []string{l.VaultDir(name), l.GitDir(name)} {
		if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("refusing to create %q: %s already exists", name, dir)
		}
	}

	staging := filepath.Join(l.VaultsDir(), ".new-"+name)
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(l.GitDir(name), 0o755); err != nil {
		return err
	}
	rp, err := repo.Open(staging, l.GitDir(name))
	if err == nil && encrypted {
		err = rp.SetEncrypted()
	}
	if err == nil {
		// Initialising recorded the staging directory as the work tree. Point
		// it at the real one, so the result is what the server would have made.
		err = rp.SetWorkTree(filepath.Join("..", "..", "vaults", name))
	}
	if err == nil {
		err = os.Rename(staging, l.VaultDir(name))
	}
	if err != nil {
		os.RemoveAll(staging)
		os.RemoveAll(l.GitDir(name))
		return err
	}
	return nil
}
