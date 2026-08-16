// Package watcher turns local filesystem edits into commits.
//
// Two things here are easy to get wrong and expensive to debug:
//
//  1. fsnotify is NOT recursive. A watch covers one directory; subdirectories
//     need their own. Miss that and edits below a newly created folder are
//     silently invisible -- which presents as "sync just doesn't see that
//     folder", with no error anywhere.
//
//  2. The server writes to the working tree itself when applying a push, and
//     its own watcher sees those writes. Without suppression that is an
//     endless loop of no-op commits.
//
// Startup order is scan -> watch -> rescan. The final rescan closes the window
// between the first scan and the watches becoming live.
package watcher

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/pkronstrom/obsidian-archivist/internal/reconcile"
	"github.com/pkronstrom/obsidian-archivist/internal/vault"
)

type Watcher struct {
	v        *vault.Vault
	rc       *reconcile.Reconciler
	debounce time.Duration
	log      *slog.Logger

	fsw *fsnotify.Watcher

	mu       sync.Mutex
	pending  map[string]struct{}
	timer    *time.Timer
	firstAt  time.Time
	maxDelay time.Duration
}

func New(v *vault.Vault, rc *reconcile.Reconciler, debounce time.Duration, log *slog.Logger) *Watcher {
	// maxDelay caps how long sustained activity can postpone a commit.
	//
	// A plain debounce re-arms on every event, so a steady stream of writes --
	// a long editing session, an rsync, a script generating notes -- defers the
	// commit indefinitely. Measured: 60 writes at 80ms intervals with a 200ms
	// debounce produced ZERO commits until the writes stopped. Everything was
	// still on disk, but nothing was in history, so a backup taken during that
	// window captured no history at all.
	maxDelay := 10 * debounce
	if maxDelay < 5*time.Second {
		maxDelay = 5 * time.Second
	}
	return &Watcher{
		v: v, rc: rc, debounce: debounce, log: log,
		pending: map[string]struct{}{}, maxDelay: maxDelay,
	}
}

// Run performs the startup reconciliation and then watches until ctx is done.
//
// The startup scan is not optional: fsnotify replays nothing, so without it
// every edit made while the process was stopped stays invisible forever.
func (w *Watcher) Run(ctx context.Context) error {
	if _, err := w.rc.Scan("startup scan"); err != nil {
		return err
	}

	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	w.fsw = fsw
	defer fsw.Close()

	if err := w.addTree(w.v.Dir()); err != nil {
		return err
	}

	// Rescan: anything created between the startup scan and the watches going
	// live would otherwise be missed until something else touched it.
	if _, err := w.rc.Scan("startup rescan"); err != nil {
		return err
	}
	w.log.Info("watching", "dir", w.v.Dir(), "debounce", w.debounce)

	for {
		select {
		case <-ctx.Done():
			w.cancelTimer()
			return nil

		case ev, ok := <-fsw.Events:
			if !ok {
				return nil
			}
			w.handle(ev)

		case err, ok := <-fsw.Errors:
			if !ok {
				return nil
			}
			// Most likely an inotify queue overflow, which a large rsync or
			// checkout can trigger. Events were dropped, so the only safe
			// response is a full scan -- carrying on would be silent data loss.
			w.log.Error("watcher error; falling back to a full scan", "err", err)
			if _, serr := w.rc.Scan("recovery scan after watcher error"); serr != nil {
				return serr
			}
		}
	}
}

// addTree watches dir and every syncable directory beneath it.
func (w *Watcher) addTree(dir string) error {
	return filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if p != dir && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		return w.fsw.Add(p)
	})
}

func (w *Watcher) handle(ev fsnotify.Event) {
	rel, err := filepath.Rel(w.v.Dir(), ev.Name)
	if err != nil {
		return
	}
	rel = filepath.ToSlash(rel)
	if rel == "." || vault.Skip(rel) {
		return
	}

	// A new directory needs its own watch, and then an immediate scan of it:
	// files can be created inside between the mkdir and the watch being
	// registered. Moving a populated tree in hits that race every time.
	if ev.Has(fsnotify.Create) {
		if fi, err := os.Stat(ev.Name); err == nil && fi.IsDir() {
			if err := w.addTree(ev.Name); err != nil {
				w.log.Error("failed to watch new directory", "dir", ev.Name, "err", err)
			}
			w.queue(rel)
			return
		}
	}

	// Echo suppression: if this is exactly what we just wrote, it is our own
	// change coming back and committing it again would loop.
	if ev.Has(fsnotify.Write) || ev.Has(fsnotify.Create) {
		if content, err := w.v.Read(rel); err == nil && w.rc.WasOurWrite(rel, content) {
			return
		}
	}

	w.queue(rel)
}

// queue records a path and (re)arms the debounce timer. Any further event
// pushes the commit out again, so a burst of writes becomes one commit.
func (w *Watcher) queue(rel string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.pending) == 0 {
		w.firstAt = time.Now()
	}
	w.pending[rel] = struct{}{}
	if w.timer != nil {
		w.timer.Stop()
	}
	// Re-arm for the debounce, but never push the commit further out than
	// maxDelay from the first pending change.
	wait := w.debounce
	if elapsed := time.Since(w.firstAt); elapsed+wait > w.maxDelay {
		wait = w.maxDelay - elapsed
		if wait < 0 {
			wait = 0
		}
	}
	w.timer = time.AfterFunc(wait, w.flush)
}

func (w *Watcher) cancelTimer() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
}

func (w *Watcher) flush() {
	w.mu.Lock()
	paths := make([]string, 0, len(w.pending))
	for p := range w.pending {
		paths = append(paths, p)
	}
	w.pending = map[string]struct{}{}
	w.firstAt = time.Time{}
	w.mu.Unlock()

	if len(paths) == 0 {
		return
	}
	msg := "local edit: " + summarise(paths)
	head, err := w.rc.Scan(msg)
	if err != nil {
		w.log.Error("commit failed", "err", err, "paths", len(paths))
		return
	}
	w.log.Info("committed local edits", "paths", len(paths), "head", short(head))
}

func summarise(paths []string) string {
	if len(paths) == 1 {
		return paths[0]
	}
	return fmt.Sprintf("%s and %d more", paths[0], len(paths)-1)
}

func short(h string) string {
	if len(h) > 8 {
		return h[:8]
	}
	return h
}
