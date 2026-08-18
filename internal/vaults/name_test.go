package vaults

import "testing"

func TestLegalNames(t *testing.T) {
	for _, n := range []string{
		"personal", "work", "this-vault-name", "MyVault", "My Own Vault",
		"vault2", "työ", "a",
	} {
		if err := ValidName(n); err != nil {
			t.Errorf("ValidName(%q) = %v, want nil", n, err)
		}
	}
}

func TestIllegalNames(t *testing.T) {
	for _, n := range []string{
		"",             // nothing
		".",            // current directory
		"..",           // parent
		".hidden",      // leading dot: invisible, and collides with .archivist
		"a/b",          // more than one path segment
		"a\\b",         // a segment separator on the other platform
		"../escape",    // traversal
		"trailing/",    // a separator at all
		"v1",           // reserved: /v1/vaults lives at the server root
		"healthz",      // reserved: the unauthenticated liveness route
		"vaults",       // reserved: confusing under $ROOT/vaults/
		"with\x00null", // a NUL byte
		"\n",           // control characters
	} {
		if err := ValidName(n); err == nil {
			t.Errorf("ValidName(%q) = nil; it must be refused", n)
		}
	}
}

func TestNameLengthIsBounded(t *testing.T) {
	long := ""
	for i := 0; i < 256; i++ {
		long += "a"
	}
	if err := ValidName(long); err == nil {
		t.Error("a 256-character name must be refused; most filesystems cap a component at 255")
	}
}

// macOS and iOS already disagreed about "ä" in this vault. "työ" created from a
// Mac and "työ" created from a phone would be two directories that look
// identical and serve different content.
func TestNFCCollisionIsRefused(t *testing.T) {
	// Built from escapes, not typed literally: the two spellings are
	// indistinguishable on screen, so literals would silently become two
	// copies of the same string and the test would pass for the wrong reason.
	composed := "ty\u00f6"    // o-with-diaeresis as ONE code point
	decomposed := "tyo\u0308" // o + COMBINING DIAERESIS

	if Collides(composed, []string{decomposed}) == "" {
		t.Error("a decomposed existing name must collide with a composed new one")
	}
	if Collides(decomposed, []string{composed}) == "" {
		t.Error("and the other way round")
	}
	if got := Collides("work", []string{"personal", "ty\u00f6"}); got != "" {
		t.Errorf("Collides returned %q for a name that does not collide", got)
	}
}

func TestCollidesReportsTheExistingSpelling(t *testing.T) {
	existing := "tyo\u0308"                                     // decomposed
	got := Collides("ty\u00f6", []string{"personal", existing}) // composed
	if got != existing {
		t.Errorf("Collides = %q, want the EXISTING spelling %q so the message can name it", got, existing)
	}
}

// An exact repeat is a collision too -- it is the ordinary "already exists".
func TestIdenticalNameCollides(t *testing.T) {
	if Collides("work", []string{"work"}) != "work" {
		t.Error("an existing name must collide with itself")
	}
}
