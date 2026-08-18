// Command archivist-relay is the sidecar: it lets things that are NOT on the
// server use the vault.
//
// It holds no vault and no sync state. Every operation is a straight call to
// the server, which is what makes it safe to restart at any moment.
//
// If your tool runs on the SAME machine as the server, do not use this: the
// vault is an ordinary directory, and opening a file beats calling an API. The
// relay may still run there pointed at localhost, so one instance serves
// everything on the network.
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
	"strings"
	"syscall"
	"time"

	// Embeds the timezone database, for the same reason the server does it.
	//
	// This image is alpine rather than scratch, but alpine ships no tzdata
	// either, so TZ=Europe/Helsinki was set and silently ignored: the relay
	// logged in UTC while the server it fronts logged local, three hours apart.
	// Noticed while checking a note_history timestamp that looked wrong and was
	// not -- reading the two logs side by side is the case that would have
	// wasted real time later.
	_ "time/tzdata"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pkronstrom/obsidian-archivist/internal/client"
	"github.com/pkronstrom/obsidian-archivist/internal/logging"
	"github.com/pkronstrom/obsidian-archivist/internal/relay"
	"github.com/pkronstrom/obsidian-archivist/internal/version"
)

type config struct {
	url       string
	token     string
	listen    string
	relayTok  string
	device    string
	vault     string
	webhooks  []string
	enableMCP bool
	logLevel  string
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func load(args []string) (*config, error) {
	fs := flag.NewFlagSet("archivist-relay", flag.ContinueOnError)
	c := &config{}
	var hooks string

	fs.StringVar(&c.url, "url", env("ARCHIVIST_URL", ""),
		"base URL of the Archivist server")
	fs.StringVar(&c.token, "token", env("ARCHIVIST_TOKEN", ""),
		"bearer token for the server")
	fs.StringVar(&c.listen, "listen", env("ARCHIVIST_RELAY_LISTEN", ":8091"),
		"HTTP listen address for the relay's own API and MCP")
	fs.StringVar(&c.relayTok, "relay-token", env("ARCHIVIST_RELAY_TOKEN", ""),
		"bearer token callers must present to the relay (defaults to -token)")
	fs.StringVar(&c.vault, "vault", env("ARCHIVIST_VAULT", ""),
		"vault this relay addresses by default; tools may override it per call")
	fs.StringVar(&c.device, "device", env("ARCHIVIST_DEVICE", "relay"),
		"names this relay in commit messages and conflict filenames")
	fs.StringVar(&hooks, "webhook", env("ARCHIVIST_WEBHOOKS", ""),
		"comma-separated URLs to POST change events to (best-effort, never retried)")
	fs.BoolVar(&c.enableMCP, "mcp", env("ARCHIVIST_MCP", "true") != "false",
		"serve MCP over HTTP at /mcp")
	fs.StringVar(&c.logLevel, "log-level", env("ARCHIVIST_LOG_LEVEL", "info"),
		"debug, info, warn or error")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if c.url == "" {
		return nil, errors.New("server URL is required (-url or ARCHIVIST_URL)")
	}
	if c.token == "" {
		return nil, errors.New("server token is required (-token or ARCHIVIST_TOKEN)")
	}
	// Defaulting the relay token to the server's is a convenience for a
	// single-user setup, not a recommendation: if the relay is reachable by
	// anything you would not hand a vault credential, set it separately.
	if c.relayTok == "" {
		c.relayTok = c.token
	}
	for _, h := range strings.Split(hooks, ",") {
		if h = strings.TrimSpace(h); h != "" {
			c.webhooks = append(c.webhooks, h)
		}
	}
	return c, nil
}

func main() {
	cfg, err := load(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "archivist-relay:", err)
		os.Exit(1)
	}
	level, err := logging.Level(cfg.logLevel)
	if err != nil {
		fmt.Fprintln(os.Stderr, "archivist-relay:", err)
		os.Exit(1)
	}
	log, closer, err := logging.New(level, "", 0, 0)
	if err != nil {
		fmt.Fprintln(os.Stderr, "archivist-relay:", err)
		os.Exit(1)
	}
	defer closer.Close()

	if err := run(cfg, log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(cfg *config, log *slog.Logger) error {
	c := client.New(cfg.url, cfg.token, cfg.device).WithVault(cfg.vault)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Refuse a server speaking a protocol we do not, rather than failing oddly
	// later. Only `protocol` is compared -- the build version moves every
	// release and would make every upgrade look like a break.
	//
	// A server that is merely DOWN is not fatal: the relay stays up, /healthz
	// reports degraded, and calls fail until it returns.
	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := c.CheckCompatible(checkCtx); err != nil {
		if strings.Contains(err.Error(), "protocol") {
			return err
		}
		log.Warn("cannot reach the server yet; starting anyway", "url", cfg.url, "err", err)
	} else {
		log.Info("server is compatible", "url", cfg.url, "protocol", version.Version)
	}

	// Re-check periodically. The startup check alone leaves two gaps: the server
	// may have been down just now (explicitly supported), and it may be
	// redeployed under a running relay. Writes are refused while a mismatch
	// stands, and resume by themselves once it is resolved.
	go func() {
		t := time.NewTicker(60 * time.Second)
		defer t.Stop()
		var lastBad bool
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
				err := c.CheckCompatible(cctx)
				cancel()
				switch {
				case err != nil && strings.Contains(err.Error(), "protocol"):
					if !lastBad {
						log.Error("server protocol no longer matches; writes are refused", "err", err)
						lastBad = true
					}
				case err == nil && lastBad:
					log.Info("server protocol matches again; writes resumed")
					lastBad = false
				}
			}
		}
	}()

	var mcpHandler http.Handler
	if cfg.enableMCP {
		srv := relay.NewMCPServer(c, "archivist", version.Version)
		mcpHandler = mcp.NewStreamableHTTPHandler(
			func(*http.Request) *mcp.Server { return srv }, nil)
	}

	var hooks *relay.Webhooks
	if len(cfg.webhooks) > 0 {
		hooks = relay.NewWebhooks(c, cfg.webhooks, log)
		go func() {
			if err := hooks.Run(ctx); err != nil {
				log.Error("webhook fan-out stopped", "err", err)
			}
		}()
	}

	httpSrv := &http.Server{
		Addr:              cfg.listen,
		Handler:           relay.NewHandler(c, cfg.relayTok, log, mcpHandler, hooks),
		ReadHeaderTimeout: 10 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()
	log.Info("archivist-relay listening",
		"addr", cfg.listen, "mcp", cfg.enableMCP, "webhooks", len(cfg.webhooks),
		"separateRelayToken", cfg.relayTok != cfg.token)

	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-serveErr:
		return err
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShutdown()
	return httpSrv.Shutdown(shutdownCtx)
}
