package vault

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// The two spellings of "ä": macOS writes the decomposed form, iOS the composed
// one, and on Linux they are different filenames.
const (
	nfdName = "Oirepäiväkirja.md" // a + combining diaeresis
	nfcName = "Oirepäiväkirja.md"   // precomposed ä
)

// requireDistinctSpellings skips when the filesystem cannot hold both spellings
// of a name at once.
//
// macOS PRESERVES the bytes you write but folds them on LOOKUP, so writing the
// composed and decomposed spellings hits one file and the second write silently
// overwrites the first. Testing preservation is therefore not enough -- an
// earlier version of this helper did exactly that and let an impossible test run.
//
// Detected at runtime rather than by GOOS, because it is a property of the
// filesystem and not of the compiler target. The server runs on Linux, where the
// spellings are genuinely different paths, so this case is meaningful there and
// unconstructible here. Verified by running this package in a Linux container.
func requireDistinctSpellings(t *testing.T, v *Vault) {
	t.Helper()
	a, b := "probe-a-"+nfcName, "probe-a-"+nfdName
	if err := v.Write(a, []byte("one")); err != nil {
		t.Fatal(err)
	}
	if err := v.Write(b, []byte("two")); err != nil {
		t.Fatal(err)
	}
	defer func() { v.Remove(a); v.Remove(b) }()
	first, _ := v.Read(a)
	if string(first) != "one" {
		t.Skip("filesystem folds Unicode spellings together; both cannot coexist here")
	}
}

func TestNFDNameIsRenamedToNFC(t *testing.T) {
	v, _ := newVault(t)
	if err := v.Write(nfdName, []byte("body\n")); err != nil {
		t.Fatal(err)
	}

	renamed, err := v.NormalizeToNFC(quiet())
	if err != nil {
		t.Fatal(err)
	}
	if len(renamed) != 1 || renamed[0] != nfcName {
		t.Fatalf("renamed = %q, want [%q]", renamed, nfcName)
	}
	if _, err := v.Stat(nfcName); err != nil {
		t.Errorf("composed name not present after normalising: %v", err)
	}
	body, err := v.Read(nfcName)
	if err != nil || string(body) != "body\n" {
		t.Errorf("content did not survive the rename: %q %v", body, err)
	}
}

func TestAlreadyNFCIsLeftAlone(t *testing.T) {
	v, _ := newVault(t)
	v.Write(nfcName, []byte("body\n"))
	v.Write("plain.md", []byte("x\n"))

	renamed, err := v.NormalizeToNFC(quiet())
	if err != nil {
		t.Fatal(err)
	}
	if len(renamed) != 0 {
		t.Errorf("renamed %q; nothing needed renaming", renamed)
	}
}

// Both spellings present as separate files. Picking a winner could discard
// content, so the pass must report and skip rather than resolve.
func TestBothSpellingsPresentIsLeftAlone(t *testing.T) {
	v, _ := newVault(t)
	requireDistinctSpellings(t, v)
	if err := v.Write(nfcName, []byte("composed content\n")); err != nil {
		t.Fatal(err)
	}
	if err := v.Write(nfdName, []byte("decomposed content\n")); err != nil {
		t.Fatal(err)
	}

	renamed, err := v.NormalizeToNFC(quiet())
	if err != nil {
		t.Fatalf("a collision must not be an error: %v", err)
	}
	if len(renamed) != 0 {
		t.Errorf("renamed %q despite a collision; content could have been lost", renamed)
	}
	// BOTH must survive untouched.
	a, _ := v.Read(nfcName)
	b, _ := v.Read(nfdName)
	if string(a) != "composed content\n" || string(b) != "decomposed content\n" {
		t.Errorf("collision changed content: %q / %q", a, b)
	}
}

func TestNormalisationSkipsDotfiles(t *testing.T) {
	v, _ := newVault(t)
	dir := filepath.Join(v.Dir(), ".obsidian")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, nfdName), []byte("cfg\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	renamed, err := v.NormalizeToNFC(quiet())
	if err != nil {
		t.Fatal(err)
	}
	if len(renamed) != 0 {
		t.Errorf("touched an excluded path: %q", renamed)
	}
}

func TestToNFCIsIdempotent(t *testing.T) {
	once := ToNFC(nfdName)
	if twice := ToNFC(once); twice != once {
		t.Errorf("not idempotent: %q then %q", once, twice)
	}
	if !NFC(once) {
		t.Error("result of ToNFC is not reported as NFC")
	}
	if NFC(nfdName) {
		t.Error("decomposed name wrongly reported as NFC")
	}
}
