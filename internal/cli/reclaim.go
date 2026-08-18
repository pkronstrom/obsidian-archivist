package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/pkronstrom/obsidian-archivist/internal/repo"
)

// defaultMinAge is how long a path must have been deleted before prune will
// touch it.
//
// Recent deletions are the ones you might want back, and history is what makes
// getting them back possible. Reclaiming space is worth far less than undoing
// a mistake made yesterday.
const defaultMinAge = 90 * 24 * time.Hour

// ReclaimFlags are reclaim's options.
//
// They are registered on the caller's flagset rather than parsed here. The
// binary parses -vault/-git/-json for every subcommand, and Go's flag package
// stops at the first flag it does not recognise, so a second flagset would
// never see them: `reclaim --prune` failed with "flag provided but not
// defined" before this was one flagset.
type ReclaimFlags struct {
	Prune     bool
	OlderThan string
	MinSize   int64
	Yes       bool
}

// RegisterReclaimFlags adds reclaim's options to fs.
func RegisterReclaimFlags(fs *flag.FlagSet) *ReclaimFlags {
	rf := &ReclaimFlags{}
	fs.BoolVar(&rf.Prune, "prune", false, "rewrite history to remove eligible paths (server must be stopped)")
	fs.StringVar(&rf.OlderThan, "older-than", "90d", "only touch paths deleted at least this long ago")
	fs.Int64Var(&rf.MinSize, "min-size", 0, "ignore paths smaller than this many bytes")
	fs.BoolVar(&rf.Yes, "yes", false, "actually prune; without it --prune only explains what it would do")
	return rf
}

func reclaim(r *repo.Repo, args []string, env Env) error {
	rf := env.Reclaim
	if rf == nil {
		rf = &ReclaimFlags{OlderThan: "90d"}
	}
	doPrune, minSize, yes := &rf.Prune, &rf.MinSize, &rf.Yes
	olderStr := &rf.OlderThan
	older, err := parseAge(*olderStr)
	if err != nil {
		return err
	}

	items, err := r.Reclaimable()
	if err != nil {
		return err
	}

	now := time.Now()
	var eligible []repo.Reclaimable
	var total, eligibleBytes int64
	for _, it := range items {
		total += it.Bytes
		if it.Bytes < *minSize {
			continue
		}
		if it.GoneFor(now) < older {
			continue
		}
		eligible = append(eligible, it)
		eligibleBytes += it.Bytes
	}

	if env.JSON {
		return json.NewEncoder(env.Out).Encode(map[string]any{
			"all":            items,
			"eligible":       eligible,
			"total_bytes":    total,
			"eligible_bytes": eligibleBytes,
			"older_than":     older.String(),
			"would_prune":    *doPrune,
		})
	}

	if !*doPrune {
		return reportOnly(items, eligible, total, eligibleBytes, older, now, env)
	}

	if len(eligible) == 0 {
		fmt.Fprintf(env.Out, "nothing eligible: %d deleted path(s) found, none deleted more than %s ago\n",
			len(items), *olderStr)
		return nil
	}

	paths := make([]string, 0, len(eligible))
	for _, it := range eligible {
		paths = append(paths, it.Path)
	}

	if !*yes {
		fmt.Fprintf(env.Out, "About to rewrite history, removing %d path(s) and about %s.\n",
			len(paths), humanBytes(eligibleBytes))
		fmt.Fprintf(env.Out, "The server must be STOPPED. Devices keep syncing normally afterwards.\n")
		fmt.Fprintf(env.Out, "Re-run with --yes to proceed.\n")
		return nil
	}

	res, err := r.Prune(paths)
	if err != nil {
		return err
	}
	fmt.Fprintf(env.Out, "rewrote %d commit(s)\n", res.Rewrote)
	fmt.Fprintf(env.Out, "head %s -> %s\n", short(res.OldHead), short(res.NewHead))
	fmt.Fprintf(env.Out, "reclaimed %s\n", humanBytes(res.Reclaimed))
	fmt.Fprintf(env.Out, "\nStart the server. Devices translate the old head automatically;\n")
	fmt.Fprintf(env.Out, "none of them needs to re-download the vault.\n")
	return nil
}

func reportOnly(all, eligible []repo.Reclaimable, total, eligibleBytes int64, older time.Duration, now time.Time, env Env) error {
	if len(all) == 0 {
		fmt.Fprintln(env.Out, "Nothing to reclaim: no deleted paths are still costing space.")
		return nil
	}

	w := tabwriter.NewWriter(env.Out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "PATH\tSIZE\tVERSIONS\tADDED\tDELETED\tGONE")
	for _, it := range all {
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\n",
			it.Path, humanBytes(it.Bytes), it.Versions,
			it.Added.Format("2006-01-02"), it.Deleted.Format("2006-01-02"),
			humanAge(it.GoneFor(now)))
	}
	w.Flush()

	fmt.Fprintf(env.Out, "\n%d deleted path(s), %s total.\n", len(all), humanBytes(total))
	fmt.Fprintf(env.Out, "%d eligible to prune (deleted over %s ago), %s.\n",
		len(eligible), humanAge(older), humanBytes(eligibleBytes))
	if len(eligible) > 0 {
		fmt.Fprintf(env.Out, "\nStop the server, then: archivist-server reclaim --prune --yes\n")
	}
	return nil
}

// parseAge accepts a day suffix as well as Go durations, because the useful
// units here are months and "2160h" is not a thing anyone should have to work
// out.
func parseAge(s string) (time.Duration, error) {
	if s == "" {
		return defaultMinAge, nil
	}
	if n := len(s); n > 1 && s[n-1] == 'd' {
		var days int
		if _, err := fmt.Sscanf(s[:n-1], "%d", &days); err != nil {
			return 0, fmt.Errorf("bad --older-than %q: want something like 90d or 720h", s)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("bad --older-than %q: want something like 90d or 720h", s)
	}
	return d, nil
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func humanAge(d time.Duration) string {
	days := int(d.Hours() / 24)
	switch {
	case days >= 365:
		return fmt.Sprintf("%dy", days/365)
	case days >= 1:
		return fmt.Sprintf("%dd", days)
	default:
		return "today"
	}
}
