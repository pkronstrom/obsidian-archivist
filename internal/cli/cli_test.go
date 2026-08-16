package cli

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pkronstrom/vaultsync/internal/repo"
	"github.com/pkronstrom/vaultsync/internal/vault"
)

func setup(t *testing.T) (Env, *vault.Vault, *repo.Repo) {
	t.Helper()
	base := t.TempDir()
	work := filepath.Join(base, "vault")
	gitdir := filepath.Join(base, "git")
	v, err := vault.New(work)
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Open(work, gitdir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { v.Close() })
	return Env{Vault: work, Git: gitdir, Out: &bytes.Buffer{}}, v, r
}

func run(t *testing.T, env Env, name string, args ...string) (string, error) {
	t.Helper()
	buf := &bytes.Buffer{}
	env.Out = buf
	err := Run(name, args, env)
	return buf.String(), err
}

func TestHistoryAndShow(t *testing.T) {
	env, v, r := setup(t)
	v.Write("a.md", []byte("version one\n"))
	r.Commit("first")
	v.Write("a.md", []byte("version two\n"))
	r.Commit("second")

	out, err := run(t, env, "history", "a.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "REVISION") || strings.Count(out, "\n") < 3 {
		t.Errorf("history output looks wrong:\n%s", out)
	}

	// show without a revision is the current version.
	out, err = run(t, env, "show", "a.md")
	if err != nil || out != "version two\n" {
		t.Errorf("show = %q, %v", out, err)
	}

	// show at an old revision must NOT modify the vault.
	revs, _ := r.History("a.md", 10)
	out, err = run(t, env, "show", "a.md", revs[1].Short)
	if err != nil {
		t.Fatal(err)
	}
	if out != "version one\n" {
		t.Errorf("show at an old revision = %q", out)
	}
	cur, _ := v.Read("a.md")
	if string(cur) != "version two\n" {
		t.Errorf("show modified the vault: %q", cur)
	}
}

func TestRestoreWritesButDoesNotCommit(t *testing.T) {
	env, v, r := setup(t)
	v.Write("a.md", []byte("good\n"))
	r.Commit("first")
	head1, _ := r.Head()
	v.Write("a.md", []byte("ruined\n"))
	r.Commit("second")
	head2, _ := r.Head()

	if _, err := run(t, env, "restore", "a.md", head1[:8]); err != nil {
		t.Fatal(err)
	}
	got, _ := v.Read("a.md")
	if string(got) != "good\n" {
		t.Errorf("restored content = %q", got)
	}
	// Committing here would race a running server for the git index.
	head3, _ := r.Head()
	if head3 != head2 {
		t.Errorf("restore committed: %s -> %s", head2, head3)
	}
}

func TestCheckReportsDriftWithANonZeroError(t *testing.T) {
	env, v, r := setup(t)
	v.Write("a.md", []byte("x\n"))
	r.Commit("first")

	out, err := run(t, env, "check")
	if err != nil {
		t.Fatalf("check on a clean tree: %v (%s)", err, out)
	}
	if !strings.Contains(out, "OK") {
		t.Errorf("check output = %q", out)
	}

	v.Remove("a.md")
	out, err = run(t, env, "check")
	if !errors.Is(err, ErrDrift) {
		t.Errorf("err = %v, want ErrDrift so cron can use the exit status", err)
	}
	if !strings.Contains(out, "a.md") {
		t.Errorf("check did not name the missing file:\n%s", out)
	}
}

func TestExportProducesAnArchive(t *testing.T) {
	env, v, r := setup(t)
	v.Write("a.md", []byte("x\n"))
	r.Commit("first")
	out, err := run(t, env, "export")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) < 1000 {
		t.Errorf("archive suspiciously small: %d bytes", len(out))
	}
}
