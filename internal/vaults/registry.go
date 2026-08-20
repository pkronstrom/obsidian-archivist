package vaults

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/pkronstrom/obsidian-archivist/internal/guard"
	"github.com/pkronstrom/obsidian-archivist/internal/reconcile"
	"github.com/pkronstrom/obsidian-archivist/internal/repo"
	"github.com/pkronstrom/obsidian-archivist/internal/vault"
	"github.com/pkronstrom/obsidian-archivist/internal/watcher"
)

// ErrNotFound means no such vault. Distinguished from every other failure so
// the API can answer 404 rather than 500.
var ErrNotFound = errors.New("vaults: no such vault")

func IsNotFound(err error) bool { return errors.Is(err, ErrNotFound) }

// Instance is one vault's entire state.
//
// Each has its OWN guard, because the counters are per-vault by definition:
// a vault-wide byte budget shared across two vaults would let a runaway client
// in one throttle writes in the other.
type Instance struct {
	Name       string
	Vault      *vault.Vault
	Repo       *repo.Repo
	Reconciler *reconcile.Reconciler
	Guard      *guard.Guard

	watcher *watcher.Watcher
}

// Options are everything a vault needs that does not come from the layout.
type Options struct {
	MaxVaults int
	Limits    guard.Limits
	Debounce  time.Duration
	// ThrottleMaxDebounce caps how far the local path may defer commits.
	ThrottleMaxDebounce time.Duration
	NormalizeNFC        bool
	// OnTrip is the guard's notification callback, given the vault name so an
	// alert says WHICH vault refused a write.
	OnTrip func(vaultName, code, path, reason string)
	Log    *slog.Logger
	Now    func() time.Time
}

// rescanTTL bounds how often a listing hits the filesystem.
//
// A readdir is nearly free, so this is a rate limit against a caller polling
// GET /v1/vaults in a loop, NOT a correctness boundary: a vault appearing a few
// seconds late is invisible, and never appearing at all is the failure worth
// avoiding.
const rescanTTL = 5 * time.Second

type Registry struct {
	layout Layout
	opts   Options

	mu       sync.Mutex
	open     map[string]*Instance
	cached   []string
	cachedAt time.Time

	// watchCtx is set by StartWatchers, so a vault created or discovered later
	// gets a watcher on the same lifetime as the ones started at boot.
	watchCtx context.Context
	wg       sync.WaitGroup
	// watchFail carries the first unexpected watcher exit to the server.
	// Buffered by one: the server shuts down on the first, so later ones have
	// nowhere to go and must not block the goroutine reporting them.
	watchFail chan error
}

// NewRegistry checks the layout, discovers what is there and opens all of it.
//
// Opening eagerly rather than on first request means a broken repository is a
// startup failure, at the moment someone is watching, rather than a 500 on the
// first sync from a phone.
func NewRegistry(l Layout, opts Options) (*Registry, error) {
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if err := l.CheckRoot(); err != nil {
		return nil, err
	}
	if err := l.EnsureDirs(); err != nil {
		return nil, err
	}

	r := &Registry{
		layout: l, opts: opts, open: map[string]*Instance{},
		watchFail: make(chan error, 1),
	}

	names, err := l.Discover()
	if err != nil {
		return nil, err
	}
	// Past the limit, WARN and serve. Refusing would make a mis-set limit an
	// outage, and the number is a guard against mistakes rather than against an
	// adversary -- so raising it must always be a way out, not the only way in.
	if opts.MaxVaults > 0 && len(names) > opts.MaxVaults {
		opts.Log.Warn("more vaults on disk than ARCHIVIST_MAX_VAULTS allows; serving them all",
			"found", len(names), "limit", opts.MaxVaults,
			"note", "creation is refused past the limit; raise ARCHIVIST_MAX_VAULTS to create more")
	}
	for _, n := range names {
		if _, err := r.openLocked(n); err != nil {
			return nil, err
		}
	}
	r.cached, r.cachedAt = names, opts.Now()
	return r, nil
}

