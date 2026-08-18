package reconcile

import (
	"encoding/json"
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

// Two devices editing different keys of the same settings file must both keep
// their edit, and must not produce a conflict file.
func TestConfigJSONMergesByKey(t *testing.T) {
	rc, v, r := newRecWithPolicy(t)

	base := []byte("{\n  \"theme\": \"obsidian\",\n  \"fontSize\": 16\n}\n")
	if err := v.Write(".obsidian/appearance.json", base); err != nil {
		t.Fatal(err)
	}
	baseHead, err := rc.Scan("seed")
	if err != nil {
		t.Fatal(err)
	}

	// The server moves on: fontSize changes here.
	if err := v.Write(".obsidian/appearance.json",
		[]byte("{\n  \"theme\": \"obsidian\",\n  \"fontSize\": 18\n}\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := rc.Scan("server edit"); err != nil {
		t.Fatal(err)
	}

	// The client pushes a theme change computed from the older base.
	theirs := []byte("{\n  \"theme\": \"minimal\",\n  \"fontSize\": 16\n}\n")
	h, err := r.WriteBlob(theirs)
	if err != nil {
		t.Fatal(err)
	}
	_, results, err := rc.Push(baseHead, "mac", []Change{
		{Path: ".obsidian/appearance.json", Op: protocol.OpPut, Hash: h},
	})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Status != StatusMerged {
		t.Fatalf("status = %q, want %q", results[0].Status, StatusMerged)
	}

	got, err := v.Read(".obsidian/appearance.json")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("merged config is not valid JSON: %v\n%s", err, got)
	}
	if m["theme"] != "minimal" {
		t.Errorf("theme = %v, want minimal (the client's edit was lost)", m["theme"])
	}
	if m["fontSize"] != float64(18) {
		t.Errorf("fontSize = %v, want 18 (the server's edit was lost)", m["fontSize"])
	}
}

// Same key, different values: the last writer wins AND a conflict copy is kept.
// A settings file has no way to carry conflict markers, so the copy is the only
// record that a choice was made.
func TestConfigJSONConflictKeepsACopy(t *testing.T) {
	rc, v, r := newRecWithPolicy(t)

	if err := v.Write(".obsidian/appearance.json",
		[]byte("{\n  \"theme\": \"obsidian\"\n}\n")); err != nil {
		t.Fatal(err)
	}
	baseHead, err := rc.Scan("seed")
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Write(".obsidian/appearance.json",
		[]byte("{\n  \"theme\": \"things\"\n}\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := rc.Scan("server edit"); err != nil {
		t.Fatal(err)
	}

	theirs := []byte("{\n  \"theme\": \"minimal\"\n}\n")
	h, err := r.WriteBlob(theirs)
	if err != nil {
		t.Fatal(err)
	}
	_, results, err := rc.Push(baseHead, "mac", []Change{
		{Path: ".obsidian/appearance.json", Op: protocol.OpPut, Hash: h},
	})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Status != StatusConflict {
		t.Fatalf("status = %q, want %q", results[0].Status, StatusConflict)
	}
	if results[0].ConflictPath == "" {
		t.Fatal("no conflict copy was recorded")
	}

	got, err := v.Read(".obsidian/appearance.json")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("the winning file must be valid JSON Obsidian can read: %v", err)
	}
	if m["theme"] != "minimal" {
		t.Errorf("theme = %v, want minimal (the last writer)", m["theme"])
	}

	kept, err := v.Read(results[0].ConflictPath)
	if err != nil {
		t.Fatalf("reading the conflict copy: %v", err)
	}
	var km map[string]any
	if err := json.Unmarshal(kept, &km); err != nil {
		t.Fatalf("the conflict copy must be valid JSON, not a fenced merge: %v\n%s", err, kept)
	}
	if km["theme"] != "things" {
		t.Errorf("conflict copy theme = %v, want things (the server's version)", km["theme"])
	}
}

// A CSS snippet is ordinary text and keeps the ordinary text merge.
func TestConfigCSSStillTextMerges(t *testing.T) {
	rc, v, r := newRecWithPolicy(t)

	if err := v.Write(".obsidian/snippets/dark.css", []byte("a\nb\nc\n")); err != nil {
		t.Fatal(err)
	}
	baseHead, err := rc.Scan("seed")
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Write(".obsidian/snippets/dark.css", []byte("A\nb\nc\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := rc.Scan("server edit"); err != nil {
		t.Fatal(err)
	}

	h, err := r.WriteBlob([]byte("a\nb\nC\n"))
	if err != nil {
		t.Fatal(err)
	}
	_, results, err := rc.Push(baseHead, "mac", []Change{
		{Path: ".obsidian/snippets/dark.css", Op: protocol.OpPut, Hash: h},
	})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Status != StatusMerged {
		t.Fatalf("status = %q, want %q", results[0].Status, StatusMerged)
	}
	got, err := v.Read(".obsidian/snippets/dark.css")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "A\nb\nC\n" {
		t.Errorf("merged css = %q, want %q", got, "A\nb\nC\n")
	}
}
