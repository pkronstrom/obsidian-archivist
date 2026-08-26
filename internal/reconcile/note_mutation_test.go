package reconcile

import (
	"bytes"
	"errors"
	"testing"

	"github.com/pkronstrom/obsidian-archivist/internal/repo"
	"github.com/pkronstrom/obsidian-archivist/internal/vault"
	"github.com/pkronstrom/obsidian-archivist/protocol"
)

func TestAppendNoteAppendsToExistingNoteAndReturnsRevisions(t *testing.T) {
	rc, v, r := newRec(t)
	before := []byte("before\n")
	oldHead, revision := seedMutationNote(t, rc, r, "Triage.md", before)
	want := []byte("before\nafter\n")

	head, contentHash, err := rc.AppendNote(
		"Triage.md",
		[]byte("after\n"),
		revision,
		Origin{Device: "agent", Via: "mcp"},
	)
	if err != nil {
		t.Fatalf("AppendNote: %v", err)
	}
	assertMutationResult(t, v, r, "Triage.md", oldHead, head, contentHash, want)
}

func TestAppendNoteRefusesMissingPath(t *testing.T) {
	rc, v, r := newRec(t)
	head, _ := seedMutationNote(t, rc, r, "kept.md", []byte("kept\n"))

	_, _, err := rc.AppendNote(
		"missing.md",
		[]byte("new\n"),
		"",
		Origin{Device: "agent"},
	)
	requireProtocolCode(t, err, protocol.CodeNotFound)
	assertMissingPathAtHead(t, v, r, "missing.md", head)
	assertNoteAtHead(t, v, r, "kept.md", []byte("kept\n"), head)
}

func TestAppendNoteRequiresFullCurrentBlobWhenGuarded(t *testing.T) {
	rc, v, r := newRec(t)
	body := []byte("before\n")
	head, revision := seedMutationNote(t, rc, r, "Triage.md", body)

	// A matching prefix is still stale: the guard is the full Git blob ID.
	_, _, err := rc.AppendNote(
		"Triage.md",
		[]byte("after\n"),
		revision[:12],
		Origin{Device: "agent"},
	)
	requireProtocolCode(t, err, protocol.CodeStale)
	assertNoteAtHead(t, v, r, "Triage.md", body, head)
}

func TestAppendNoteRefusesEmptySuffix(t *testing.T) {
	rc, v, r := newRec(t)
	body := []byte("before\n")
	head, revision := seedMutationNote(t, rc, r, "Triage.md", body)

	_, _, err := rc.AppendNote(
		"Triage.md",
		nil,
		revision,
		Origin{Device: "agent"},
	)
	requireProtocolCode(t, err, protocol.CodeMalformed)
	assertNoteAtHead(t, v, r, "Triage.md", body, head)
}

func TestConcurrentAppendsBothSurviveExactlyOnce(t *testing.T) {
	rc, v, r := newRec(t)
	seedHead, _ := seedMutationNote(t, rc, r, "Triage.md", []byte("start\n"))
	origin := Origin{Device: "agent", Via: "mcp"}

	type result struct {
		head string
		hash string
		err  error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for _, suffix := range [][]byte{[]byte("A\n"), []byte("B\n")} {
		suffix := suffix
		go func() {
			<-start
			head, hash, err := rc.AppendNote("Triage.md", suffix, "", origin)
			results <- result{head: head, hash: hash, err: err}
		}()
	}
	close(start)

	gotResults := []result{<-results, <-results}
	for _, got := range gotResults {
		if got.err != nil {
			t.Fatalf("AppendNote: %v", got.err)
		}
		if got.head == "" || got.head == seedHead {
			t.Errorf("returned head = %q, want a new commit", got.head)
		}
		if got.hash == "" {
			t.Error("returned content hash is empty")
		}
	}
	if gotResults[0].head == gotResults[1].head {
		t.Errorf("concurrent appends returned one head %q", gotResults[0].head)
	}

	body, err := v.Read("Triage.md")
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "start\nA\nB\n" && string(body) != "start\nB\nA\n" {
		t.Fatalf("content = %q, want both atomic appends", body)
	}
	if bytes.Count(body, []byte("A\n")) != 1 || bytes.Count(body, []byte("B\n")) != 1 {
		t.Fatalf("content = %q, want each suffix exactly once", body)
	}

	finalHead, err := r.Head()
	if err != nil {
		t.Fatal(err)
	}
	finalHash := protocol.HashContent(body)
	if finalHead != gotResults[0].head && finalHead != gotResults[1].head {
		t.Errorf("repo head = %q, want one returned append head", finalHead)
	}
	if finalHash != gotResults[0].hash && finalHash != gotResults[1].hash {
		t.Errorf("final content hash = %q, want one returned append hash", finalHash)
	}
}

func TestAppendNoteRefusesNonTextCurrentFile(t *testing.T) {
	tests := []struct {
		name string
		body []byte
	}{
		{name: "NUL binary", body: []byte("before\x00after")},
		{name: "invalid UTF-8", body: []byte{'o', 'k', '\n', 0xff}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rc, v, r := newRec(t)
			head, revision := seedMutationNote(t, rc, r, "Triage.md", tt.body)

			_, _, err := rc.AppendNote(
				"Triage.md",
				[]byte("suffix\n"),
				revision,
				Origin{Device: "agent"},
			)
			requireProtocolCode(t, err, protocol.CodeNotText)
			assertNoteAtHead(t, v, r, "Triage.md", tt.body, head)
		})
	}
}

