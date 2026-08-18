package vaults

import (
	"os"
	"path/filepath"
	"testing"
)

func seedRoot(t *testing.T, names ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, n := range names {
		if err := os.MkdirAll(filepath.Join(root, "vaults", n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestLayoutPaths(t *testing.T) {
	l := Layout{Root: "/data"}
	if got, want := l.VaultDir("personal"), "/data/vaults/personal"; got != want {
		t.Errorf("VaultDir = %q, want %q", got, want)
	}
	if got, want := l.GitDir("personal"), "/data/.archivist/personal"; got != want {
		t.Errorf("GitDir = %q, want %q", got, want)
	}
}

func TestDiscoverFindsDirectoriesOnly(t *testing.T) {
	root := seedRoot(t, "personal", "work")
	if err := os.WriteFile(filepath.Join(root, "vaults", "notes.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "vaults", ".hidden"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := Layout{Root: root}.Discover()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"personal", "work"}
	if len(got) != len(want) {
		t.Fatalf("Discover = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Discover = %v, want %v (sorted)", got, want)
		}
	}
}

func TestDiscoverOnAnAbsentRootIsEmptyNotAnError(t *testing.T) {
	got, err := Layout{Root: filepath.Join(t.TempDir(), "nothing")}.Discover()
	if err != nil {
		t.Fatalf("Discover on an absent root: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Discover = %v, want empty", got)
	}
}

// The guard the spec assumed already existed. It did not.
func TestCheckRootRefusesARootThatIsItselfAVault(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := Layout{Root: root}.CheckRoot()
	if err == nil {
		t.Fatal("a root containing .git must be refused: ROOT was pointed at a vault")
	}
	if !contains(err.Error(), ".git") {
		t.Errorf("the error must name what it found: %v", err)
	}
}

func TestCheckRootRefusesARootThatIsAVaultsChild(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "vaults", "personal", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A .git inside a vault is a different mistake and must ALSO be refused:
	// the repository is meant to live outside the working tree.
	if err := (Layout{Root: root}).CheckRoot(); err == nil {
		t.Fatal("a vault containing its own .git must be refused")
	}
}

// $ROOT is not a vault, so no .git of any form belongs there -- pointer
// included.
func TestCheckRootRefusesAGitFileAtTheRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".git"),
		[]byte("gitdir: /somewhere/else/.git/worktrees/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := (Layout{Root: root}).CheckRoot(); err == nil {
		t.Fatal("a .git FILE at the root must be refused just like a directory")
	}
}

// THE case that took the server down on a correctly laid-out vault.
//
// This deployment writes `<vault>/.git` as a pointer to $ROOT/.archivist/<name>
// on purpose, so `git -C <vault> log` works from the host. That is not an
// embedded repository and must not be refused.
func TestCheckRootAcceptsTheDeliberateGitdirPointer(t *testing.T) {
	root := seedRoot(t, "personal")
	if err := os.MkdirAll(filepath.Join(root, ".archivist", "personal"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "vaults", "personal", ".git"),
		[]byte("gitdir: ../../.archivist/personal\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := (Layout{Root: root}).CheckRoot(); err != nil {
		t.Errorf("the layout's own gitdir pointer was refused: %v", err)
	}
}

// A pointer aimed anywhere else is still an embedded repository by another
// name, and is refused.
func TestCheckRootRefusesAPointerToSomewhereElse(t *testing.T) {
	root := seedRoot(t, "personal")
	if err := os.WriteFile(filepath.Join(root, "vaults", "personal", ".git"),
		[]byte("gitdir: /somewhere/else\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := (Layout{Root: root}).CheckRoot(); err == nil {
		t.Fatal("a pointer to a foreign git dir must be refused")
	}
}

// An absolute pointer to the right place is still the right place.
func TestCheckRootAcceptsAnAbsolutePointerToItsOwnGitDir(t *testing.T) {
	root := seedRoot(t, "personal")
	git := filepath.Join(root, ".archivist", "personal")
	if err := os.MkdirAll(git, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "vaults", "personal", ".git"),
		[]byte("gitdir: "+git+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := (Layout{Root: root}).CheckRoot(); err != nil {
		t.Errorf("an absolute pointer to its own git dir was refused: %v", err)
	}
}

func TestCheckRootAcceptsAProperLayout(t *testing.T) {
	root := seedRoot(t, "personal")
	if err := os.MkdirAll(filepath.Join(root, ".archivist", "personal"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := (Layout{Root: root}).CheckRoot(); err != nil {
		t.Errorf("a proper layout was refused: %v", err)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
