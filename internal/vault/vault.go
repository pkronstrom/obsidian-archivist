// Package vault is all filesystem access to the working tree.
//
// Every operation is confined to the vault root by os.Root, which enforces
// containment in the kernel (openat2 / O_NOFOLLOW-style walks) rather than by
// comparing strings.
//
// A purely lexical check is NOT sufficient, and this is not theoretical. The
// obvious implementation --
//
//	abs := filepath.Join(root, filepath.Clean("/"+rel))
//	if !strings.HasPrefix(abs, root+"/") { return error }
//
// -- accepts "../escape.md", because Clean clamps ".." at the virtual root and
// yields "/escape.md", which joins back INSIDE root and passes the prefix
// guard. "a/../../b.md" does the same. Verified by running it. And no lexical
// check at all can stop a symlink inside the vault pointing out of it.
//
// So: filepath.IsLocal rejects the path shapes, and os.Root handles the rest.
package vault

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// tmpPrefix is a dotfile so Obsidian hides it, the watcher ignores it, and it
// never reaches git.
const tmpPrefix = ".archivist-tmp-"

type Vault struct {
	root *os.Root
	dir  string
}

func New(dir string) (*Vault, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &Vault{root: r, dir: dir}, nil
}

func (v *Vault) Close() error { return v.root.Close() }

// Dir is the absolute path of the vault root. Only for wiring up components
// that need it (go-git's worktree); never for building paths by hand.
func (v *Vault) Dir() string { return v.dir }

// check rejects anything that is not a plain relative path inside the vault.
// filepath.IsLocal is false for "", ".", "..", absolute paths, and any path
// whose cleaned form escapes the root.
// ErrInvalidPath is returned for any path that is not local to the vault, so
// callers can classify it as client fault without matching on message text.
var ErrInvalidPath = errors.New("vault: path is not local to the vault")

func check(rel string) error {
	if !filepath.IsLocal(rel) {
		return fmt.Errorf("%w: %q", ErrInvalidPath, rel)
	}
	return nil
}

// ValidPath reports whether a client-supplied path is acceptable at all.
// Callers should use this to reject bad input loudly, rather than letting it
// fall through to Skip and be reported as a routine exclusion -- an escaping
// path is a broken or hostile client, not a dotfile.
func ValidPath(rel string) error { return check(rel) }

