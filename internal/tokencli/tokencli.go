// Package tokencli is the `archivist-server token` subcommand: mint, list and
// revoke tokens in the tokens file.
//
// Deliberately offline. There is no HTTP route that mints, because the server
// is reachable from the internet behind Caddy and a mint endpoint would let any
// leaked admin token issue itself a successor that survives revocation of the
// original. Minting requires shell access to the host, which is a far higher
// bar and does not degrade when a token leaks.
//
// The server picks up what this writes without a restart: Save renames the file
// into place and the server watches the directory. See auth.Set.Watch.
package tokencli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/pkronstrom/obsidian-archivist/internal/auth"
	"github.com/pkronstrom/obsidian-archivist/internal/stepup"
	"github.com/pkronstrom/obsidian-archivist/internal/vaults"
)

// posture is what a profile does about step-up on a protected vault.
type posture int

const (
	postureNone posture = iota // record an exemption
	postureOps                 // ops:<vault> -- confirm destructive operations only
	postureFull                // vault:<vault> and ops:<vault>
)

// profiles are named for WHO HOLDS the token, because that is what determines
// whether a code can ever be typed. Explicit flags override every field.
//
// The attended/unattended split is the one that breaks things when wrong: a
// token with vault: step-up and no human behind it is a service that dies at
// its first restart and cannot recover.
var profiles = map[string]struct {
	scopes   []string
	posture  posture
	attended bool
}{
	// The plugin persists the vault to disk, so gating its ACCESS protects
	// nothing and would prompt on every restart. Confirming a destructive
	// operation triggered from it is the opposite: worth having.
	"obsidian-plugin": {[]string{auth.ScopeRead, auth.ScopeWrite, auth.ScopeDelete}, postureOps, true},
	"mcp-client":      {[]string{auth.ScopeRead, auth.ScopeWrite}, postureFull, true},
	// memo-ai is an MCP client running under dagu with nobody watching: the
	// same hazard as the relay's own token, reached through a name that
	// describes the surface rather than who is present.
	"mcp-scheduled":    {[]string{auth.ScopeRead, auth.ScopeWrite}, postureNone, false},
	"relay-background": {[]string{auth.ScopeRead}, postureNone, false},
}

