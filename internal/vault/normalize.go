package vault

import (
	"golang.org/x/text/unicode/norm"
)

// ToNFC returns the composed form of a path.
//
// macOS and iOS disagree about how to store a filename containing "ä". macOS
// writes it decomposed (NFD: "a" + combining diaeresis) and iOS writes it
// composed (NFC: a single code point). On Linux those are two different
// filenames, so a vault synced from both ends up with the same note under two
// spellings, or renamed back and forth between devices forever.
//
// Observed, not theorised: an iPhone renamed five notes on first sync --
// "Oirepäiväkirja.md" to "Oirepäiväkirja.md", identical to the eye, zero content
// change -- because the Mac had written them decomposed.
//
// NFC is the right canonical form: it is what iOS produces, what the web
// platform normalises to, and what most Linux tooling assumes.
//
// This only canonicalises a path a caller already holds. Nothing here renames
// files on disk: a vault that already carries both spellings keeps them until a
// person decides which one wins.
func ToNFC(rel string) string { return norm.NFC.String(rel) }
