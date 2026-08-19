package auth

import (
	"context"
	"log/slog"
	"path/filepath"

	"github.com/fsnotify/fsnotify"
)

// Watch re-reads path whenever it changes, so minting a token is not an outage.
//
// It watches the DIRECTORY, not the file. Save writes a temp file and renames
// it over the target, which replaces the inode -- a watch on the file itself
// would follow the old inode and go deaf after the first mint. Watching the
// directory and filtering by name survives any number of replacements.
//
// A file that fails to parse is logged and IGNORED: the live table keeps what
// it had. The alternative -- emptying it -- turns a typo into a lockout from
// every device at once.
//
// Watch returns once the watcher is established, and runs until ctx is done.
func (s *Set) Watch(ctx context.Context, path string, log *slog.Logger) error {
	if path == "" {
		return nil // bootstrap mode: there is no file to watch
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	dir, name := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	if err := w.Add(dir); err != nil {
		w.Close()
		return err
	}

	go func() {
		defer w.Close()
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				if filepath.Base(ev.Name) != name {
					continue
				}
				// Create covers the rename-into-place that Save performs; Write
				// covers a hand-edited file saved in place.
				if !ev.Has(fsnotify.Create) && !ev.Has(fsnotify.Write) {
					continue
				}
				next, err := Load(path, "")
				if err != nil {
					log.Error("tokens: the file changed but does not load; keeping the previous table",
						"path", path, "err", err)
					continue
				}
				s.Replace(next)
				log.Info("tokens: reloaded", "path", path, "tokens", len(next.Entries()))
			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				log.Warn("tokens: watcher error", "err", err)
			}
		}
	}()
	return nil
}
