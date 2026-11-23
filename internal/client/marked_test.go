package client

import (
	"context"
	"net/http"
	"testing"

	"github.com/pkronstrom/obsidian-archivist/protocol"
)

// A marked path holds ciphertext. The refusal has to come before any request:
// a write uploads its content first, and by the time the server refused the
// push the plaintext would already be in its object store.
func TestMarkedPathIsRefusedBeforeAnyRequest(t *testing.T) {
	var calls []string
	c := stub(t, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		writeJSON(w, map[string]any{})
	})
	ctx := context.Background()
	const secret = "Clients.encrypted/mfrgg"

	attempts := map[string]func() error{
		"read":    func() error { _, err := c.Read(ctx, secret); return err },
		"read at": func() error { _, err := c.ReadAt(ctx, "abc", "Salary.encrypted.md", 1<<20); return err },
		"edit":    func() error { _, _, err := c.ReadForEdit(ctx, secret, 1<<20); return err },
		"history": func() error { _, err := c.History(ctx, secret, 10); return err },
		"write":   func() error { _, err := c.WriteAt(ctx, secret, []byte("plaintext"), "abc"); return err },
		"delete":  func() error { _, err := c.DeleteAt(ctx, secret, "abc"); return err },
		"move in": func() error { _, err := c.MoveAt(ctx, "Note.md", "Note.encrypted.md", "abc"); return err },
		"move out": func() error {
			_, err := c.MoveAt(ctx, "Salary.encrypted.md", "Salary.md", "abc")
			return err
		},
		"append": func() error {
			_, err := c.AppendNote(ctx, protocol.AppendNoteRequest{Path: secret, Content: "x"})
			return err
		},
		"edit note": func() error {
			_, err := c.EditNote(ctx, protocol.EditNoteRequest{Path: secret, OldText: "a", NewText: "b"})
			return err
		},
	}
	for name, attempt := range attempts {
		if err := attempt(); !IsCode(err, protocol.CodeEncryptedPath) {
			t.Errorf("%s: err = %v, want IsCode(encrypted_path)", name, err)
		}
	}
	for _, call := range calls {
		// Reading for an edit asks for the head first, which names no path.
		if call != "GET /v1/head" {
			t.Errorf("a refused call still reached the server: %s", call)
		}
	}
}

// Every caller of List goes on to read, search or offer what it returns.
func TestListLeavesMarkedPathsOut(t *testing.T) {
	c := stub(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, protocol.SnapshotResponse{Head: "abc", Files: map[string]protocol.Entry{
			"Note.md":                    {Hash: "1"},
			"Work/Plan.md":               {Hash: "2"},
			"Salary.encrypted.md":        {Hash: "3"},
			"Work/Clients.encrypted/mfz": {Hash: "4"},
		}})
	})
	all, err := c.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all["Note.md"].Hash != "1" || all["Work/Plan.md"].Hash != "2" {
		t.Errorf("want the two ordinary notes, got %v", all)
	}
	work, err := c.List(context.Background(), "Work/")
	if err != nil {
		t.Fatal(err)
	}
	if len(work) != 1 || work["Work/Plan.md"].Hash != "2" {
		t.Errorf("want the one ordinary note under Work/, got %v", work)
	}
}
