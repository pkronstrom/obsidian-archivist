package client_test

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pkronstrom/obsidian-archivist/internal/api"
	"github.com/pkronstrom/obsidian-archivist/internal/client"
	"github.com/pkronstrom/obsidian-archivist/internal/reconcile"
	"github.com/pkronstrom/obsidian-archivist/internal/repo"
	"github.com/pkronstrom/obsidian-archivist/internal/vault"
	"github.com/pkronstrom/obsidian-archivist/protocol"
)

// These run against a REAL server -- the actual handlers, reconciler and git
// repository, not a double. Every bug in the TypeScript client was found this
// way and none by unit tests, so the Go client gets the same treatment from the
// start.
func live(t *testing.T) (*client.Client, *vault.Vault) {
	t.Helper()
	base := t.TempDir()
	work := filepath.Join(base, "vault")
	v, err := vault.New(work)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { v.Close() })

	r, err := repo.Open(work, filepath.Join(base, "git"))
	if err != nil {
		t.Fatal(err)
	}
	r.SetSyncable(func(p string) bool { return !vault.Skip(p) })

	srv := httptest.NewServer(api.New(reconcile.New(v, r), r, "live-token"))
	t.Cleanup(srv.Close)

	return client.New(srv.URL, "live-token", "relay"), v
}