func profileNames() string {
	names := make([]string, 0, len(profiles))
	for n := range profiles {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func flagWasSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

// protectedIn lists the protected vaults under a root. An unreadable entry is
// an error, never a silent "not protected".
func protectedIn(l vaults.Layout) ([]string, error) {
	names, err := l.Discover()
	if err != nil {
		return nil, fmt.Errorf("listing vaults under %s: %w", l.Root, err)
	}
	out := []string{}
	for _, n := range names {
		_, err := os.Lstat(filepath.Join(l.GitDir(n), vaults.StepUpMarker))
		switch {
		case err == nil:
			out = append(out, n)
		case errors.Is(err, fs.ErrNotExist):
		default:
			return nil, fmt.Errorf("cannot tell whether %s is protected: %w", n, err)
		}
	}
	return out, nil
}

// gatedOnAccess keeps the plugin warning to vault: entries. An ops: posture on a
// plugin token is a feature, not a mistake.
func gatedOnAccess(p auth.Principal) bool {
	for _, e := range p.RequiresStepUpAuth {
		if strings.HasPrefix(e, auth.StepUpVault+":") {
			return true
		}
	}
	return false
}

// Handles reports whether name is this package's subcommand.
func Handles(name string) bool { return name == "token" }

// Run executes `token <add|list|revoke>`. args excludes the "token" word.
func Run(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("token: need a subcommand: add, list or revoke")
	}
	switch args[0] {
	case "add":
		return runAdd(args[1:], out)
	case "list":
		return runList(args[1:], out)
	case "revoke":
		return runRevoke(args[1:], out)
	default:
		return fmt.Errorf("token: unknown subcommand %q (add, list, revoke)", args[0])
	}
}

func flagSet(name string) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet("token "+name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	path := fs.String("tokens", os.Getenv("ARCHIVIST_TOKENS"),
		"tokens file to read and write")
	return fs, path
}

// load opens the tokens file, treating a missing file as an empty set so the
// first `token add` on a fresh install works without a seed file.
func load(path string) (*auth.Set, error) {
	if path == "" {
		return nil, errors.New("token: -tokens is required (or set ARCHIVIST_TOKENS)")
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return auth.New(), nil
	}
	return auth.Load(path, "")
}

func runAdd(args []string, out io.Writer) error {
	fs, path := flagSet("add")
	label := fs.String("label", "", "human name for this token, shown in listings and logs")
	// vaultNames, not vaults: the package of that name is needed in this
	// function, and a local would shadow it.
	vaultNames := fs.String("vaults", "", `comma-separated vault names, or "*" for every vault`)
	scopes := fs.String("scopes", "read", "comma-separated: read, write, delete")
	canCreate := fs.Bool("can-create-vaults", false, "allow this token to create vaults")
	expires := fs.Duration("expires-in", 0, "expire after this long; 0 means never")
	stepUp := fs.String("step-up", "", "comma-separated vault:<name> / ops:<name> entries requiring a one-time code")
	noStepUp := fs.String("no-step-up", "", "comma-separated protected vaults this token deliberately does not gate")
	profile := fs.String("profile", "", "starting point: "+profileNames())
	root := fs.String("root", os.Getenv("ARCHIVIST_ROOT"), "vault root, for checking which vaults are protected")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *label == "" {
		return errors.New("token add: -label is required; an unlabelled token cannot be revoked confidently later")
	}
	if *vaultNames == "" {
		return errors.New("token add: -vaults is required")
	}

	var prof struct {
		scopes   []string
		posture  posture
		attended bool
	}
	if *profile != "" {
		got, ok := profiles[*profile]
		if !ok {
			return fmt.Errorf("token add: %q is not a profile (%s)", *profile, profileNames())
		}
		prof = got
		if !flagWasSet(fs, "scopes") {
			*scopes = strings.Join(prof.scopes, ",")
		}
		if !prof.attended && *stepUp != "" {
			return fmt.Errorf("token add: %s runs with no human present, so it can never "+
				"present a code; a step-up posture would make it fail at its first restart", *profile)
		}
	}

	// Without a root the protection check cannot run, and a check that skips
	// itself when a variable is unset is not a check.
	if *root == "" {
		return errors.New("token add: -root (or ARCHIVIST_ROOT) is required, so minting can " +
			"tell which vaults are protected")
	}
	layout := vaults.Layout{Root: *root}

	p := auth.Principal{
		Label:           *label,
		Vaults:          split(*vaultNames),
		Scopes:          split(*scopes),
		CanCreateVaults: *canCreate,
		CreatedAt:       time.Now().Unix(),
	}
	// A negative duration asked for an expiry and would silently have produced a
	// permanent token, which is the opposite of what was requested.
	if *expires < 0 {
		return fmt.Errorf("token add: -expires-in must not be negative, got %s", *expires)
	}
	if *expires > 0 {
		p.ExpiresAt = time.Now().Add(*expires).Unix()
	}
	for _, s := range p.Scopes {
		if !auth.ValidScope(s) {
			return fmt.Errorf("token add: %q is not a scope (read, write, delete)", s)
		}
	}

	p.RequiresStepUpAuth = split(*stepUp)
	p.StepUpExempt = split(*noStepUp)

	protectedVaults, err := protectedIn(layout)
	if err != nil {
		return fmt.Errorf("token add: %w", err)
	}
	// A wildcard opens vaults that do not exist yet, so checking it against what
	// happens to be present now proves nothing about tomorrow. Refuse the
	// combination rather than pretend it was verified.
	if slices.Contains(p.Vaults, "*") && len(protectedVaults) > 0 &&
		!flagWasSet(fs, "step-up") && !flagWasSet(fs, "no-step-up") {
		return fmt.Errorf("token add: -vaults * with protected vaults present (%s) needs an "+
			"explicit -step-up or -no-step-up; a wildcard cannot be checked against vaults "+
			"that do not exist yet", strings.Join(protectedVaults, ", "))
	}

	targets := p.Vaults
	if slices.Contains(targets, "*") {
		targets = protectedVaults
	}
	for _, v := range targets {
		if !slices.Contains(protectedVaults, v) || p.StepUpDecided(v) {
			continue
		}
		if *profile == "" {
			return fmt.Errorf("token add: %s requires an explicit step-up posture.\n"+
				"Pass -step-up vault:%s[,ops:%s], or -no-step-up %s to opt out deliberately.", v, v, v, v)
		}
		switch prof.posture {
		case postureFull:
			p.RequiresStepUpAuth = append(p.RequiresStepUpAuth, auth.StepUpVault+":"+v, auth.StepUpOps+":"+v)
		case postureOps:
			p.RequiresStepUpAuth = append(p.RequiresStepUpAuth, auth.StepUpOps+":"+v)
		case postureNone:
			p.StepUpExempt = append(p.StepUpExempt, v)
		}
	}

	if len(p.RequiresStepUpAuth) > 0 {
		secret, err := stepup.NewSecret()
		if err != nil {
			return err
		}
		p.TotpSecret = secret
	}

	var token string
	if err := withFileLock(*path, func() error {
		set, err := load(*path)
		if err != nil {
			return err
		}
		token, err = set.Mint(p)
		if err != nil {
			return err
		}
		return set.Save(*path)
	}); err != nil {
		return err
	}

	// Permissions first, token last. You read what you granted before you copy
	// it, and the token ends up the final line: the easiest to select, and the
	// one that survives scrolling.
	fmt.Fprintf(out, "Label:      %s\n", p.Label)
	if *profile != "" {
		fmt.Fprintf(out, "Profile:    %s\n", *profile)
	}
	fmt.Fprintf(out, "Vaults:     %s\n", strings.Join(p.Vaults, ", "))
	fmt.Fprintf(out, "Scopes:     %s\n", strings.Join(p.Scopes, ", "))
	if len(p.RequiresStepUpAuth) > 0 {
		fmt.Fprintf(out, "Step-up:    %s\n", strings.Join(p.RequiresStepUpAuth, ", "))
	}
	if len(p.StepUpExempt) > 0 {
		fmt.Fprintf(out, "Not gated:  %s\n", strings.Join(p.StepUpExempt, ", "))
	}
	if p.ExpiresAt != 0 {
		fmt.Fprintf(out, "Expires:    %s\n", time.Unix(p.ExpiresAt, 0).Format(time.RFC3339))
	} else {
		fmt.Fprint(out, "Expires:    never\n")
	}
	// The plugin needs BOTH verbs, not just write: it syncs down, and asks the
	// read-scoped POST /v1/have which blobs are missing before it uploads
	// anything. Warning only about write let a write-only token through clean
	// and then fail on the first sync.
	if !p.Can(auth.ScopeRead) || !p.Can(auth.ScopeWrite) {
		fmt.Fprint(out, "\nNOTE: the Obsidian plugin needs both read and write. Do NOT paste "+
			"this token into it -- syncing will fail.\n")
	}
	if gatedOnAccess(p) {
		fmt.Fprint(out, "\nNOTE: a vault: posture is for MCP agents. The Obsidian plugin cannot "+
			"present a code, and it persists the vault to disk anyway. An ops: posture is fine there.\n")
	}
	fmt.Fprint(out, "\nThis is the only time the token and the secret are shown.\n")
	if p.TotpSecret != "" {
		fmt.Fprintf(out, "\nScan this, or run:\n  qrencode -t ANSIUTF8 '%s'\n",
			stepup.URI(p.Label, p.TotpSecret))
	}
	fmt.Fprintf(out, "\n%s\n", token)
	return nil
}

func runList(args []string, out io.Writer) error {
	fs, path := flagSet("list")
	if err := fs.Parse(args); err != nil {
		return err
	}
	set, err := load(*path)
	if err != nil {
		return err
	}
	entries := set.Entries()
	hashes := make([]string, 0, len(entries))
	for h := range entries {
		hashes = append(hashes, h)
	}
	sort.Strings(hashes)

	fmt.Fprintf(out, "%-12s  %-18s  %-20s  %-18s  %s\n", "ID", "LABEL", "VAULTS", "SCOPES", "EXPIRES")
	for _, h := range hashes {
		p := entries[h]
		expiry := "never"
		if p.ExpiresAt != 0 {
			expiry = time.Unix(p.ExpiresAt, 0).Format("2006-01-02")
		}
		fmt.Fprintf(out, "%-12s  %-18s  %-20s  %-18s  %s\n",
			h[:12], p.Label, strings.Join(p.Vaults, ","), strings.Join(p.Scopes, ","), expiry)
	}
	return nil
}

func runRevoke(args []string, out io.Writer) error {
	fs, path := flagSet("revoke")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("token revoke: need exactly one ID prefix (see `token list`)")
	}
	prefix := fs.Arg(0)

	var hash, label string
	if err := withFileLock(*path, func() error {
		set, err := load(*path)
		if err != nil {
			return err
		}
		var matches []string
		for h := range set.Entries() {
			if strings.HasPrefix(h, prefix) {
				matches = append(matches, h)
			}
		}
		switch len(matches) {
		case 0:
			return fmt.Errorf("token revoke: no token starts with %q", prefix)
		case 1:
		default:
			// Refusing beats guessing: revoking the wrong token locks out a
			// device, and the operator cannot tell which one it was afterwards.
			return fmt.Errorf("token revoke: %q matches %d tokens; use more characters",
				prefix, len(matches))
		}
		hash = matches[0]
		label = set.Entries()[hash].Label
		set.Revoke(hash)
		return set.Save(*path)
	}); err != nil {
		return err
	}
	fmt.Fprintf(out, "revoked %s (%s)\n", hash[:12], label)
	return nil
}

func split(s string) []string {
	out := []string{}
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
