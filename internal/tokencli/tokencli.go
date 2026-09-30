// Package tokencli is the `archivist-server token` subcommand: mint, list,
// update and revoke tokens in the tokens file.
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
	"os"
	"sort"
	"strings"
	"time"

	"github.com/pkronstrom/obsidian-archivist/internal/auth"
	"github.com/pkronstrom/obsidian-archivist/internal/stepup"
)

// profiles are scope presets named for WHO HOLDS the token. They do not set
// step-up: that is always an explicit -step-up, so a gate is never implied by a
// name. Explicit -scopes overrides the preset.
var profiles = map[string][]string{
	// The plugin needs all three: it syncs down, uploads, and propagates
	// deletes.
	"obsidian-plugin": {auth.ScopeRead, auth.ScopeWrite, auth.ScopeDelete},
	"mcp-client":      {auth.ScopeRead, auth.ScopeWrite},
	// memo-ai under dagu: an MCP client with nobody watching, so never give it
	// -step-up -- it could not present a code after its first restart.
	"mcp-scheduled":    {auth.ScopeRead, auth.ScopeWrite},
	"relay-background": {auth.ScopeRead},
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

// Handles reports whether name is this package's subcommand.
func Handles(name string) bool { return name == "token" }

// Run executes `token <add|list|update|revoke>`. args excludes the "token" word.
func Run(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("token: need a subcommand: add, list, update or revoke")
	}
	switch args[0] {
	case "add":
		return runAdd(args[1:], out)
	case "list":
		return runList(args[1:], out)
	case "update":
		return runUpdate(args[1:], out)
	case "revoke":
		return runRevoke(args[1:], out)
	default:
		return fmt.Errorf("token: unknown subcommand %q (add, list, update, revoke)", args[0])
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
	return auth.Load(path)
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
	stepUp := fs.String("step-up", "", "comma-separated vaults this token must unlock with a one-time code")
	profile := fs.String("profile", "", "scope preset: "+profileNames())
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *label == "" {
		return errors.New("token add: -label is required; an unlabelled token cannot be revoked confidently later")
	}
	if *vaultNames == "" {
		return errors.New("token add: -vaults is required")
	}
	if *profile != "" {
		preset, ok := profiles[*profile]
		if !ok {
			return fmt.Errorf("token add: %q is not a profile (%s)", *profile, profileNames())
		}
		if !flagWasSet(fs, "scopes") {
			*scopes = strings.Join(preset, ",")
		}
	}

	p := auth.Principal{
		Label:           *label,
		Vaults:          split(*vaultNames),
		Scopes:          split(*scopes),
		CanCreateVaults: *canCreate,
		CreatedAt:       time.Now().Unix(),
		StepUp:          split(*stepUp),
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
	if len(p.StepUp) > 0 {
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
	printPermissions(out, p)
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

	fmt.Fprintf(out, "%-12s  %-22s  %-20s  %-18s  %-12s  %s\n", "ID", "LABEL", "VAULTS", "SCOPES", "STEP-UP", "EXPIRES")
	for _, h := range hashes {
		p := entries[h]
		expiry := "never"
		if p.ExpiresAt != 0 {
			expiry = time.Unix(p.ExpiresAt, 0).Format("2006-01-02")
		}
		fmt.Fprintf(out, "%-12s  %-22s  %-20s  %-18s  %-12s  %s\n",
			h[:12], p.Label, strings.Join(p.Vaults, ","), strings.Join(p.Scopes, ","),
			orNone(p.StepUp), expiry)
	}
	return nil
}

// matchPrefix resolves an ID prefix from `token list` to exactly one hash.
//
// Refusing beats guessing: acting on the wrong token locks out or widens a
// device, and the operator cannot tell which one it was afterwards.
func matchPrefix(set *auth.Set, prefix string) (string, error) {
	var matches []string
	for h := range set.Entries() {
		if strings.HasPrefix(h, prefix) {
			matches = append(matches, h)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no token starts with %q", prefix)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("%q matches %d tokens; use more characters", prefix, len(matches))
	}
}

// runUpdate changes an existing token's permissions in place. The bearer
// secret is unchanged, so no device needs a new credential. Only the flags
// given change; everything else is kept.
//
// Takes the ID prefix first (`token update <id> -step-up work`) or last.
func runUpdate(args []string, out io.Writer) error {
	fs, path := flagSet("update")
	label := fs.String("label", "", "new label")
	vaultNames := fs.String("vaults", "", `comma-separated vault names, or "*" for every vault`)
	scopes := fs.String("scopes", "", "comma-separated: read, write, delete")
	expires := fs.Duration("expires-in", 0, "expire this long from now; 0 means never")
	stepUp := fs.String("step-up", "", "comma-separated vaults needing a one-time code; empty clears it")

	var prefix string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		prefix, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if prefix == "" && fs.NArg() == 1 {
		prefix = fs.Arg(0)
	} else if fs.NArg() != 0 || prefix == "" {
		return errors.New("token update: need exactly one ID prefix (see `token list`)")
	}
	changed := 0
	fs.Visit(func(f *flag.Flag) {
		if f.Name != "tokens" {
			changed++
		}
	})
	if changed == 0 {
		return errors.New("token update: nothing to change; pass -vaults, -scopes, -step-up, -expires-in or -label")
	}
	if *expires < 0 {
		return fmt.Errorf("token update: -expires-in must not be negative, got %s", *expires)
	}

	var hash string
	var p auth.Principal
	newSecret := false
	if err := withFileLock(*path, func() error {
		set, err := load(*path)
		if err != nil {
			return err
		}
		hash, err = matchPrefix(set, prefix)
		if err != nil {
			return fmt.Errorf("token update: %w", err)
		}
		p = set.Entries()[hash]
		if flagWasSet(fs, "label") {
			p.Label = *label
		}
		if flagWasSet(fs, "vaults") {
			p.Vaults = split(*vaultNames)
		}
		if flagWasSet(fs, "scopes") {
			p.Scopes = split(*scopes)
		}
		if flagWasSet(fs, "expires-in") {
			p.ExpiresAt = 0
			if *expires > 0 {
				p.ExpiresAt = time.Now().Add(*expires).Unix()
			}
		}
		if flagWasSet(fs, "step-up") {
			p.StepUp = split(*stepUp)
			switch {
			case len(p.StepUp) == 0:
				p.TotpSecret = ""
			case p.TotpSecret == "":
				// The authenticator entry already scanned for this token keeps
				// working when step-up only changes vaults; a secret is minted
				// only when the token had none.
				if p.TotpSecret, err = stepup.NewSecret(); err != nil {
					return err
				}
				newSecret = true
			}
		}
		if err := set.Update(hash, p); err != nil {
			return fmt.Errorf("token update: %w", err)
		}
		return set.Save(*path)
	}); err != nil {
		return err
	}

	fmt.Fprintf(out, "Updated:    %s (%s)\n", hash[:12], p.Label)
	printPermissions(out, p)
	if newSecret {
		fmt.Fprintf(out, "\nThis is the only time the secret is shown. Scan this, or run:\n  qrencode -t ANSIUTF8 '%s'\n",
			stepup.URI(p.Label, p.TotpSecret))
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
		hash, err = matchPrefix(set, prefix)
		if err != nil {
			return fmt.Errorf("token revoke: %w", err)
		}
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

func orNone(v []string) string {
	if len(v) == 0 {
		return "none"
	}
	return strings.Join(v, ",")
}

// printPermissions is what add and update both show: what the token may do,
// then the warnings that follow from it.
func printPermissions(out io.Writer, p auth.Principal) {
	fmt.Fprintf(out, "Vaults:     %s\n", strings.Join(p.Vaults, ", "))
	fmt.Fprintf(out, "Scopes:     %s\n", strings.Join(p.Scopes, ", "))
	// Always printed, "none" included: this line is what catches a forgotten
	// -step-up at mint or update time, so it must be there to read when it is missing.
	fmt.Fprintf(out, "Step-up:    %s\n", orNone(p.StepUp))
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
	if len(p.StepUp) > 0 {
		fmt.Fprint(out, "\nNOTE: step-up is for MCP agents. The Obsidian plugin cannot present a "+
			"code, so do NOT paste this token into it for a gated vault.\n")
	}
}
