package vault

import (
	"testing"

	"golang.org/x/text/unicode/norm"
)

// The two spellings of "ä": macOS writes the decomposed form, iOS the composed
// one, and on Linux they are different filenames.
const (
	nfdName = "Oirepäiväkirja.md" // a + combining diaeresis
	nfcName = "Oirepäiväkirja.md"   // precomposed ä
)

func TestToNFCComposes(t *testing.T) {
	if got := ToNFC(nfdName); got != nfcName {
		t.Errorf("ToNFC(%q) = %q, want %q", nfdName, got, nfcName)
	}
}

func TestToNFCIsIdempotent(t *testing.T) {
	once := ToNFC(nfdName)
	if twice := ToNFC(once); twice != once {
		t.Errorf("not idempotent: %q then %q", once, twice)
	}
	if !norm.NFC.IsNormalString(once) {
		t.Error("result of ToNFC is not composed")
	}
}
