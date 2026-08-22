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

	// pressure reports whether a guard threshold is currently tripped. Nil
	// means no guard, so no deferral.
	pressure func() bool
	// maxDebounce caps how far pressure may widen the debounce.
	maxDebounce time.Duration
}

// SetPressure installs the guard's pressure probe and the deferral ceiling.
// Passing a nil probe disables deferral.
func (w *Watcher) SetPressure(fn func() bool, max time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pressure, w.maxDebounce = fn, max
}

// effectiveDebounce is the debounce to use right now.
//
// Under pressure it widens toward maxDebounce. That is the local path's only
// useful response to a loop: it cannot refuse a write, because the bytes are
// already on disk, but committing once a minute instead of once a second
// means only the file's final state in each window becomes a blob and the
// intermediate revisions are never stored at all.
//
// Callers must hold w.mu.
func (w *Watcher) effectiveDebounce() time.Duration {
	if w.pressure == nil || !w.pressure() {
		return w.debounce
	}
	if w.maxDebounce > w.debounce {
		return w.maxDebounce
	}
	return w.debounce
}

// effectiveMaxDelay keeps the ceiling proportional to the debounce in force.
//
// The ceiling is never removed, only scaled. Without one, a steady write
// stream produces no commits at all -- see the measurement in New. Widening
// the debounce without widening this would instead make maxDelay fire on
// every event, which defeats the deferral.
//
// Callers must hold w.mu.
func (w *Watcher) effectiveMaxDelay() time.Duration {
	d := w.effectiveDebounce()
	if d == w.debounce {
		return w.maxDelay
	}
	max := 10 * d
	if max < w.maxDelay {
		return w.maxDelay
	}
	return max
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
//
// Dot-directories are skipped, with one exception: the Obsidian configuration
// directory, which holds allowlisted files that DO sync. Without descending
// into it, a snippet edited on the server host produces no event, so no Scan,
// so no commit -- the local write path would carry notes and silently not carry
// config. vault.Skip is the filter for individual events; this is the separate
// decision about which directories are worth an inotify watch at all, and it
// has to be kept in step with it by hand.
func (w *Watcher) addTree(dir string) error {
	return filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if p != dir && strings.HasPrefix(d.Name(), ".") && !w.watchableConfigDir(p) {
			return filepath.SkipDir
		}
		return w.fsw.Add(p)
	})
}

// watchableConfigRel reports whether a vault-relative path is the config
// directory or lives inside it.
//
// Watching the whole subtree costs a handful of inotify descriptors, and the
// ordinary Skip filter still decides file by file what may be committed -- so
// this buys the guarantee that no allowlisted file is invisible, at no risk of
// committing one that is not.
func (w *Watcher) watchableConfigRel(rel string) bool {
	return rel == vault.ConfigDir || strings.HasPrefix(rel, vault.ConfigDir+"/")
}

// watchableConfigDir is watchableConfigRel for an absolute path.
func (w *Watcher) watchableConfigDir(abs string) bool {
	rel, err := filepath.Rel(w.v.Dir(), abs)
	if err != nil {
		return false
	}
	return w.watchableConfigRel(filepath.ToSlash(rel))
}

func (w *Watcher) handle(ev fsnotify.Event) {
	rel, err := filepath.Rel(w.v.Dir(), ev.Name)
	if err != nil {
		return
	}
	rel = filepath.ToSlash(rel)
	if rel == "." {
		return
	}
	// vault.Skip is a FILE policy: it says .obsidian is excluded, because the
	// allowlist names files inside it rather than the directory itself. A
	// directory event has to be judged separately, or a .obsidian/ created
	// after startup is dropped here and never reaches addTree -- which is what
	// happens the first time a remote push creates it, leaving every later
	// host-local config edit invisible.
	//
	// Letting a refused config path through costs at most one no-op commit
	// attempt: Repo.Commit consults syncable and will not stage it.
	// A new directory needs its own watch, and then an immediate scan of it:
	// files can be created inside between the mkdir and the watch being
	// registered. Moving a populated tree in hits that race every time.
	//
	// Judged BEFORE vault.Skip, because Skip carries file rules that a
	// directory must not inherit. A folder called "project.local.assets"
	// satisfies the .local grammar, and refusing to watch it would silently
	// stop every note inside it from syncing -- while the startup walk, which
	// only skips dot-directories, descends into it happily. The two must not
	// disagree.
	if ev.Has(fsnotify.Create) {
		if fi, err := os.Stat(ev.Name); err == nil && fi.IsDir() {
			if err := w.addTree(ev.Name); err != nil {
				w.log.Error("failed to watch new directory", "dir", ev.Name, "err", err)
			}
			w.queue(rel)
			return
		}
	}

	if vault.Skip(rel) && !w.watchableConfigRel(rel) {
		return
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
	wait := w.effectiveDebounce()
	maxDelay := w.effectiveMaxDelay()
	if elapsed := time.Since(w.firstAt); elapsed+wait > maxDelay {
		wait = maxDelay - elapsed
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