func TestLiveWriteReadRoundTrip(t *testing.T) {
	c, v := live(t)
	ctx := context.Background()

	res, err := c.Write(ctx, "notes/a.md", []byte("# hello\n\nbody\n"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if res.Status != protocol.StatusApplied {
		t.Errorf("status = %q, want applied", res.Status)
	}
	if res.Hash == "" {
		t.Error("result carries no hash")
	}

	got, err := c.Read(ctx, "notes/a.md")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != "# hello\n\nbody\n" {
		t.Errorf("Read = %q", got)
	}
	// And it really is a file on disk, not just an API illusion.
	onDisk, err := v.Read("notes/a.md")
	if err != nil || string(onDisk) != "# hello\n\nbody\n" {
		t.Errorf("vault content = %q, %v", onDisk, err)
	}
}

func TestLiveCompatibilityCheckPassesAgainstOurOwnServer(t *testing.T) {
	c, _ := live(t)
	if err := c.CheckCompatible(context.Background()); err != nil {
		t.Errorf("CheckCompatible against our own server: %v", err)
	}
}

// The whole reason Write captures a base: a second writer must be merged, not
// silently clobbered.
func TestLiveConcurrentWritesToOnePathMerge(t *testing.T) {
	c, _ := live(t)
	ctx := context.Background()

	if _, err := c.Write(ctx, "notes/m.md", []byte("one\ntwo\nthree\n")); err != nil {
		t.Fatal(err)
	}
	// Two writers both capture the same head, then push in turn.
	base, _ := c.Head(ctx)
	h1 := protocol.HashContent([]byte("ONE\ntwo\nthree\n"))
	h2 := protocol.HashContent([]byte("one\ntwo\nTHREE\n"))
	c.PutContent(ctx, h1, []byte("ONE\ntwo\nthree\n"))
	c.PutContent(ctx, h2, []byte("one\ntwo\nTHREE\n"))

	if _, err := c.Push(ctx, base, []protocol.Change{
		{Path: "notes/m.md", Op: protocol.OpPut, Hash: h1}}); err != nil {
		t.Fatal(err)
	}
	resp, err := c.Push(ctx, base, []protocol.Change{
		{Path: "notes/m.md", Op: protocol.OpPut, Hash: h2}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Results[0].Status != protocol.StatusMerged {
		t.Fatalf("status = %q, want merged", resp.Results[0].Status)
	}
	got, _ := c.Read(ctx, "notes/m.md")
	if string(got) != "ONE\ntwo\nTHREE\n" {
		t.Errorf("merged content = %q, want both edits", got)
	}
	// The result must describe what the server holds, not what we sent.
	if resp.Results[0].Hash != protocol.HashContent([]byte("ONE\ntwo\nTHREE\n")) {
		t.Errorf("merged result hash does not describe the stored content")
	}
}

func TestLiveConflictReportsBothSides(t *testing.T) {
	c, _ := live(t)
	ctx := context.Background()
	c.Write(ctx, "notes/c.md", []byte("base\n"))

	base, _ := c.Head(ctx)
	hA := protocol.HashContent([]byte("from A\n"))
	hB := protocol.HashContent([]byte("from B\n"))
	c.PutContent(ctx, hA, []byte("from A\n"))
	c.PutContent(ctx, hB, []byte("from B\n"))

	c.Push(ctx, base, []protocol.Change{{Path: "notes/c.md", Op: protocol.OpPut, Hash: hA}})
	resp, err := c.Push(ctx, base, []protocol.Change{{Path: "notes/c.md", Op: protocol.OpPut, Hash: hB}})
	if err != nil {
		t.Fatal(err)
	}
	r := resp.Results[0]
	if r.Status != protocol.StatusConflict {
		t.Fatalf("status = %q, want conflict", r.Status)
	}
	if r.ConflictPath == "" || r.ConflictHash == "" || r.Hash == "" {
		t.Fatalf("conflict result is not self-describing: %+v", r)
	}
	// Both versions must be fetchable without downloading the whole snapshot.
	kept, _ := c.GetContent(ctx, r.Hash)
	aside, _ := c.GetContent(ctx, r.ConflictHash)
	if string(kept) != "from A\n" {
		t.Errorf("server did not keep its own version: %q", kept)
	}
	// The conflict copy is now the three-way merge with markers, not the bare
	// losing version, so that opening it shows BOTH sides rather than one you
	// then have to diff by hand. Both texts must still be in there.
	if !strings.Contains(string(aside), "from B") {
		t.Errorf("conflict copy lost the client version: %q", aside)
	}
	if !strings.Contains(string(aside), "from A") {
		t.Errorf("conflict copy does not show the server side: %q", aside)
	}
}

func TestLiveDeleteAndList(t *testing.T) {
	c, _ := live(t)
	ctx := context.Background()
	c.Write(ctx, "notes/keep.md", []byte("keep\n"))
	c.Write(ctx, "notes/gone.md", []byte("gone\n"))
	c.Write(ctx, "other/x.md", []byte("x\n"))

	res, err := c.Delete(ctx, "notes/gone.md")
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if res.Status != protocol.StatusApplied {
		t.Errorf("delete status = %q", res.Status)
	}

	all, err := c.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, still := all["notes/gone.md"]; still {
		t.Error("deleted path still listed")
	}
	notes, _ := c.List(ctx, "notes/")
	if len(notes) != 1 {
		t.Errorf("prefix list = %+v, want just notes/keep.md", notes)
	}
}

func TestLiveHistoryAndReadAt(t *testing.T) {
	c, _ := live(t)
	ctx := context.Background()
	c.Write(ctx, "notes/h.md", []byte("first\n"))
	c.Write(ctx, "notes/h.md", []byte("second\n"))

	revs, err := c.History(ctx, "notes/h.md", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(revs) != 2 {
		t.Fatalf("history = %d revisions, want 2", len(revs))
	}
	old, err := c.ReadAt(ctx, revs[1].Short, "notes/h.md")
	if err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if string(old) != "first\n" {
		t.Errorf("ReadAt = %q, want the older version", old)
	}
	// Inspecting must not change the current version.
	cur, _ := c.Read(ctx, "notes/h.md")
	if string(cur) != "second\n" {
		t.Errorf("ReadAt modified the vault: %q", cur)
	}
}

// A path with a space and an accent, which is where URL escaping goes wrong.
func TestLiveAwkwardPathsRoundTrip(t *testing.T) {
	c, _ := live(t)
	ctx := context.Background()
	for _, p := range []string{
		"notes/a note with spaces.md",
		"notes/Kronström.md",
		"notes/deep/nested/folder/file.md",
		"notes/hash#and&query?.md",
	} {
		if _, err := c.Write(ctx, p, []byte("content of "+p)); err != nil {
			t.Errorf("Write(%q): %v", p, err)
			continue
		}
		got, err := c.Read(ctx, p)
		if err != nil {
			t.Errorf("Read(%q): %v", p, err)
			continue
		}
		if string(got) != "content of "+p {
			t.Errorf("Read(%q) = %q", p, got)
		}
	}
}

func TestLiveRejectsAnEscapingPath(t *testing.T) {
	c, _ := live(t)
	_, err := c.Write(context.Background(), "../escape.md", []byte("pwned\n"))
	if err == nil {
		t.Fatal("server accepted a path outside the vault")
	}
	if !client.IsCode(err, protocol.CodeInvalidPath) {
		t.Errorf("err = %v, want invalid_path", err)
	}
}

func TestLiveUnknownBaseIsTyped(t *testing.T) {
	c, _ := live(t)
	_, err := c.Changes(context.Background(), strings.Repeat("a", 40))
	if !client.IsCode(err, protocol.CodeUnknownBase) {
		t.Errorf("err = %v, want unknown_base", err)
	}
}

// The stream is what the webhook fan-out is built on, so it gets a live test.
func TestLiveEventsStream(t *testing.T) {
	c, _ := live(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	events, errs, err := c.Events(ctx)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}

	go func() {
		time.Sleep(200 * time.Millisecond)
		c.Write(context.Background(), "notes/streamed.md", []byte("hello from the stream\n"))
	}()

	for {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatal("stream closed before the event arrived")
			}
			if len(ev.Changes) == 0 {
				continue // the head sent on connect
			}
			ch := ev.Changes[0]
			if ch.Path != "notes/streamed.md" {
				t.Errorf("path = %q", ch.Path)
			}
			// Triage metadata must be present, or a consumer has to fetch to
			// decide whether it cares.
			if ch.Ext != "md" || ch.Kind != protocol.KindText || ch.Size == 0 {
				t.Errorf("event lacks triage metadata: %+v", ch)
			}
			return
		case err := <-errs:
			t.Fatalf("stream error: %v", err)
		case <-ctx.Done():
			t.Fatal("no event within the timeout")
		}
	}
}
