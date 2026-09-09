package vault

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newVault(t *testing.T) (*Vault, string) {
	t.Helper()
	dir := t.TempDir()
	v, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { v.Close() })
	return v, dir
}

// The previous design used filepath.Clean + a HasPrefix guard. That is broken:
// filepath.Clean("/" + "../escape.md") == "/escape.md", which joins back INSIDE
// the root and passes the guard. "a/../../b.md" is the same trap one level
// deeper. Both must be rejected.
func TestRejectsNonLocalPaths(t *testing.T) {
	v, _ := newVault(t)
	for _, p := range []string{
		"../escape.md",
		"a/../../b.md",
		"/etc/passwd",
		"",
		".",
		"..",
	} {
		if _, err := v.Read(p); err == nil {
			t.Errorf("Read(%q) succeeded, want rejection", p)
		}
		if err := v.Write(p, []byte("x")); err == nil {
			t.Errorf("Write(%q) succeeded, want rejection", p)
		}
	}
}

// A lexical check cannot stop this one: the path is perfectly local, but the
// symlink points out of the vault. os.Root refuses it in the kernel.
func TestRefusesToFollowSymlinkOutOfVault(t *testing.T) {
	v, dir := newVault(t)
	secretDir := t.TempDir()
	secret := filepath.Join(secretDir, "secret")
	if err := os.WriteFile(secret, []byte("classified"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(dir, "link.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if b, err := v.Read("link.md"); err == nil {
		t.Fatalf("followed a symlink out of the vault, read %q", b)
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	v, _ := newVault(t)
	want := []byte("# hello\n\nsome text\n")
	if err := v.Write("notes/deep/nested/a.md", want); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := v.Read("notes/deep/nested/a.md")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("Read = %q, want %q", got, want)
	}
}

func TestWriteLeavesNoTempFile(t *testing.T) {
	v, dir := newVault(t)
	if err := v.Write("notes/a.md", []byte("hello")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "notes"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tmpPrefix) {
			t.Errorf("temp file %q left behind", e.Name())
		}
	}
}

func TestWriteReplacesExistingAtomically(t *testing.T) {
	v, _ := newVault(t)
	if err := v.Write("a.md", []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := v.Write("a.md", []byte("second")); err != nil {
		t.Fatal(err)
	}
	got, _ := v.Read("a.md")
	if string(got) != "second" {
		t.Errorf("Read = %q, want second", got)
	}
}

func TestRemove(t *testing.T) {
	v, _ := newVault(t)
	v.Write("a.md", []byte("x"))
	if err := v.Remove("a.md"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := v.Read("a.md"); err == nil {
		t.Error("file still readable after Remove")
	}
	// Removing something absent is not an error -- a delete we already applied
	// must be idempotent, or replaying a change set fails spuriously.
	if err := v.Remove("a.md"); err != nil {
		t.Errorf("second Remove: %v, want nil", err)
	}
}

func TestStat(t *testing.T) {
	v, _ := newVault(t)
	v.Write("a.md", []byte("12345"))
	st, err := v.Stat("a.md")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if st.Size() != 5 {
		t.Errorf("Size = %d, want 5", st.Size())
	}
}

// Exclusion policy lives in Skip, and Skip is what both the watcher and the
// commit-staging path consult. Testing it directly is closer to the thing that
// matters than walking a directory was.
func TestSkipExcludesDotfilesAtEveryLevel(t *testing.T) {
	for _, p := range []string{
		// workspace.json rather than appearance.json: the latter is now
		// allowlisted for config sync, so asserting it is skipped would assert
		// the opposite of the policy. See TestSkipAllowsAllowlistedConfig.
		".obsidian/workspace.json",
		"notes/.hidden.md",
		".trash/old.md",
		".git",
		tmpPrefix + "1234",
		"notes/" + tmpPrefix + "abcd",
	} {
		if !Skip(p) {
			t.Errorf("Skip(%q) = false, want true", p)
		}
	}
	for _, p := range []string{"a.md", "notes/deep/b.md", "att/scan.pdf", "no.dot.here.md"} {
		if Skip(p) {
			t.Errorf("Skip(%q) = true, want false", p)
		}
	}
}

func TestHashIsStableAndPrefixed(t *testing.T) {
	a := Hash([]byte("hello"))
	b := Hash([]byte("hello"))
	c := Hash([]byte("hello!"))
	if a != b {
		t.Errorf("Hash not stable: %q vs %q", a, b)
	}
	if a == c {
		t.Error("different content produced the same hash")
	}
	if !strings.HasPrefix(a, "sha256:") {
		t.Errorf("Hash = %q, want a sha256: prefix", a)
	}
}

func TestSkipStillRefusesEverythingElseDotted(t *testing.T) {
	for _, p := range []string{
		".obsidian/workspace.json",
		".obsidian/plugins/archivist/data.json",
		".obsidian/plugins/dataview/main.js",
		".trash/gone.md",
		".smart-env/cache.json",
		"notes/.hidden.md",
	} {
		if !Skip(p) {
			t.Errorf("Skip(%q) = false; it must stay excluded", p)
		}
	}
}

func TestSkipStillRefusesOurTempFiles(t *testing.T) {
	if !Skip(".archivist-tmp-1234-abcdef01") {
		t.Error("temp files must never be committed")
	}
	if !Skip("notes/.archivist-tmp-1234-abcdef01") {
		t.Error("temp files must never be committed, at any depth")
	}
}
