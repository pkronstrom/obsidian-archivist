package vaults

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Layout is where vaults and their histories live.
//
//	$ROOT/vaults/<name>/       the vault: plain Markdown and attachments
//	$ROOT/.archivist/<name>/   its history, never inside the vault
//
// A dedicated vaults/ container rather than scanning $ROOT directly, because
// there is then no way a folder called "2. Areas" is mistaken for a vault --
// and no way a vault is mistaken for a note folder either.
//
// This is the layout already deployed: compose sets
// ARCHIVIST_GIT=/data/.archivist/${VAULT_NAME} against a single /data mount.
// Multi-vault makes it the server's own convention rather than a deployment
// one, which is why the binary's default changes with it.
type Layout struct{ Root string }

func (l Layout) VaultsDir() string { return filepath.Join(l.Root, "vaults") }
func (l Layout) StateDir() string  { return filepath.Join(l.Root, ".archivist") }

func (l Layout) VaultDir(name string) string { return filepath.Join(l.VaultsDir(), name) }
func (l Layout) GitDir(name string) string   { return filepath.Join(l.StateDir(), name) }

// Discover lists the vault directories present right now, sorted.
//
// A readdir is nearly free, which is what makes rescanning on request practical
// -- and rescanning is what avoids the failure where a vault is rsynced in,
// nothing appears, and nothing says why.
//
// An absent vaults/ directory yields no vaults and no error: that is a server
// started against a fresh root, not a broken one.
func (l Layout) Discover() ([]string, error) {
	entries, err := os.ReadDir(l.VaultsDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("vaults: reading %s: %w", l.VaultsDir(), err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if ValidName(e.Name()) != nil {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

// CheckRoot refuses a root that is itself a vault, or that holds a vault with
// its own repository inside it.
//
// Both are the same mistake seen from two sides: pointing ROOT at a directory
// of notes. Started that way the server would happily serve $ROOT/vaults as a
// note folder, discover no vaults, and say nothing -- and the git directory it
// wrote would sit inside content that syncs to every device.
//
// The repository lives OUTSIDE the working tree by design (see the package
// comment on internal/repo), so a .git inside either is proof that something
// else put it there.
func (l Layout) CheckRoot() error {
	if badGitEntry(filepath.Join(l.Root, ".git"), "") {
		return fmt.Errorf(
			"vaults: %s contains a .git directory, so it looks like a vault rather than a "+
				"root holding vaults/. Point ARCHIVIST_ROOT at the PARENT: vaults go in "+
				"$ROOT/vaults/<name> and their history in $ROOT/.archivist/<name>",
			l.Root)
	}

	names, err := l.Discover()
	if err != nil {
		return err
	}
	for _, n := range names {
		if badGitEntry(filepath.Join(l.VaultDir(n), ".git"), l.GitDir(n)) {
			return fmt.Errorf(
				"vaults: %s contains its own .git directory. Archivist keeps history OUTSIDE "+
					"the working tree, at %s, so no client and no other tool ever sees a .git "+
					"inside the notes. Move or remove it before starting",
				l.VaultDir(n), l.GitDir(n))
		}
	}
	return nil
}

// EnsureDirs creates the two container directories. Called once at startup so a
// fresh root works without a mkdir in the deployment.
func (l Layout) EnsureDirs() error {
	for _, d := range []string{l.VaultsDir(), l.StateDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("vaults: creating %s: %w", d, err)
		}
	}
	return nil
}

// Describe renders the layout for a startup log line, so a misconfiguration is
// visible in the first three lines of `docker logs` rather than inferred.
func (l Layout) Describe(names []string) string {
	if len(names) == 0 {
		return fmt.Sprintf("no vaults under %s", l.VaultsDir())
	}
	return fmt.Sprintf("%d vault(s) under %s: %s", len(names), l.VaultsDir(), strings.Join(names, ", "))
}

// badGitEntry reports whether a .git at path is an embedded repository rather
// than the deliberate pointer this layout creates.
//
// A `.git` FILE holding `gitdir: <path>` is not a repository -- it is a
// redirect, and this deployment writes one on purpose so `git -C <vault> log`
// works from the host without knowing where history actually lives. Refusing
// every .git entry took the whole server down on a vault that was correctly
// laid out, which is a worse failure than the one the guard exists to catch.
//
// So: a DIRECTORY is always wrong, and a pointer is wrong only when it points
// somewhere other than this vault's own git directory. wantGitDir empty means
// no pointer is acceptable here, which is the case for $ROOT itself -- the root
// holds vaults, it is not one.
func badGitEntry(path, wantGitDir string) bool {
	fi, err := os.Lstat(path)
	if err != nil {
		return false // absent, which is the ordinary case
	}
	if fi.IsDir() {
		return true // an embedded repository: the mistake this guard is for
	}
	if wantGitDir == "" {
		return true
	}
	target, err := gitdirTarget(path)
	if err != nil {
		return true // unreadable or not a pointer at all
	}
	want, err := filepath.Abs(wantGitDir)
	if err != nil {
		return true
	}
	got, err := filepath.Abs(target)
	if err != nil {
		return true
	}
	return filepath.Clean(got) != filepath.Clean(want)
}

// gitdirTarget reads a `gitdir: <path>` pointer, resolving a relative path
// against the directory holding the pointer, exactly as git does.
func gitdirTarget(pointer string) (string, error) {
	b, err := os.ReadFile(pointer)
	if err != nil {
		return "", err
	}
	rest, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir:")
	if !ok {
		return "", fmt.Errorf("vaults: %s is not a gitdir pointer", pointer)
	}
	target := strings.TrimSpace(rest)
	if filepath.IsAbs(target) {
		return target, nil
	}
	return filepath.Join(filepath.Dir(pointer), target), nil
}
