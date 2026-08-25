// Command archivist-server serves every vault under one root.
//
// One process, one registry, N vaults. Each vault gets its own working tree,
// repository, reconciler, guard and filesystem watcher, so nothing is shared
// but the process and the listening socket -- which is what makes a leaked
// token's blast radius exactly the vaults that token names.
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

	// Embeds the timezone database in the binary.
	//
	// The container image is FROM scratch, so there is no /usr/share/zoneinfo
	// for Go to read. Without this, TZ=Europe/Helsinki has NO effect: the lookup
	// fails, the process falls back to UTC, and every commit in the vault
	// history is stamped +0000 while the compose file claims Helsinki. That
	// shipped, and was caught only by noticing timestamps three hours behind
	// local.
	//
	// Costs 403 KB measured, which buys git timestamps that match the wall clock
	// of whoever reads them.
	_ "time/tzdata"

	"github.com/pkronstrom/obsidian-archivist/internal/api"
	"github.com/pkronstrom/obsidian-archivist/internal/auth"
	"github.com/pkronstrom/obsidian-archivist/internal/cli"
	"github.com/pkronstrom/obsidian-archivist/internal/config"
	"github.com/pkronstrom/obsidian-archivist/internal/guard"
	"github.com/pkronstrom/obsidian-archivist/internal/logging"
	"github.com/pkronstrom/obsidian-archivist/internal/notify"
	"github.com/pkronstrom/obsidian-archivist/internal/stepup"
	"github.com/pkronstrom/obsidian-archivist/internal/tokencli"
	"github.com/pkronstrom/obsidian-archivist/internal/vaults"
)

