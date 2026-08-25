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
	"strings"
	"time"
)

type Config struct {
	// Root holds every vault and every history:
	//
	//	$ROOT/vaults/<name>/       the vault
	//	$ROOT/.archivist/<name>/   its history
	//
	// This was a deployment convention -- compose already sets exactly these
	// paths against one /data mount -- and multi-vault makes it the server's
	// own, which is why the old per-vault -vault/-git defaults are gone.
	Root string
	// MaxVaults refuses creation past this many. Discovery finding more warns
	// and serves them, so raising the value is always a way out. It is a guard
	// against mistakes, not against an adversary.
	MaxVaults int
	// TokensFile maps tokens to the vaults they open. Empty falls back to
	// Token, which opens EVERY vault -- convenient for one vault, and not
	// isolation.
	TokensFile string
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
	// NormalizeNFC composes the paths of incoming changes before they are
	// applied.
	//
	// macOS writes filenames decomposed and iOS writes them composed, and on
	// Linux those are two different paths -- so the same note arrives twice, or
	// devices rename it back and forth. Canonicalising every push makes the
	// writers converge on one spelling.
	//
	// It does NOT rename anything already on disk. A vault that already carries
	// both spellings keeps both until a person picks a winner; turning this on
	// stops the divergence rather than repairing it.
	NormalizeNFC bool
	// LogLevel is debug, info, warn or error.
	LogLevel string
	// LogFile, when set, also writes to a rotating file. Leave empty in a
	// container: Docker and journald already rotate.
	LogFile     string
	LogMaxBytes int64
	LogKeep     int

	// Guard thresholds. Zero disables an individual counter; it never means
	// "block everything". See docs/2026-08-18-vault-size-guards-design.md.
	QuarantineWrites     int
	QuarantinePathBytes  int64
	QuarantineTotalBytes int64
	QuarantineWindow     time.Duration
	QuarantineCooldown   time.Duration
	// ThrottleMaxDebounce caps how far the local path may defer commits while
	// a threshold is tripped.
	ThrottleMaxDebounce time.Duration

	// StepUpTTL is how long one unlock lasts on a protected vault. Absolute
	// from the unlock, never extended by use.
	StepUpTTL time.Duration
	// MinFreeBytes refuses writes below this much free disk.
	MinFreeBytes int64
	// NtfyURL is a full ntfy URL including the topic. Empty disables alerts.
	// Archivist's own channel, deliberately not the backup notifier's: restic
	// posts on a schedule, so a vault alarm in that topic reads as routine.
	NtfyURL string
}

