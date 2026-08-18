package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pkronstrom/obsidian-archivist/internal/repo"
)

func cliRepo(t *testing.T) (*repo.Repo, string) {
	t.Helper()
	base := t.TempDir()
	work := filepath.Join(base, "vault")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := repo.Open(work, filepath.Join(base, "git"))
	if err != nil {
		t.Fatal(err)
	}
	return r, work
}

func commitFile(t *testing.T, r *repo.Repo, work, path, content, msg string) {
	t.Helper()
	full := filepath.Join(work, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit(msg); err != nil {
		t.Fatal(err)
	}
}

func TestReclaimReportListsDeletedPaths(t *testing.T) {
	r, work := cliRepo(t)
	commitFile(t, r, work, "keep.md", "kept", "add keep")
	commitFile(t, r, work, "big.bin", "bytes to reclaim", "add big")
	os.Remove(filepath.Join(work, "big.bin"))
	if _, err := r.Commit("delete big"); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := reclaim(r, nil, Env{Out: &out, Reclaim: &ReclaimFlags{OlderThan: "90d"}}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "big.bin") {
		t.Errorf("report does not mention the deleted file:\n%s", got)
	}
	if strings.Contains(got, "keep.md") {
		t.Errorf("report mentions a live file:\n%s", got)
	}
}

// The report must never modify anything. It is the safe half of this command
// and the whole reason it exists separately.
func TestReclaimReportDoesNotRewrite(t *testing.T) {
	r, work := cliRepo(t)
	commitFile(t, r, work, "big.bin", "bytes", "add")
	os.Remove(filepath.Join(work, "big.bin"))
	if _, err := r.Commit("delete"); err != nil {
		t.Fatal(err)
	}
	before, _ := r.Head()

	var out bytes.Buffer
	if err := reclaim(r, nil, Env{Out: &out, Reclaim: &ReclaimFlags{OlderThan: "90d"}}); err != nil {
		t.Fatal(err)
	}
	after, _ := r.Head()
	if before != after {
		t.Fatalf("the report rewrote history: %s -> %s", before, after)
	}
}

// --prune without --yes must explain and stop. Rewriting history on a bare
// flag would be the wrong default for an operation that stops the server.
func TestPruneWithoutYesDoesNothing(t *testing.T) {
	r, work := cliRepo(t)
	commitFile(t, r, work, "big.bin", "bytes", "add")
	os.Remove(filepath.Join(work, "big.bin"))
	if _, err := r.Commit("delete"); err != nil {
		t.Fatal(err)
	}
	before, _ := r.Head()

	var out bytes.Buffer
	if err := reclaim(r, nil, Env{Out: &out, Reclaim: &ReclaimFlags{Prune: true, OlderThan: "0d"}}); err != nil {
		t.Fatal(err)
	}
	after, _ := r.Head()
	if before != after {
		t.Fatal("--prune without --yes rewrote history")
	}
	if !strings.Contains(out.String(), "--yes") {
		t.Errorf("no instruction to confirm:\n%s", out.String())
	}
}

// The age cutoff is the safety property: recent deletions stay recoverable
// however the command is invoked.
func TestPruneSkipsRecentDeletions(t *testing.T) {
	r, work := cliRepo(t)
	commitFile(t, r, work, "big.bin", "bytes", "add")
	os.Remove(filepath.Join(work, "big.bin"))
	if _, err := r.Commit("delete"); err != nil {
		t.Fatal(err)
	}
	before, _ := r.Head()

	var out bytes.Buffer
	// Default cutoff is 90d and the deletion just happened.
	if err := reclaim(r, nil, Env{Out: &out, Reclaim: &ReclaimFlags{Prune: true, Yes: true, OlderThan: "90d"}}); err != nil {
		t.Fatal(err)
	}
	after, _ := r.Head()
	if before != after {
		t.Fatal("a deletion from moments ago was pruned under the 90d default")
	}
	if !strings.Contains(out.String(), "nothing eligible") {
		t.Errorf("unclear output:\n%s", out.String())
	}
}

func TestPruneWithYesRewrites(t *testing.T) {
	r, work := cliRepo(t)
	commitFile(t, r, work, "keep.md", "kept", "add keep")
	commitFile(t, r, work, "big.bin", "bytes to reclaim", "add big")
	os.Remove(filepath.Join(work, "big.bin"))
	if _, err := r.Commit("delete big"); err != nil {
		t.Fatal(err)
	}
	before, _ := r.Head()

	var out bytes.Buffer
	if err := reclaim(r, nil, Env{Out: &out, Reclaim: &ReclaimFlags{Prune: true, Yes: true, OlderThan: "0d"}}); err != nil {
		t.Fatal(err)
	}
	after, _ := r.Head()
	if before == after {
		t.Fatal("nothing was rewritten")
	}
	if !strings.Contains(out.String(), "reclaimed") {
		t.Errorf("no summary:\n%s", out.String())
	}

	left, err := r.Reclaimable()
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("still reclaimable after prune: %+v", left)
	}
}

func TestParseAge(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want time.Duration
		bad  bool
	}{
		{in: "90d", want: 90 * 24 * time.Hour},
		{in: "0d", want: 0},
		{in: "720h", want: 720 * time.Hour},
		{in: "", want: defaultMinAge},
		{in: "banana", bad: true},
		{in: "xd", bad: true},
	} {
		got, err := parseAge(tc.in)
		if tc.bad {
			if err == nil {
				t.Errorf("parseAge(%q) accepted a bad value", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseAge(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseAge(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