// Names lists every vault, rescanning behind the cache.
func (r *Registry) Names() ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cached != nil && r.opts.Now().Sub(r.cachedAt) < rescanTTL {
		return append([]string(nil), r.cached...), nil
	}
	names, err := r.layout.Discover()
	if err != nil {
		return nil, err
	}
	r.cached, r.cachedAt = names, r.opts.Now()
	return append([]string(nil), names...), nil
}

// InvalidateCache forces the next Names to hit the filesystem. Used after a
// creation, and by tests.
func (r *Registry) InvalidateCache() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cachedAt = time.Time{}
}

// Get returns a vault, opening it if it appeared since startup.
func (r *Registry) Get(name string) (*Instance, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if inst, ok := r.open[name]; ok {
		return inst, nil
	}
	if err := ValidName(name); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, err)
	}
	if fi, err := os.Stat(r.layout.VaultDir(name)); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	return r.openLocked(name)
}

// Create makes a new vault. Callers must have checked the capability first.
func (r *Registry) Create(name string) error {
	if err := ValidName(name); err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	existing, err := r.layout.Discover()
	if err != nil {
		return err
	}
	if clash := Collides(name, existing); clash != "" {
		return fmt.Errorf(
			"vault %q collides with the existing %q: after Unicode NFC normalisation they are "+
				"the same name, and two directories that look identical would serve different "+
				"content", name, clash)
	}
	if r.opts.MaxVaults > 0 && len(existing) >= r.opts.MaxVaults {
		return fmt.Errorf(
			"refusing to create %q: %d vaults already exist and ARCHIVIST_MAX_VAULTS is %d",
			name, len(existing), r.opts.MaxVaults)
	}

	if err := os.MkdirAll(r.layout.VaultDir(name), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(r.layout.GitDir(name), 0o755); err != nil {
		return err
	}
	r.cachedAt = time.Time{}

	inst, err := r.openLocked(name)
	if err != nil {
		return err
	}
	r.startWatcherLocked(inst)
	r.opts.Log.Info("vault created", "vault", name, "dir", r.layout.VaultDir(name))
	return nil
}

// openLocked builds one vault's state. Callers must hold r.mu.
func (r *Registry) openLocked(name string) (*Instance, error) {
	vaultDir, gitDir := r.layout.VaultDir(name), r.layout.GitDir(name)

	v, err := vault.New(vaultDir)
	if err != nil {
		return nil, fmt.Errorf("vaults: opening %s: %w", vaultDir, err)
	}
	rp, err := repo.Open(vaultDir, gitDir)
	if err != nil {
		v.Close()
		return nil, err
	}
	// The same exclusion policy the single-vault server installed, per vault.
	rp.SetSyncable(func(p string) bool { return !vault.Skip(p) })

	if pm, err := repo.ReadPruneMap(gitDir); err != nil {
		v.Close()
		return nil, fmt.Errorf("vaults: reading the prune map for %s: %w", name, err)
	} else if len(pm) > 0 {
		rp.SetPruneMap(pm)
		r.opts.Log.Info("loaded head translations from past prunes", "vault", name, "entries", len(pm))
	}

	rc := reconcile.New(v, rp)
	rc.SetNormalizeNFC(r.opts.NormalizeNFC)

	// One guard per vault. Both write paths consult it, exactly as before:
	// Push refuses, Scan records and lets the watcher widen its debounce.
	g := guard.New(r.opts.Limits, r.opts.Now, guard.FreeOn(vaultDir))
	rc.SetGuard(g)
	if r.opts.OnTrip != nil {
		rc.SetTripHandler(func(code, path, reason string) {
			r.opts.OnTrip(name, code, path, reason)
		})
	}

	inst := &Instance{Name: name, Vault: v, Repo: rp, Reconciler: rc, Guard: g}
	r.open[name] = inst

	// A vault opened after StartWatchers -- discovered at runtime or just
	// created -- needs its watcher too, or it has remote sync and silently no
	// local sync. That asymmetry is the shape of a bug this codebase has
	// already shipped once.
	if r.watchCtx != nil {
		r.startWatcherLocked(inst)
	}
	return inst, nil
}

// StartWatchers runs one filesystem watcher per vault, and arranges for vaults
// opened later to get one too.
func (r *Registry) StartWatchers(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.watchCtx = ctx
	for _, inst := range r.open {
		r.startWatcherLocked(inst)
	}
	return nil
}

// startWatcherLocked is idempotent. Callers must hold r.mu.
func (r *Registry) startWatcherLocked(inst *Instance) {
	if inst.watcher != nil || r.watchCtx == nil {
		return
	}
	w := watcher.New(inst.Vault, inst.Reconciler, r.opts.Debounce, r.opts.Log.With("vault", inst.Name))
	w.SetPressure(inst.Guard.Pressure, r.opts.ThrottleMaxDebounce)
	inst.watcher = w

	ctx := r.watchCtx
	name := inst.Name
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		err := w.Run(ctx)
		if err == nil || ctx.Err() != nil {
			return // ordinary shutdown
		}
		// A watcher that dies is NOT a log line. The single-vault server treated
		// it as fatal, and it has to stay fatal: otherwise the HTTP server keeps
		// serving Push while Scan is permanently dead for that vault, and local
		// edits stop reaching history with nothing to show for it. That is the
		// silent second-write-path failure this whole plan is arranged around.
		r.opts.Log.Error("watcher stopped unexpectedly", "vault", name, "err", err)
		select {
		case r.watchFail <- fmt.Errorf("vaults: watcher for %q stopped: %w", name, err):
		default: // a failure is already reported; one is enough to bring the server down
		}
	}()
}

