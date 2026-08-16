// Command vaultsync serves one vault.
//
// One vault per process: two vaults means two containers, which is how they
// would be deployed anyway, and it keeps every path in the API unqualified.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/pkronstrom/vaultsync/internal/api"
	"github.com/pkronstrom/vaultsync/internal/config"
	"github.com/pkronstrom/vaultsync/internal/reconcile"
	"github.com/pkronstrom/vaultsync/internal/repo"
	"github.com/pkronstrom/vaultsync/internal/vault"
	"github.com/pkronstrom/vaultsync/internal/watcher"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load(os.Args[1:])
	if err != nil {
		return err
	}

	v, err := vault.New(cfg.Vault)
	if err != nil {
		return err
	}
	defer v.Close()

	r, err := repo.Open(cfg.Vault, cfg.Git)
	if err != nil {
		return err
	}
	rc := reconcile.New(v, r)

	head, err := r.Head()
	if err != nil {
		return err
	}
	log.Info("vaultsync starting",
		"vault", cfg.Vault, "git", cfg.Git, "listen", cfg.Listen, "head", head)

	// SIGINT/SIGTERM cancels the watcher and starts a graceful HTTP shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	watchDone := make(chan error, 1)
	if cfg.Watch {
		w := watcher.New(v, rc, cfg.Debounce, log)
		go func() { watchDone <- w.Run(ctx) }()
	} else {
		// Even without the watcher, reconcile once: whatever changed while the
		// process was stopped still has to reach history.
		if _, err := rc.Scan("startup scan"); err != nil {
			return err
		}
		log.Warn("filesystem watching is disabled; local edits will not be committed")
		watchDone <- nil
	}

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           api.New(rc, r, cfg.Token),
		ReadHeaderTimeout: 10 * time.Second,
	}

	serveDone := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveDone <- err
			return
		}
		serveDone <- nil
	}()
	log.Info("listening", "addr", cfg.Listen)

	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-serveDone:
		return err
	case err := <-watchDone:
		if err != nil {
			return err
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	// A final scan so anything written during shutdown still reaches history.
	if _, err := rc.Scan("shutdown scan"); err != nil {
		log.Error("final scan failed", "err", err)
	}
	return nil
}
