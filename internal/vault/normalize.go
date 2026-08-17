package vault

import (
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"path/filepath"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// NFC reports whether a path is already in composed form.
func NFC(rel string) bool { return norm.NFC.IsNormalString(rel) }

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
func ToNFC(rel string) string { return norm.NFC.String(rel) }

// NormalizeToNFC renames every syncable file whose path is not already composed.
//
// This changes files on disk, which is why it is off unless asked for. It has to
// touch the disk: on Linux the two spellings are genuinely different paths, so
// normalising only the keys we compare would leave a name in git that does not
// match the file beside it.
//
// Returns the paths it renamed. A collision -- both spellings present as
// separate files -- is reported and left alone rather than resolved by picking a
// winner, because the two files may have different content and only a person can
// say which matters.
func (v *Vault) NormalizeToNFC(log *slog.Logger) ([]string, error) {
	var renamed []string
	var collisions []string

	// Collect first, rename after: renaming during a walk is undefined.
	//
	// present holds the paths as the directory actually spells them, which is
	// what a collision has to be judged against. Using Stat for that is wrong on
	// macOS, where lookup is normalisation-insensitive: Stat of the composed name
	// finds the decomposed file, every rename looks like a collision, and nothing
	// is ever normalised. Membership in the real listing means the same thing on
	// both platforms.
	present := map[string]bool{}
	var todo []string
	err := fs.WalkDir(v.root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == "." {
			return nil
		}
		if Skip(p) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		present[p] = true
		if !NFC(p) {
			todo = append(todo, p)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("vault: walk for normalisation: %w", err)
	}

	for _, old := range todo {
		neu := ToNFC(old)
		if neu == old {
			continue
		}
		if present[neu] {
			// Both spellings exist as separate entries. Picking one could
			// discard content, so say so and move on.
			collisions = append(collisions, old)
			log.Warn("vault: both spellings of a filename exist; leaving them alone",
				"decomposed", old, "composed", neu)
			continue
		}
		// The PARENT, not the target. mkdirAll creates every segment it is
		// given, so passing the full path created a DIRECTORY named
		// "Oirepäiväkirja.md" and the rename then failed with "file exists".
		// macOS never showed it -- there the rename is a no-op the kernel folds
		// away -- and only running this package on Linux exposed it.
		//
		// A parent is needed at all because a DIRECTORY name can itself be
		// decomposed: only files are renamed here, so the normalised path of a
		// file under a decomposed directory has a parent that does not exist yet.
		if parent := path.Dir(neu); parent != "." {
			if err := v.mkdirAll(parent); err != nil {
				return renamed, fmt.Errorf("vault: mkdir %s: %w", parent, err)
			}
		}
		if err := v.root.Rename(old, neu); err != nil {
			return renamed, fmt.Errorf("vault: rename %s -> %s: %w", old, neu, err)
		}
		renamed = append(renamed, neu)
		log.Info("vault: normalised filename to NFC", "from", old, "to", neu)
	}

	if len(collisions) > 0 {
		log.Warn("vault: filenames left unnormalised because both spellings exist",
			"count", len(collisions))
	}
	if len(renamed) > 0 {
		log.Info("vault: filename normalisation complete", "renamed", len(renamed),
			"dirs", len(uniqueDirs(renamed)))
	}
	return renamed, nil
}

func uniqueDirs(paths []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, p := range paths {
		d := filepath.Dir(p)
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	return out
}

// NormalizeSlash is a convenience for callers holding a path with OS separators.
func NormalizeSlash(p string) string { return ToNFC(filepath.ToSlash(strings.TrimPrefix(p, "./"))) }
