// Package config resolves settings from flags, then the environment, then
// built-in defaults.
//
// Flags win over the environment: the environment is the deployment's baseline
// (set once in a compose file), so a flag on the command line is a deliberate
// override of it.
package config

import (
	"errors"
	"flag"
	"io"
	"os"
	"strconv"
	"time"
)

type Config struct {
	// Vault is the working tree -- the ordinary directory of notes.
	Vault string
	// Git is the git directory, deliberately OUTSIDE the vault so that no
	// client and no other tool ever sees a .git inside the notes.
	Git string
	// Listen is the HTTP bind address.
	Listen string
	// Token is the bearer token every client must present.
	Token string
	// Debounce is how long a path must be quiet before a filesystem change is
	// acted on.
	Debounce time.Duration
	// Watch enables the filesystem watcher. Off is useful in tests and for a
	// read-only replica.
	Watch bool
	// NormalizeNFC renames files on disk whose names are not composed.
	//
	// Off by default because it MODIFIES the vault. macOS writes filenames
	// decomposed and iOS writes them composed, and on Linux those are two
	// different paths -- so the same note arrives twice, or devices rename it
	// back and forth. Turn it on once and one spelling wins.
	NormalizeNFC bool
	// LogLevel is debug, info, warn or error.
	LogLevel string
	// LogFile, when set, also writes to a rotating file. Leave empty in a
	// container: Docker and journald already rotate.
	LogFile     string
	LogMaxBytes int64
	LogKeep     int
}

func envInt(key string, def int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Load parses args over environment defaults.
func Load(args []string) (*Config, error) {
	debounce, err := time.ParseDuration(env("ARCHIVIST_DEBOUNCE", "1s"))
	if err != nil {
		return nil, errors.New("ARCHIVIST_DEBOUNCE: " + err.Error())
	}

	fs := flag.NewFlagSet("archivist-server", flag.ContinueOnError)
	// The caller reports errors; the flag package writing to stderr on its own
	// makes test output unreadable.
	fs.SetOutput(io.Discard)

	c := &Config{}
	fs.StringVar(&c.Vault, "vault", env("ARCHIVIST_VAULT", ""),
		"vault directory (the notes themselves)")
	fs.StringVar(&c.Git, "git", env("ARCHIVIST_GIT", "/var/lib/archivist/git"),
		"git directory, kept outside the vault")
	fs.StringVar(&c.Listen, "listen", env("ARCHIVIST_LISTEN", ":8090"),
		"HTTP listen address")
	fs.StringVar(&c.Token, "token", env("ARCHIVIST_TOKEN", ""),
		"bearer token required from clients")
	fs.DurationVar(&c.Debounce, "debounce", debounce,
		"quiet period before a filesystem change is acted on")
	fs.BoolVar(&c.NormalizeNFC, "normalize-nfc", env("ARCHIVIST_NORMALIZE_NFC", "false") == "true",
		"rename files whose names are not Unicode NFC, so macOS and iOS agree on one spelling")
	fs.BoolVar(&c.Watch, "watch", env("ARCHIVIST_WATCH", "true") != "false",
		"watch the vault for local edits")
	fs.StringVar(&c.LogLevel, "log-level", env("ARCHIVIST_LOG_LEVEL", "info"),
		"debug, info, warn or error")
	fs.StringVar(&c.LogFile, "log-file", env("ARCHIVIST_LOG_FILE", ""),
		"also write logs to this rotating file (unnecessary under Docker or systemd)")
	fs.Int64Var(&c.LogMaxBytes, "log-max-bytes", envInt("ARCHIVIST_LOG_MAX_BYTES", 10<<20),
		"rotate the log file at this size")
	fs.IntVar(&c.LogKeep, "log-keep", int(envInt("ARCHIVIST_LOG_KEEP", 5)),
		"how many rotated log files to keep")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if c.Vault == "" {
		return nil, errors.New("vault directory is required (-vault or ARCHIVIST_VAULT)")
	}
	if c.Token == "" {
		return nil, errors.New("bearer token is required (-token or ARCHIVIST_TOKEN)")
	}
	return c, nil
}
