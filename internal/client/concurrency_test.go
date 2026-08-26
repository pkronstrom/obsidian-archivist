package client_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"

	"github.com/pkronstrom/obsidian-archivist/internal/client"
	"testing"

	"github.com/pkronstrom/obsidian-archivist/protocol"
)

const doc = "line1\nline2\nline3\nline4\nline5\n"

// interleave performs the read-modify-write an agent actually does: read, then
// another writer commits, then write back the edit derived from the read.
func interleave(t *testing.T, theirs, mine string, useBase bool) (protocol.Result, string, *client.Client) {
	t.Helper()
	c, _ := live(t)
	ctx := context.Background()
	c.Write(ctx, "n.md", []byte(doc))

	seen, base, err := c.ReadForEdit(ctx, "n.md")
	if err != nil {
		t.Fatal(err)
	}
	if string(seen) != doc {
		t.Fatalf("read %q", seen)
	}

	// Another device syncs a change while the caller is thinking.
	h := protocol.HashContent([]byte(theirs))
	c.PutContent(ctx, h, []byte(theirs))
	head, _ := c.Head(ctx)
	c.Push(ctx, head, []protocol.Change{{Path: "n.md", Op: protocol.OpPut, Hash: h}})

	var res protocol.Result
	if useBase {
		res, err = c.WriteAt(ctx, "n.md", []byte(mine), base)
	} else {
		res, err = c.Write(ctx, "n.md", []byte(mine))
	}
	if err != nil {
		t.Fatal(err)
	}
	final, _ := c.Read(ctx, "n.md")
	return res, string(final), c
}

// The bug this exists for: a relay/MCP caller read a note, edited it, and its
// write silently discarded a concurrent change, because Write captured head
// AFTER the read. Carrying the base makes the server merge the two.
func TestWriteAtMergesDisjointConcurrentEdits(t *testing.T) {
	theirs := strings.Replace(doc, "line5", "line5 THEIRS", 1)
	mine := strings.Replace(doc, "line1", "line1 MINE", 1)

	res, final, _ := interleave(t, theirs, mine, true)
	if !strings.Contains(final, "THEIRS") {
		t.Errorf("LOST UPDATE: the other writer's edit is gone. status=%q final=%q", res.Status, final)
	}
	if !strings.Contains(final, "MINE") {
		t.Errorf("the caller's edit is gone. status=%q final=%q", res.Status, final)
	}
	if res.Status != protocol.StatusMerged {
		t.Errorf("status = %q, want merged", res.Status)
	}
}

func TestWriteAtWithRevisionReturnsPushHeadAndPreservesWriteAtSignature(t *testing.T) {
	c, _ := live(t)
	ctx := context.Background()
	if _, err := c.Write(ctx, "revision.md", []byte("before\n")); err != nil {
		t.Fatal(err)
	}
	base, err := c.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}

	result, revision, err := c.WriteAtWithRevision(ctx, "revision.md", []byte("after\n"), base)
	if err != nil {
		t.Fatal(err)
	}
	head, err := c.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if revision == "" || revision != head {
		t.Errorf("returned revision = %q, want PushResponse.Head %q", revision, head)
	}
	if result.Status != protocol.StatusApplied || result.Hash != protocol.HashContent([]byte("after\n")) {
		t.Errorf("result = %+v, want applied result for stored bytes", result)
	}

	// Keep the old two-result call compiling and behaving as before. Existing
	// callers need not opt in to the repository revision.
	legacyResult, err := c.WriteAt(ctx, "legacy.md", []byte("still works\n"), "")
	if err != nil {
		t.Fatal(err)
	}
	if legacyResult.Status != protocol.StatusApplied {
		t.Errorf("legacy WriteAt status = %q, want applied", legacyResult.Status)
	}
}

// Overlapping edits cannot merge, but nothing may be lost: the server keeps its
// version and stores the caller's alongside.
func TestWriteAtConflictKeepsBothVersions(t *testing.T) {
	theirs := strings.Replace(doc, "line3", "line3 THEIRS", 1)
	mine := strings.Replace(doc, "line3", "line3 MINE", 1)

	res, final, c := interleave(t, theirs, mine, true)
	if res.Status != protocol.StatusConflict {
		t.Fatalf("status = %q, want conflict", res.Status)
	}
	if !strings.Contains(final, "THEIRS") {
		t.Errorf("the other writer's edit is gone: %q", final)
	}
	if res.ConflictPath == "" || res.ConflictHash == "" {
		t.Fatalf("conflict is not self-describing: %+v", res)
	}
	aside, err := c.GetContent(context.Background(), res.ConflictHash)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(aside), "MINE") {
		t.Errorf("the caller's version was not preserved aside: %q", aside)
	}
}

// Write without a base is documented as a blind overwrite. Pinned here so the
// difference between the two is deliberate and visible.
func TestWriteWithoutABaseIsABlindOverwrite(t *testing.T) {
	theirs := strings.Replace(doc, "line5", "line5 THEIRS", 1)
	mine := strings.Replace(doc, "line1", "line1 MINE", 1)

	res, final, _ := interleave(t, theirs, mine, false)
	if strings.Contains(final, "THEIRS") {
		t.Errorf("Write merged; its doc comment claims it does not: %q", final)
	}
	if res.Status != protocol.StatusApplied {
		t.Errorf("status = %q, want applied", res.Status)
	}
}

// A relay may start while the server is down, then reach a server speaking a
// protocol it does not. Writes must be refused rather than sent to a server
// that may read the fields differently -- a startup-only check misses this
// entirely, and both review models flagged it independently.
func TestPushIsRefusedWhenTheServerProtocolDiffers(t *testing.T) {
	var proto atomic.Int64
	proto.Store(int64(protocol.Version))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/push" {
			t.Errorf("push reached a server speaking an incompatible protocol")
			w.WriteHeader(500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"protocol": proto.Load(), "version": "test", "service": "archivist-server",
		})
	}))
	defer srv.Close()

	c := client.New(srv.URL, "tok", "relay")
	ctx := context.Background()

	// Confirmed compatible: a push is allowed through (and reaches the handler,
	// which is why the handler fails the test only for the mismatched case).
	if err := c.CheckCompatible(ctx); err != nil {
		t.Fatalf("baseline: %v", err)
	}

	// The server is redeployed speaking something else.
	proto.Store(int64(protocol.Version + 1))
	if err := c.CheckCompatible(ctx); err == nil {
		t.Fatal("CheckCompatible accepted a different protocol")
	}

	_, err := c.Push(ctx, "abc", []protocol.Change{{Path: "n.md", Op: protocol.OpPut, Hash: "x"}})
	if err == nil {
		t.Fatal("Push was allowed against an incompatible server")
	}
	if !strings.Contains(err.Error(), "protocol") {
		t.Errorf("err = %v, want a protocol mismatch", err)
	}
}

// The gate must not fire on a server that is simply down: unreachable is not
// incompatible, and the call has to fail on its own terms.
func TestPushIsNotBlockedByAnUnreachableServer(t *testing.T) {
	c := client.New("http://127.0.0.1:1", "tok", "relay")
	_, err := c.Push(context.Background(), "abc", nil)
	if err == nil {
		t.Fatal("expected a network error")
	}
	if strings.Contains(err.Error(), "speaks protocol") {
		t.Errorf("an unreachable server was reported as a protocol mismatch: %v", err)
	}
}