func (v *Vault) Read(rel string) ([]byte, error) {
	if err := check(rel); err != nil {
		return nil, err
	}
	f, err := v.root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

func (v *Vault) Stat(rel string) (fs.FileInfo, error) {
	if err := check(rel); err != nil {
		return nil, err
	}
	return v.root.Stat(rel)
}

// mkdirAll creates rel's ancestors inside the root. os.Root has no MkdirAll,
// and doing it segment by segment keeps every step confined.
func (v *Vault) mkdirAll(rel string) error {
	rel = path.Clean(rel)
	if rel == "." || rel == "/" {
		return nil
	}
	var built string
	for _, seg := range strings.Split(rel, "/") {
		if seg == "" {
			continue
		}
		if built == "" {
			built = seg
		} else {
			built = built + "/" + seg
		}
		if err := v.root.Mkdir(built, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
	}
	return nil
}

// Write replaces a file atomically: a temp file in the SAME directory, synced,
// then renamed over the target. POSIX rename is atomic, so a concurrent reader
// -- SilverBullet, a script, Obsidian -- sees either the whole old file or the
// whole new one, never a partial write.
//
// Same-directory is load-bearing: a rename across filesystems is not atomic and
// would silently degrade to copy-then-delete.
func (v *Vault) Write(rel string, data []byte) error {
	if err := check(rel); err != nil {
		return err
	}
	dir := path.Dir(rel)
	if err := v.mkdirAll(dir); err != nil {
		return err
	}

	tmp := path.Join(dir, fmt.Sprintf("%s%d", tmpPrefix, os.Getpid()))
	if dir == "." {
		tmp = fmt.Sprintf("%s%d", tmpPrefix, os.Getpid())
	}
	// A unique suffix per call, so concurrent writes to different paths in one
	// directory cannot collide on the temp name.
	tmp = tmp + "-" + Hash([]byte(rel))[7:15]

	f, err := v.root.Create(tmp)
	if err != nil {
		return err
	}
	cleanup := func() { _ = v.root.Remove(tmp) }

	if _, err := f.Write(data); err != nil {
		f.Close()
		cleanup()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		cleanup()
		return err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return err
	}
	if err := v.root.Rename(tmp, rel); err != nil {
		cleanup()
		return err
	}
	return nil
}

// Remove deletes a file. Absent is not an error: applying a delete twice must
// be a no-op, or replaying a change set fails spuriously.
func (v *Vault) Remove(rel string) error {
	if err := check(rel); err != nil {
		return err
	}
	if err := v.root.Remove(rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// isTemp reports whether a path is one of our own in-flight temp files.
func isTemp(rel string) bool {
	return strings.HasPrefix(path.Base(rel), tmpPrefix)
}

// Skip reports whether a path is excluded from sync entirely.
//
// Dotfiles at any level are skipped: .git never appears here (the git dir lives
// outside the vault) but .trash and editor droppings do, and none of them
// belong in the synced set.
//
// The one exception is an allowlisted path inside the Obsidian configuration
// directory -- see config.go. That is a WIDENING of this refusal, not a
// loosening of it: everything not named there is still refused, and the
// allowlist is consulted from exactly this one predicate so the four places
// that use it cannot disagree about what may sync:
//
//	internal/reconcile/reconcile.go  applyOne, before any remote write
//	internal/repo/repo.go            Commit, at staging, for BOTH write paths
//	internal/repo/history.go         Check, so exclusions are not reported as drift
//	internal/watcher/watcher.go      handle, so excluded events are dropped
//
// The watcher's addTree is the fifth and does NOT go through here; it tests
// directory names directly and is handled alongside this change.
func Skip(rel string) bool {
	if isTemp(rel) {
		return true
	}
	// Checked BEFORE the dotfile rules, and never widened by the config
	// allowlist: .local is the one exclusion a person opts into by naming a
	// file, so it has to beat every other rule including the allowlist that
	// otherwise lets specific config paths through.
	if LocalOnly(rel) {
		return true
	}
	for _, seg := range strings.Split(rel, "/") {
		if strings.HasPrefix(seg, ".") {
			return !ConfigSyncable(rel)
		}
	}
	return false
}

// LocalOnly reports whether a path is in the user's never-sync namespace.
//
// One rule, in two shapes a person can hold in their head:
//
//	Scratch.local.md   a file, marked before its extension
//	Scratch.local/     a folder -- nothing inside it ever syncs
//	Notes/Plan.local   a file with no extension, same idea
//
// It is a FEATURE, not an internal marker: naming something .local is how you
// keep it on one device, which is why it must be documented in the plugin
// settings and the README. A file that silently stops syncing is only safe
// when the user chose the name that stopped it.
//
// The two shapes exist because a folder cannot use the file spelling: a
// directory called "project.local.assets" would match "marked before the
// extension" purely by coincidence, and excluding it would strand every note
// inside a tree nobody meant to hide. Requiring a directory to END in .local
// makes the deliberate case unambiguous and the accidental case impossible.
//
// The revision browser materialises Note.<hash>.local.md and so lands in this
// namespace by construction; renaming the marker away is what "restores" it.
func LocalOnly(rel string) bool {
	seg := strings.Split(rel, "/")
	for _, s := range seg {
		// Any ancestor marked .local takes its whole subtree with it.
		if strings.HasSuffix(s, ".local") && len(s) > len(".local") {
			return true
		}
	}
	base := strings.Split(seg[len(seg)-1], ".")
	return len(base) >= 3 && base[len(base)-2] == "local"
}

// LocalOnlyDir is LocalOnly for a DIRECTORY name, without the file grammar.
//
// Traversal must use this rather than LocalOnly: "project.local.assets" is a
// perfectly ordinary folder that happens to match the file spelling, and
// refusing to descend into it would silently stop everything inside it from
// syncing -- inconsistently, too, since a startup walk that only skips
// dot-directories would descend into it anyway.
func LocalOnlyDir(rel string) bool {
	for _, s := range strings.Split(rel, "/") {
		if strings.HasSuffix(s, ".local") && len(s) > len(".local") {
			return true
		}
	}
	return false
}

// Hash is the content address used on the wire.
func Hash(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}
