package vaults

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAddMakesAnEncryptedVaultTheServerOpensAsEncrypted(t *testing.T) {
	root := t.TempDir()
	l := Layout{Root: root}
	if err := Add(l, "work", true); err != nil {
		t.Fatal(err)
	}
	if err := Add(l, "personal", false); err != nil {
		t.Fatal(err)
	}

	reg, err := NewRegistry(l, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	for name, want := range map[string]bool{"work": true, "personal": false} {
		inst, err := reg.Get(name)
		if err != nil {
			t.Fatal(err)
		}
		if inst.Encrypted != want {
			t.Errorf("%s: encrypted=%v, want %v", name, inst.Encrypted, want)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(root, "vaults", ".new-*")); len(left) != 0 {
		t.Errorf("staging directories left behind: %v", left)
	}
	// The same shape the server makes, so real git can still be pointed at it.
	cfg, err := os.ReadFile(filepath.Join(l.GitDir("work"), "config"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cfg), "worktree = ../../vaults/work\n") {
		t.Errorf("the work tree must be the vault directory:\n%s", cfg)
	}
}

// Adding over an existing vault must not be a way to mark it encrypted, or to
// reset its history.
func TestAddRefusesAnExistingVault(t *testing.T) {
	root := t.TempDir()
	l := Layout{Root: root}
	if err := os.MkdirAll(l.VaultDir("work"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Add(l, "work", true); err == nil {
		t.Fatal("want a refusal for a vault directory that already exists")
	}
	if _, err := os.Stat(l.GitDir("work")); err == nil {
		t.Error("a refused add must not create the history directory")
	}
}