func envInt(key string, def int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

func envDuration(key, def string) (time.Duration, error) {
	d, err := time.ParseDuration(env(key, def))
	if err != nil {
		return 0, errors.New(key + ": " + err.Error())
	}
	return d, nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Load parses args over environment defaults.
func Load(args []string) (*Config, error) {
	// The old single-vault flags. Rejecting them by name beats "flag provided
	// but not defined", which says nothing about what to do instead. Prefix,
	// not equality: `-vault=/path` is a single argv entry.
	for _, a := range args {
		if strings.HasPrefix(a, "-vault") || strings.HasPrefix(a, "--vault") ||
			strings.HasPrefix(a, "-git") || strings.HasPrefix(a, "--git") {
			return nil, errors.New(
				"-vault and -git are gone: one process now serves every vault under " +
					"-root (ARCHIVIST_ROOT), with vaults at $ROOT/vaults/<name> and their " +
					"history at $ROOT/.archivist/<name>. The offline subcommands still " +
					"accept -vault and -git for a single repository")
		}
	}

	debounce, err := time.ParseDuration(env("ARCHIVIST_DEBOUNCE", "1s"))
	if err != nil {
		return nil, errors.New("ARCHIVIST_DEBOUNCE: " + err.Error())
	}

	qWindow, err := envDuration("ARCHIVIST_QUARANTINE_WINDOW", "5m")
	if err != nil {
		return nil, err
	}
	qCooldown, err := envDuration("ARCHIVIST_QUARANTINE_COOLDOWN", "15m")
	if err != nil {
		return nil, err
	}
	stepUpTTL, err := envDuration("ARCHIVIST_STEP_UP_TTL", "15m")
	if err != nil {
		return nil, err
	}
	maxDebounce, err := envDuration("ARCHIVIST_THROTTLE_MAX_DEBOUNCE", "60s")
	if err != nil {
		return nil, err
	}

	fs := flag.NewFlagSet("archivist-server", flag.ContinueOnError)
	// The caller reports errors; the flag package writing to stderr on its own
	// makes test output unreadable.
	fs.SetOutput(io.Discard)

	c := &Config{}
	fs.StringVar(&c.Root, "root", env("ARCHIVIST_ROOT", ""),
		"directory holding vaults/ and .archivist/")
	fs.IntVar(&c.MaxVaults, "max-vaults", int(envInt("ARCHIVIST_MAX_VAULTS", 5)),
		"refuse creating more vaults than this; discovery past it warns and serves them")
	fs.StringVar(&c.TokensFile, "tokens", env("ARCHIVIST_TOKENS", ""),
		"tokens file written by `archivist-server token add`; hashed, never hand-edited")
	fs.StringVar(&c.Listen, "listen", env("ARCHIVIST_LISTEN", ":8090"),
		"HTTP listen address")
	fs.StringVar(&c.Token, "token", env("ARCHIVIST_TOKEN", ""),
		"bearer token required from clients")
	fs.DurationVar(&c.Debounce, "debounce", debounce,
		"quiet period before a filesystem change is acted on")
	fs.BoolVar(&c.NormalizeNFC, "normalize-nfc", env("ARCHIVIST_NORMALIZE_NFC", "false") == "true",
		"compose incoming paths to Unicode NFC, so macOS and iOS agree on one spelling")
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

	fs.IntVar(&c.QuarantineWrites, "quarantine-writes",
		int(envInt("ARCHIVIST_QUARANTINE_WRITES", 300)),
		"writes to one path per window before it is quarantined (0 disables)")
	fs.Int64Var(&c.QuarantinePathBytes, "quarantine-path-bytes",
		envInt("ARCHIVIST_QUARANTINE_PATH_BYTES", 100<<20),
		"bytes to one path per window before it is quarantined (0 disables)")
	fs.Int64Var(&c.QuarantineTotalBytes, "quarantine-total-bytes",
		envInt("ARCHIVIST_QUARANTINE_TOTAL_BYTES", 2<<30),
		"bytes to the whole vault per window before throttling (0 disables)")
	fs.DurationVar(&c.QuarantineWindow, "quarantine-window", qWindow,
		"rolling window for the guard counters")
	fs.DurationVar(&c.QuarantineCooldown, "quarantine-cooldown", qCooldown,
		"how long a quarantine holds")
	fs.DurationVar(&c.StepUpTTL, "step-up-ttl", stepUpTTL,
		"how long one unlock lasts on a protected vault (ARCHIVIST_STEP_UP_TTL)")
	fs.DurationVar(&c.ThrottleMaxDebounce, "throttle-max-debounce", maxDebounce,
		"ceiling on the local path's deferred commit cadence")
	fs.Int64Var(&c.MinFreeBytes, "min-free-bytes",
		envInt("ARCHIVIST_MIN_FREE_BYTES", 20<<30),
		"refuse writes below this much free disk (0 disables)")
	fs.StringVar(&c.NtfyURL, "ntfy-url", env("ARCHIVIST_NTFY_URL", ""),
		"full ntfy URL including the topic; empty disables alerts")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if c.Root == "" {
		return nil, errors.New("root directory is required (-root or ARCHIVIST_ROOT)")
	}
	if c.Token == "" && c.TokensFile == "" {
		return nil, errors.New(
			"credentials are required: -tokens/ARCHIVIST_TOKENS for per-vault tokens, " +
				"or -token/ARCHIVIST_TOKEN for a single token that opens every vault")
	}
	return c, nil
}
