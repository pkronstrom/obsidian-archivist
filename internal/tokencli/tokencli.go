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
	"os"
	"sort"
	"strings"
	"time"

	"github.com/pkronstrom/obsidian-archivist/internal/auth"
)

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
	vaults := fs.String("vaults", "", `comma-separated vault names, or "*" for every vault`)
	scopes := fs.String("scopes", "read", "comma-separated: read, write, delete")
	canCreate := fs.Bool("can-create-vaults", false, "allow this token to create vaults")
	expires := fs.Duration("expires-in", 0, "expire after this long; 0 means never")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *label == "" {
		return errors.New("token add: -label is required; an unlabelled token cannot be revoked confidently later")
	}
	if *vaults == "" {
		return errors.New("token add: -vaults is required")
	}

	p := auth.Principal{
		Label:           *label,
		Vaults:          split(*vaults),
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

	fmt.Fprintf(out, "%s\n", token)
	fmt.Fprintf(out, "\nLabel:  %s\nVaults: %s\nScopes: %s\n",
		p.Label, strings.Join(p.Vaults, ", "), strings.Join(p.Scopes, ", "))
	if p.ExpiresAt != 0 {
		fmt.Fprintf(out, "Expires: %s\n", time.Unix(p.ExpiresAt, 0).Format(time.RFC3339))
	}
	fmt.Fprint(out, "\nThis is the only time the token is shown. It is stored hashed.\n")
	// The plugin needs BOTH verbs, not just write: it syncs down, and asks the
	// read-scoped POST /v1/have which blobs are missing before it uploads
	// anything. Warning only about write let a write-only token through clean
	// and then fail on the first sync.
	if !p.Can(auth.ScopeRead) || !p.Can(auth.ScopeWrite) {
		fmt.Fprint(out, "NOTE: the Obsidian plugin needs both read and write. Do NOT paste "+
			"this token into it -- syncing will fail.\n")
	}
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
