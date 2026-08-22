package vault

import "testing"

// The .local namespace is a user-facing promise: name a file that way and it
// never leaves the device. These cases are the promise, and the same table
// exists in test/unit/local-only.test.mjs -- the two predicates must agree, or
// one side pushes a file the other refuses and the client retries it forever.
func TestLocalOnly(t *testing.T) {
	for _, tc := range []struct {
		path string
		want bool
	}{
		// The user's own opt-in.
		{"Scratch.local.md", true},
		{"Notes/Machine.local.md", true},
		// What materialisation writes.
		{"Note.ae56b1c.local.md", true},
		{"attachments/diagram.8f1c2ad.local.png", true},
		// Not the namespace: nothing before "local", so this is just a note
		// someone called "local".
		{"local.md", false},
		// "local" must be the second-to-last segment, not the last.
		{"Note.local", false},
		{"Notes/plan.local", false},
		// A DIRECTORY called local does not make its contents local-only; the
		// rule is about the basename, so a person cannot exclude a whole tree
		// by accident.
		{"notes.local/inside.md", false},
		// Case-sensitive, so a title-cased word in a filename is safe.
		{"Note.Local.md", false},
		{"Report.LOCAL.md", false},
		// Ordinary notes are untouched.
		{"Daily/2026-08-22.md", false},
		{"Note.md", false},
	} {
		if got := LocalOnly(tc.path); got != tc.want {
			t.Errorf("LocalOnly(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// Skip must apply .local BEFORE the dotfile and config-allowlist rules.
// Otherwise an allowlisted config path could carry the marker and still sync,
// which would break the "never, for anyone" promise at exactly the place a
// user is least likely to look.
func TestSkipAppliesLocalOnlyBeforeConfigAllowlist(t *testing.T) {
	if !Skip("Scratch.local.md") {
		t.Error("Skip did not exclude a plain .local note")
	}
	// appearance.json is allowlisted; the .local form of it must not be.
	if !Skip(".obsidian/appearance.local.json") {
		t.Error("Skip let a .local file through the config allowlist")
	}
	if Skip("Note.md") {
		t.Error("Skip excluded an ordinary note")
	}
}