func TestEditNoteReplacesOneExactMatchAndReturnsRevisions(t *testing.T) {
	rc, v, r := newRec(t)
	before := []byte("before\ntarget\nafter\n")
	oldHead, revision := seedMutationNote(t, rc, r, "Triage.md", before)
	want := []byte("before\nchanged\nafter\n")

	head, contentHash, err := rc.EditNote(
		"Triage.md",
		revision,
		[]byte("target"),
		[]byte("changed"),
		Origin{Device: "agent", Via: "mcp"},
	)
	if err != nil {
		t.Fatalf("EditNote: %v", err)
	}
	assertMutationResult(t, v, r, "Triage.md", oldHead, head, contentHash, want)
}

func TestEditNoteRequiresRevision(t *testing.T) {
	rc, v, r := newRec(t)
	body := []byte("before\ntarget\nafter\n")
	head, _ := seedMutationNote(t, rc, r, "Triage.md", body)

	_, _, err := rc.EditNote(
		"Triage.md",
		"",
		[]byte("target"),
		[]byte("changed"),
		Origin{Device: "agent"},
	)
	requireProtocolCode(t, err, protocol.CodeMalformed)
	assertNoteAtHead(t, v, r, "Triage.md", body, head)
}

func TestEditNoteRequiresFullCurrentBlob(t *testing.T) {
	rc, v, r := newRec(t)
	body := []byte("before\ntarget\nafter\n")
	head, revision := seedMutationNote(t, rc, r, "Triage.md", body)

	_, _, err := rc.EditNote(
		"Triage.md",
		revision[:12],
		[]byte("target"),
		[]byte("changed"),
		Origin{Device: "agent"},
	)
	requireProtocolCode(t, err, protocol.CodeStale)
	assertNoteAtHead(t, v, r, "Triage.md", body, head)
}

func TestEditNoteRefusesEmptyOldText(t *testing.T) {
	rc, v, r := newRec(t)
	body := []byte("before\ntarget\nafter\n")
	head, revision := seedMutationNote(t, rc, r, "Triage.md", body)

	_, _, err := rc.EditNote(
		"Triage.md",
		revision,
		nil,
		[]byte("changed"),
		Origin{Device: "agent"},
	)
	requireProtocolCode(t, err, protocol.CodeMalformed)
	assertNoteAtHead(t, v, r, "Triage.md", body, head)
}

func TestEditNoteRefusesZeroMatches(t *testing.T) {
	rc, v, r := newRec(t)
	body := []byte("before\ntarget\nafter\n")
	head, revision := seedMutationNote(t, rc, r, "Triage.md", body)

	_, _, err := rc.EditNote(
		"Triage.md",
		revision,
		[]byte("missing"),
		[]byte("changed"),
		Origin{Device: "agent"},
	)
	requireProtocolCode(t, err, protocol.CodeNoMatch)
	assertNoteAtHead(t, v, r, "Triage.md", body, head)
}

func TestEditNoteRefusesMultipleMatches(t *testing.T) {
	rc, v, r := newRec(t)
	body := []byte("target\nbetween\ntarget\n")
	head, revision := seedMutationNote(t, rc, r, "Triage.md", body)

	_, _, err := rc.EditNote(
		"Triage.md",
		revision,
		[]byte("target"),
		[]byte("changed"),
		Origin{Device: "agent"},
	)
	requireProtocolCode(t, err, protocol.CodeMultipleMatches)
	assertNoteAtHead(t, v, r, "Triage.md", body, head)
}

