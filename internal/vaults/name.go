// Package vaults is discovery, naming and per-vault state.
//
// One process serves every vault under a root directory. Each vault gets its
// own working tree, repository, reconciler, guard and filesystem watcher, so
// nothing is shared between them but the process and the listening socket --
// which is the property that makes a single leaked token's blast radius exactly
// the vaults that token names.
package vaults

import (
	"fmt"
	"strings"

	"github.com/pkronstrom/obsidian-archivist/internal/vault"
)

// maxNameLen is one filesystem path component. 255 is the limit on ext4, APFS
// and every filesystem this is likely to meet; refusing at the API is a clearer
// error than a mkdir failing later.
const maxNameLen = 255

// reserved names would be ambiguous or unreachable in the URL space.
//
// "v1" and "healthz" are routes at the server root. Go's ServeMux would in fact
// route /v1/v1/head correctly, because the patterns differ in length -- but a
// vault whose name is a route prefix is a thing nobody should have to reason
// about. "vaults" is refused because $ROOT/vaults/vaults reads as a mistake.
var reserved = map[string]bool{"v1": true, "healthz": true, "vaults": true}

// ValidName reports whether a name may be used for a vault.
//
// ONE path segment, no leading dot, no separator, no traversal. "this-vault",
// "MyVault" and "My Own Vault" are all legal; the last is URL-encoded as
// My%20Own%20Vault, which the plugin does automatically and a human writing
// curl by hand must remember. URL-safe names are easier by hand and worth
// preferring, but forbidding a space would be arbitrary.
func ValidName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("vault name is empty")
	case len(name) > maxNameLen:
		return fmt.Errorf("vault name is longer than %d bytes", maxNameLen)
	case name == "." || name == "..":
		return fmt.Errorf("vault name %q is a directory reference", name)
	case strings.HasPrefix(name, "."):
		// A leading dot would make the directory invisible, and .archivist sits
		// beside vaults/ for exactly this reason.
		return fmt.Errorf("vault name %q may not begin with a dot", name)
	case strings.ContainsAny(name, `/\`):
		return fmt.Errorf("vault name %q must be a single path segment", name)
	case reserved[strings.ToLower(name)]:
		return fmt.Errorf("vault name %q is reserved", name)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("vault name contains a control character")
		}
	}
	return nil
}

// Collides returns the EXISTING name that a new one would be confused with, or
// "" when there is none. Comparison is on the NFC form, so a composed and a
// decomposed spelling of the same word count as the same name.
//
// This is not theoretical. macOS and iOS already disagreed about "ä" in this
// vault, and "työ" created from a Mac against "työ" created from a phone would
// be two directories that look identical in every listing and serve different
// content -- with no error anywhere and no way to tell which is which.
//
// The existing spelling is returned rather than a bool so the refusal can name
// what is already there, which is the only thing that makes it actionable.
func Collides(name string, existing []string) string {
	want := vault.ToNFC(name)
	for _, e := range existing {
		if vault.ToNFC(e) == want {
			return e
		}
	}
	return ""
}