func main() {
	// Token administration is offline by design: it needs shell access to this
	// host, never a network route. Dispatched before config.Load because it
	// takes its own flags and needs no vault root.
	if len(os.Args) > 1 && tokencli.Handles(os.Args[1]) {
		if err := tokencli.Run(os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "archivist:", err)
			os.Exit(1)
		}
		return
	}

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
	root := fs.String("root", envOr("ARCHIVIST_ROOT", ""),
		"root holding vaults/ and .archivist/; with -name this derives -vault and -git")
	vaultName := fs.String("name", "", "vault name under -root")
	vaultDir := fs.String("vault", envOr("ARCHIVIST_VAULT", ""), "vault directory")
	gitDir := fs.String("git", envOr("ARCHIVIST_GIT", ""), "git directory")
	// Only history and check produce structured output; show, restore and
	// export do not, and silently accepting -json there implied otherwise.
	asJSON := fs.Bool("json", false, "machine-readable output (history, check and reclaim only)")
	// reclaim's flags go on THIS flagset. Go's flag package stops at the first
	// unrecognised flag, so a second flagset inside the subcommand would never
	// receive them.
	var reclaimFlags *cli.ReclaimFlags
	if name == "reclaim" {
		reclaimFlags = cli.RegisterReclaimFlags(fs)
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	// -root plus -name is the ergonomic form; explicit -vault/-git still works
	// and wins, because a repository moved out of the layout must stay
	// inspectable.
	if *vaultDir == "" && *root != "" && *vaultName != "" {
		l := vaults.Layout{Root: *root}
		*vaultDir, *gitDir = l.VaultDir(*vaultName), l.GitDir(*vaultName)
	}
	if *vaultDir == "" {
		return errors.New("vault is required: -root with -name, or -vault with -git")
	}
	if *gitDir == "" {
		return errors.New("git directory is required (-git), or use -root with -name")
	}
	if *asJSON && name != "history" && name != "check" && name != "reclaim" {
		return fmt.Errorf("-json is not supported by %s (history, check and reclaim only)", name)
	}
	return cli.Run(name, fs.Args(), cli.Env{
		Vault: *vaultDir, Git: *gitDir, JSON: *asJSON, Out: os.Stdout,
		Reclaim: reclaimFlags,
	})
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func run(cfg *config.Config, log *slog.Logger) error {
	tokens, err := auth.Load(cfg.TokensFile)
	if err != nil {
		return err
	}

	n := notify.New(cfg.NtfyURL, time.Now, log)

	layout := vaults.Layout{Root: cfg.Root}
	reg, err := vaults.NewRegistry(layout, vaults.Options{
		MaxVaults: cfg.MaxVaults,
		Limits: guard.Limits{
			Writes:       cfg.QuarantineWrites,
			PathBytes:    cfg.QuarantinePathBytes,
			TotalBytes:   cfg.QuarantineTotalBytes,
			Window:       cfg.QuarantineWindow,
			Cooldown:     cfg.QuarantineCooldown,
			MinFreeBytes: cfg.MinFreeBytes,
		},
		Debounce:            cfg.Debounce,
		ThrottleMaxDebounce: cfg.ThrottleMaxDebounce,
		NormalizeNFC:        cfg.NormalizeNFC,
		OnTrip: func(vaultName, code, path, reason string) {
			log.Warn("guard refused a write", "vault", vaultName, "code", code,
				"path", path, "reason", reason)
			// Keyed by vault too, so a trip in one vault does not suppress the
			// alert for the same path in another.
			n.SendKeyed(vaultName+":"+code+":"+path,
				"archivist: "+code+" in "+vaultName, path+" -- "+reason, true)
		},
		Log: log,
		Now: time.Now,
	})
	if err != nil {
		return err
	}
	defer reg.Close()

	names, err := reg.Names()
	if err != nil {
		return err
	}
	log.Info("archivist-server starting",
		"root", cfg.Root, "listen", cfg.Listen, "vaults", layout.Describe(names),
		"level", cfg.LogLevel)

	if cfg.NtfyURL == "" {
		log.Info("ntfy alerts are disabled; set ARCHIVIST_NTFY_URL to enable them")
	} else if err := n.Verify("archivist: started",
		fmt.Sprintf("guards armed on %s", layout.Describe(names))); err != nil {
		// Loud, but NOT fatal. A broken notifier is not a reason to stop
		// serving the vaults -- it is a reason to know before the guards need it.
		log.Error("ntfy startup check FAILED; guard alerts will not reach you",
			"url", cfg.NtfyURL, "err", err)
	} else {
		log.Info("ntfy startup check delivered", "url", cfg.NtfyURL)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Re-read the tokens file when it changes, so `token add` is not an outage.
	// Watching the directory rather than the file is deliberate; see the method.
	//
	// After defer stop(), not before: an error here returns from run(), and the
	// signal handler registered a line earlier would otherwise never be undone.
	if err := tokens.Watch(ctx, cfg.TokensFile, log); err != nil {
		return fmt.Errorf("watching the tokens file: %w", err)
	}

	if cfg.Watch {
		// One watcher per vault. A registry that opened repositories but
		// started fewer watchers would give a vault working remote sync and
		// silently no local sync.
		if err := reg.StartWatchers(ctx); err != nil {
			return err
		}
	} else {
		reg.ScanAll("startup scan")
		log.Warn("filesystem watching is disabled; local edits will not be committed")
	}

	// Step-up state, both in memory and both dying with the process. A deploy
	// therefore revokes every grant, which is the conservative direction: you did
	// not consent to the new binary.
	grants := stepup.NewGrants(cfg.StepUpTTL, nil)
	defer grants.Close()
	verifier := stepup.NewVerifier(nil)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           api.New(reg, tokens, api.WithStepUp(grants, verifier)),
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
	case err := <-reg.WatchFailures():
		// Fatal, as it was with one vault. A server that keeps answering pushes
		// while a vault's local edits silently stop being committed is worse
		// than one that stops and says why.
		return err
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	// Watchers stop with ctx; wait for them before the final scan so nothing
	// commits underneath it.
	reg.Wait()
	reg.ScanAll("shutdown scan")
	return nil
}