func TestEditNoteRefusesNonTextCurrentFile(t *testing.T) {
	tests := []struct {
		name string
		body []byte
	}{
		{name: "NUL binary", body: []byte("before\x00target")},
		{name: "invalid UTF-8", body: []byte{'t', 'a', 'r', 'g', 'e', 't', 0xff}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rc, v, r := newRec(t)
			head, revision := seedMutationNote(t, rc, r, "Triage.md", tt.body)

			_, _, err := rc.EditNote(
				"Triage.md",
				revision,
				[]byte("target"),
				[]byte("changed"),
				Origin{Device: "agent"},
			)
			requireProtocolCode(t, err, protocol.CodeNotText)
			assertNoteAtHead(t, v, r, "Triage.md", tt.body, head)
		})
	}
}

func TestEditAndAppendDoNotRecreateMovedSource(t *testing.T) {
	rc, v, r := newRec(t)
	body := []byte("before\ntarget\nafter\n")
	base, revision := seedMutationNote(t, rc, r, "old.md", body)
	movedHead, results, err := rc.Push(base, "phone", []Change{{
		Path: "new.md",
		Op:   protocol.OpMove,
		From: "old.md",
	}})
	if err != nil {
		t.Fatalf("moving note: %v", err)
	}
	if len(results) != 1 || results[0].Status != protocol.StatusApplied {
		t.Fatalf("move results = %+v, want one applied", results)
	}
	assertMissingPathAtHead(t, v, r, "old.md", movedHead)
	assertNoteAtHead(t, v, r, "new.md", body, movedHead)

	_, _, err = rc.EditNote(
		"old.md",
		revision,
		[]byte("target"),
		[]byte("changed"),
		Origin{Device: "agent"},
	)
	requireProtocolCode(t, err, protocol.CodeNotFound)
	assertMissingPathAtHead(t, v, r, "old.md", movedHead)
	assertNoteAtHead(t, v, r, "new.md", body, movedHead)

	_, _, err = rc.AppendNote(
		"old.md",
		[]byte("suffix\n"),
		revision,
		Origin{Device: "agent"},
	)
	requireProtocolCode(t, err, protocol.CodeNotFound)
	assertMissingPathAtHead(t, v, r, "old.md", movedHead)
	assertNoteAtHead(t, v, r, "new.md", body, movedHead)
}

func seedMutationNote(t *testing.T, rc *Reconciler, r *repo.Repo, path string, body []byte) (head, contentHash string) {
	t.Helper()
	head, results, err := rc.Push("", "seed", []Change{put(t, r, path, string(body))})
	if err != nil {
		t.Fatalf("seeding %s: %v", path, err)
	}
	if len(results) != 1 || results[0].Status != protocol.StatusApplied {
		t.Fatalf("seed results = %+v, want one applied", results)
	}
	return head, protocol.HashContent(body)
}

func requireProtocolCode(t *testing.T, err error, want string) {
	t.Helper()
	var protocolErr *protocol.Error
	if !errors.As(err, &protocolErr) {
		t.Fatalf("error = %v, want *protocol.Error with code %q", err, want)
	}
	if protocolErr.Code != want {
		t.Fatalf("error code = %q, want %q (error: %v)", protocolErr.Code, want, err)
	}
}

func assertMutationResult(
	t *testing.T,
	v *vault.Vault,
	r *repo.Repo,
	path, oldHead, head, contentHash string,
	want []byte,
) {
	t.Helper()
	if head == "" || head == oldHead {
		t.Errorf("returned head = %q, want a new commit after %q", head, oldHead)
	}
	if wantHash := protocol.HashContent(want); contentHash != wantHash {
		t.Errorf("returned content hash = %q, want %q", contentHash, wantHash)
	}
	assertNoteAtHead(t, v, r, path, want, head)
}

func assertNoteAtHead(t *testing.T, v *vault.Vault, r *repo.Repo, path string, want []byte, wantHead string) {
	t.Helper()
	got, err := v.Read(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s = %q, want %q", path, got, want)
	}
	assertRepoHead(t, r, wantHead)
}

func assertMissingPathAtHead(t *testing.T, v *vault.Vault, r *repo.Repo, path, wantHead string) {
	t.Helper()
	if exists(v, path) {
		t.Errorf("%s exists, want it absent", path)
	}
	assertRepoHead(t, r, wantHead)
}

func assertRepoHead(t *testing.T, r *repo.Repo, want string) {
	t.Helper()
	got, err := r.Head()
	if err != nil {
		t.Fatalf("reading repository head: %v", err)
	}
	if got != want {
		t.Errorf("repository head = %q, want %q", got, want)
	}
}
