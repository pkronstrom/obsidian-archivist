package reconcile

import (
	"testing"

	"github.com/pkronstrom/obsidian-archivist/internal/repo"
	"github.com/pkronstrom/obsidian-archivist/internal/vault"
	"github.com/pkronstrom/obsidian-archivist/protocol"
)

// newRecWithPolicy is newRec plus the exclusion policy the SERVER installs.
// newRec leaves repo.Open's permissive default in place, which is only correct
// for tests that do not care what may be committed -- these care entirely.
func newRecWithPolicy(t *testing.T) (*Reconciler, *vault.Vault, *repo.Repo) {
	t.Helper()
	rc, v, r := newRec(t)
	r.SetSyncable(func(p string) bool { return !vault.Skip(p) })
	return rc, v, r
}

// The REMOTE write path: a push of an allowlisted config file must land, and a
// push of a refused one must be told so before anything is written.
func TestPushAcceptsAllowlistedConfigAndRefusesTheRest(t *testing.T) {
	rc, v, r := newRecWithPolicy(t)

	allowed := []byte(`{"theme":"obsidian"}`)
	refused := []byte(`{"main":{}}`)
	hAllowed, err := r.WriteBlob(allowed)
	if err != nil {
		t.Fatal(err)
	}
	hRefused, err := r.WriteBlob(refused)
	if err != nil {
		t.Fatal(err)
	}

	_, results, err := rc.Push("", "mac", []Change{
		{Path: ".obsidian/appearance.json", Op: protocol.OpPut, Hash: hAllowed},
		{Path: ".obsidian/workspace.json", Op: protocol.OpPut, Hash: hRefused},
	})
	if err != nil {
		t.Fatalf("push: %v", err)
	}

	byPath := map[string]Result{}
	for _, res := range results {
		byPath[res.Path] = res
	}
	if got := byPath[".obsidian/appearance.json"].Status; got != StatusApplied {
		t.Errorf("appearance.json status = %q, want %q", got, StatusApplied)
	}
	if got := byPath[".obsidian/workspace.json"].Status; got != StatusRefused {
		t.Errorf("workspace.json status = %q, want %q", got, StatusRefused)
	}

	if got, err := v.Read(".obsidian/appearance.json"); err != nil || string(got) != string(allowed) {
		t.Errorf("appearance.json on disk = %q, %v", got, err)
	}
	if _, err := v.Read(".obsidian/workspace.json"); err == nil {
		t.Error("workspace.json was written despite being refused")
	}
}

// The LOCAL write path: a config file edited on the server host must reach
// history through Scan, and a refused one must not.
func TestScanCommitsAllowlistedConfigOnly(t *testing.T) {
	rc, v, r := newRecWithPolicy(t)

	if err := v.Write(".obsidian/snippets/dark.css", []byte("body{}\n")); err != nil {
		t.Fatal(err)
	}
	if err := v.Write(".obsidian/workspace.json", []byte(`{"left":{}}`)); err != nil {
		t.Fatal(err)
	}

	head, err := rc.Scan("local config edit")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	snap, err := r.Snapshot(head)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := snap[".obsidian/snippets/dark.css"]; !ok {
		t.Error("a locally-edited snippet never reached history")
	}
	if _, ok := snap[".obsidian/workspace.json"]; ok {
		t.Error("workspace.json reached history through the local path")
	}
}
