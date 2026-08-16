// Package cli is the offline subcommands: history, show, restore, check, export.
//
// These work directly against the repository rather than over HTTP, so they
// still function when the server is stopped -- which is exactly when `check`
// and `export` matter most.
//
// Every capability here is ALSO exposed over REST, because an agent in another
// container cannot run a binary on this host. Both call internal/repo, so they
// cannot drift apart.
package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/pkronstrom/vaultsync/internal/repo"
	"github.com/pkronstrom/vaultsync/internal/vault"
)

// ErrDrift is returned by check when the working tree differs from HEAD, so
// cron can act on the exit status without parsing output.
var ErrDrift = errors.New("working tree differs from the last commit")

// Handles reports whether name is one of our subcommands.
func Handles(name string) bool {
	switch name {
	case "history", "show", "restore", "check", "export":
		return true
	}
	return false
}

type Env struct {
	Vault string
	Git   string
	JSON  bool
	Out   io.Writer
}

func Run(name string, args []string, env Env) error {
	r, err := repo.Open(env.Vault, env.Git)
	if err != nil {
		return err
	}
	switch name {
	case "history":
		return history(r, args, env)
	case "show":
		return show(r, args, env)
	case "restore":
		return restore(r, args, env)
	case "check":
		return check(r, env)
	case "export":
		return export(r, args, env)
	}
	return fmt.Errorf("unknown command %q", name)
}

func history(r *repo.Repo, args []string, env Env) error {
	if len(args) < 1 {
		return errors.New("usage: vaultsync history <path>")
	}
	revs, err := r.History(args[0], 50)
	if err != nil {
		return err
	}
	if env.JSON {
		return json.NewEncoder(env.Out).Encode(revs)
	}
	if len(revs) == 0 {
		fmt.Fprintf(env.Out, "no history for %s\n", args[0])
		return nil
	}
	tw := tabwriter.NewWriter(env.Out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "REVISION\tWHEN\tSIZE\tWHAT")
	for _, v := range revs {
		what := v.Message
		if v.Deleted {
			what = "(deleted) " + what
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n",
			v.Short, v.When.Local().Format("2006-01-02 15:04"), v.Size, what)
	}
	return tw.Flush()
}

// show prints a file at a revision WITHOUT touching the working tree. Looking
// is far more common than restoring, and conflating them makes inspecting an
// old version a destructive act.
func show(r *repo.Repo, args []string, env Env) error {
	if len(args) < 1 {
		return errors.New("usage: vaultsync show <path> [revision]")
	}
	rev := ""
	if len(args) > 1 {
		rev = args[1]
	}
	resolved, err := r.Resolve(rev)
	if err != nil {
		return err
	}
	content, err := r.ReadAt(resolved, args[0])
	if err != nil {
		return fmt.Errorf("%s at %s: %w", args[0], short(resolved), err)
	}
	_, err = env.Out.Write(content)
	return err
}

func restore(r *repo.Repo, args []string, env Env) error {
	if len(args) < 2 {
		return errors.New("usage: vaultsync restore <path> <revision>")
	}
	resolved, err := r.Resolve(args[1])
	if err != nil {
		return err
	}
	content, err := r.ReadAt(resolved, args[0])
	if err != nil {
		return err
	}
	v, err := vault.New(env.Vault)
	if err != nil {
		return err
	}
	defer v.Close()
	if err := v.Write(args[0], content); err != nil {
		return err
	}
	// Deliberately NOT committed here. A running server's watcher will see the
	// write and commit it; committing from a second process would race the
	// server for the git index. If the server is stopped, its startup scan
	// picks it up.
	fmt.Fprintf(env.Out, "restored %s from %s (%d bytes)\n", args[0], short(resolved), len(content))
	return nil
}

func check(r *repo.Repo, env Env) error {
	rep, err := r.Check()
	if err != nil {
		return err
	}
	if env.JSON {
		if err := json.NewEncoder(env.Out).Encode(rep); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(env.Out, "head:  %s\nfiles: %d\n", short(rep.Head), rep.Files)
		report(env.Out, "in git but missing from the vault", rep.Missing)
		report(env.Out, "in the vault but not yet committed", rep.Extra)
		report(env.Out, "modified since the last commit", rep.Differing)
		if rep.OK {
			fmt.Fprintln(env.Out, "OK")
		}
	}
	if !rep.OK {
		return ErrDrift
	}
	return nil
}

func report(w io.Writer, label string, paths []string) {
	if len(paths) == 0 {
		return
	}
	fmt.Fprintf(w, "\n%s (%d):\n", label, len(paths))
	for i, p := range paths {
		if i == 20 {
			fmt.Fprintf(w, "  ... and %d more\n", len(paths)-20)
			break
		}
		fmt.Fprintf(w, "  %s\n", p)
	}
}

func export(r *repo.Repo, args []string, env Env) error {
	out := env.Out
	if len(args) > 0 && args[0] != "-" {
		f, err := os.Create(args[0])
		if err != nil {
			return err
		}
		defer f.Close()
		out = f
	}
	// Uncompressed: gzip here costs a full copy per backup snapshot instead of
	// a delta. See repo.Archive.
	return r.Archive(out, false)
}

func short(h string) string {
	if h == "" {
		return "(none)"
	}
	if len(h) > 8 {
		return h[:8]
	}
	return h
}
