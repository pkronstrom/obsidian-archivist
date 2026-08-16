// Command archivist-server serves one vault.
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

	"github.com/pkronstrom/obsidian-archivist/internal/api"
	"github.com/pkronstrom/obsidian-archivist/internal/cli"
	"github.com/pkronstrom/obsidian-archivist/internal/config"
	"github.com/pkronstrom/obsidian-archivist/internal/logging"
	"github.com/pkronstrom/obsidian-archivist/internal/reconcile"
	"github.com/pkronstrom/obsidian-archivist/internal/repo"
	"github.com/pkronstrom/obsidian-archivist/internal/vault"
	"github.com/pkronstrom/obsidian-archivist/internal/watcher"
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
			fmt.Fprintln(os.Stderr, "archivist:", err)
			os.Exit(1)
		}
		return
	}

	cfg, err := config.Load(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "archivist:", err)
		os.Exit(1)
	}
	level, err := logging.Level(cfg.LogLevel)
	if err != nil {
		fmt.Fprintln(os.Stderr, "archivist:", err)
		os.Exit(1)
	}
	log, closer, err := logging.New(level, cfg.LogFile, cfg.LogMaxBytes, cfg.LogKeep)
	if err != nil {
		fmt.Fprintln(os.Stderr, "archivist:", err)
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
	vaultDir := fs.String("vault", envOr("ARCHIVIST_VAULT", ""), "vault directory")
	gitDir := fs.String("git", envOr("ARCHIVIST_GIT", "/var/lib/archivist/git"), "git directory")
	// Only history and check produce structured output; show, restore and
	// export do not, and silently accepting -json there implied otherwise.
	asJSON := fs.Bool("json", false, "machine-readable output (history and check only)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *vaultDir == "" {
		return errors.New("vault directory is required (-vault or ARCHIVIST_VAULT)")
	}
	if *asJSON && name != "history" && name != "check" {
		return fmt.Errorf("-json is not supported by %s (history and check only)", name)
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
	// Dotfiles must not enter git. Enforced here, at the point files are
	// staged, rather than only where events are observed.
	r.SetSyncable(func(p string) bool { return !vault.Skip(p) })
	rc := reconcile.New(v, r)

	head, err := r.Head()
	if err != nil {
		return err
	}
	log.Info("archivist-server starting",
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
		// Deliberately does NOT signal watchDone. Sending nil here made the
		// select below treat "the watcher finished" as a shutdown signal, so
		// -watch=false exited immediately and the documented read-only mode was
		// unusable. A nil channel blocks forever, which is what we want.
		watchDone = nil
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
