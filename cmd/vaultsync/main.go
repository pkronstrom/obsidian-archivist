// Command vaultsync serves one vault.
//
// One vault per process: two vaults means two containers, which is how they
// would be deployed anyway, and it keeps every path in the API unqualified.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/pkronstrom/vaultsync/internal/api"
	"github.com/pkronstrom/vaultsync/internal/cli"
	"github.com/pkronstrom/vaultsync/internal/config"
	"github.com/pkronstrom/vaultsync/internal/logging"
	"github.com/pkronstrom/vaultsync/internal/reconcile"
	"github.com/pkronstrom/vaultsync/internal/repo"
	"github.com/pkronstrom/vaultsync/internal/vault"
	"github.com/pkronstrom/vaultsync/internal/watcher"
)

func main() {
	// Subcommands are dispatched before anything else, so they work against a
	// stopped server -- which is exactly when `check` and `export` matter.
	if len(os.Args) > 1 && cli.Handles(os.Args[1]) {
		if err := runCommand(os.Args[1], os.Args[2:]); err != nil {
			if errors.Is(err, cli.ErrDrift) {
				// Already reported in full; exit non-zero for cron.
				os.Exit(2)
			}
			fmt.Fprintln(os.Stderr, "vaultsync:", err)
			os.Exit(1)
		}
		return
	}

	cfg, err := config.Load(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "vaultsync:", err)
		os.Exit(1)
	}
	level, err := logging.Level(cfg.LogLevel)
	if err != nil {
		fmt.Fprintln(os.Stderr, "vaultsync:", err)
		os.Exit(1)
	}
	log, closer, err := logging.New(level, cfg.LogFile, cfg.LogMaxBytes, cfg.LogKeep)
	if err != nil {
		fmt.Fprintln(os.Stderr, "vaultsync:", err)
		os.Exit(1)
	}
	defer closer.Close()

	if err := run(cfg, log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// runCommand handles the offline subcommands. They need only the paths, so
// they deliberately do not require a token.
func runCommand(name string, args []string) error {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	vaultDir := fs.String("vault", envOr("VAULTSYNC_VAULT", ""), "vault directory")
	gitDir := fs.String("git", envOr("VAULTSYNC_GIT", "/var/lib/vaultsync/git"), "git directory")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *vaultDir == "" {
		return errors.New("vault directory is required (-vault or VAULTSYNC_VAULT)")
	}
	return cli.Run(name, fs.Args(), cli.Env{
		Vault: *vaultDir, Git: *gitDir, JSON: *asJSON, Out: os.Stdout,
	})
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func run(cfg *config.Config, log *slog.Logger) error {
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
		"vault", cfg.Vault, "git", cfg.Git, "listen", cfg.Listen, "head", head,
		"level", cfg.LogLevel)
	log.Debug("configuration", "debounce", cfg.Debounce, "watch", cfg.Watch,
		"logFile", cfg.LogFile)

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