// WatchFailures reports watchers that died on their own. The server selects on
// it and shuts down, exactly as it did when one watcher goroutine was the only
// one there was.
func (r *Registry) WatchFailures() <-chan error { return r.watchFail }

// Wait blocks until every watcher has stopped. Call after cancelling the
// context passed to StartWatchers.
func (r *Registry) Wait() { r.wg.Wait() }

// ScanAll commits whatever is on disk in every vault. Used at startup when
// watching is disabled, and once more at shutdown.
func (r *Registry) ScanAll(msg string) {
	r.mu.Lock()
	instances := make([]*Instance, 0, len(r.open))
	for _, inst := range r.open {
		instances = append(instances, inst)
	}
	r.mu.Unlock()

	for _, inst := range instances {
		if _, err := inst.Reconciler.Scan(msg); err != nil {
			r.opts.Log.Error("scan failed", "vault", inst.Name, "err", err)
		}
	}
}

func (r *Registry) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, inst := range r.open {
		inst.Vault.Close()
	}
	r.open = map[string]*Instance{}
	return nil
}

// StepUpMarker is the file whose presence makes a vault protected. It holds no
// secret: presence is the whole content. It lives in the vault's state directory
// rather than the vault, so it is never synced to a device and it travels with
// the vault when the vault moves.
const StepUpMarker = "step-up"

// Protected reports whether this vault requires step-up authentication.
//
// It returns an error rather than a bare bool because the two failure directions
// are not symmetric. Only fs.ErrNotExist means "not protected". A permission
// error, an I/O error or a broken symlink means "cannot tell", and a caller that
// reads "cannot tell" as "not protected" serves a protected vault with no gate.
// The caller must turn an error into a refusal.
//
// One stat per call, uncached, deliberately. The tokens file justifies a watcher
// because a stale read there fails CLOSED. A stale read here fails OPEN, and a
// syscall is the cheaper mistake.
//
// Lstat, not Stat: a dangling symlink at this path is still somebody having put
// something there, and resolving it would report absence.
func (r *Registry) Protected(name string) (bool, error) {
	_, err := os.Lstat(filepath.Join(r.layout.GitDir(name), StepUpMarker))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("vaults: cannot tell whether %s is protected: %w", name, err)
	}
}
