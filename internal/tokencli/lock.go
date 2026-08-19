package tokencli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// lockTimeout bounds how long a command waits for another to finish. Generous
// relative to the work — a mint is a file read, a hash and a rename — so
// reaching it means a stale lock rather than contention.
const lockTimeout = 5 * time.Second

// withFileLock runs fn while holding an exclusive lock on path's sibling lock
// file, so two commands cannot interleave a load-modify-save.
//
// Without it the dangerous ordering is not a lost mint but a resurrected
// revocation: `add` reads the table, `revoke` saves a table without the doomed
// hash, then `add` saves ITS copy -- which still contains that hash -- and the
// server's watcher faithfully reloads the credential you just revoked.
//
// O_EXCL on a sibling file rather than flock: it needs no syscall package, so
// it behaves the same everywhere the server builds, and the lock is a real file
// an operator can see and delete when something has died holding it.
func withFileLock(path string, fn func() error) error {
	lock := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".lock")
	deadline := time.Now().Add(lockTimeout)

	for {
		f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			fmt.Fprintf(f, "%d\n", os.Getpid())
			f.Close()
			defer os.Remove(lock)
			return fn()
		}
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("token: cannot take the lock at %s: %w", lock, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf(
				"token: %s is held by another command after %s. If nothing else is "+
					"running, a previous one died holding it -- delete the file and retry",
				lock, lockTimeout)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
