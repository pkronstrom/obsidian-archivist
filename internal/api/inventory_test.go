package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/pkronstrom/obsidian-archivist/internal/reconcile"
	"github.com/pkronstrom/obsidian-archivist/internal/repo"
	"github.com/pkronstrom/obsidian-archivist/internal/vault"
	"github.com/pkronstrom/obsidian-archivist/protocol"
)

func TestInventoryTransportAcrossDevices(t *testing.T) {
	h, _, r := newServer(t)
	path := ".archivist/plugin-inventory/01234567-89ab-cdef-0123-456789abcdef.json"
	content := []byte(`{"schemaVersion":1,"installationId":"01234567-89ab-cdef-0123-456789abcdef","deviceName":"Mac","platform":"desktop","updatedAt":"2026-09-09T12:00:00Z","plugins":[]}`)
	hash, err := repo.HashContent(content)
	if err != nil {
		t.Fatal(err)
	}
	putContent(t, h, hash, content)
	w := do(t, h, "POST", "/v1/push", protocol.PushRequest{
		Device: "mac", Changes: []reconcile.Change{{Path: path, Op: "put", Hash: hash}},
	}, true)
	var pushed protocol.PushResponse
	if w.Code != http.StatusOK {
		t.Fatalf("push: %d %s", w.Code, w.Body)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &pushed); err != nil {
		t.Fatal(err)
	}
	if len(pushed.Results) != 1 || pushed.Results[0].Status != reconcile.StatusApplied {
		t.Fatalf("results: %+v", pushed)
	}
	// Verify the actual Git tree, not merely the push acknowledgement.
	if got, err := r.ReadAt(pushed.Head, path); err != nil || string(got) != string(content) {
		t.Fatalf("commit: %q %v", got, err)
	}
	// A second device starts at an empty base and fetches the advertised blob.
	w = do(t, h, "GET", "/v1/changes?since=", nil, true)
	var changes protocol.ChangesResponse
	if w.Code != http.StatusOK {
		t.Fatalf("changes: %d %s", w.Code, w.Body)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &changes); err != nil {
		t.Fatal(err)
	}
	if len(changes.Entries) != 1 || changes.Entries[0].Path != path || changes.Entries[0].Hash != hash {
		t.Fatalf("changes: %+v", changes)
	}
	w = do(t, h, "GET", "/v1/content/"+changes.Entries[0].Hash, nil, true)
	if w.Code != http.StatusOK || string(w.Body.Bytes()) != string(content) {
		t.Fatalf("download: %d %s", w.Code, w.Body)
	}
}

func TestLiveSyncFiltersLegacyConfigButHistoryRemains(t *testing.T) {
	h, v, r := newServer(t)
	config := ".obsidian/appearance.json"
	old := []byte(`{"theme":"old"}`)
	// Reproduce a repository written by the former config-enabled server.
	r.SetSyncable(func(string) bool { return true })
	if err := v.Write(config, old); err != nil {
		t.Fatal(err)
	}
	if err := v.Write("note.md", []byte("note")); err != nil {
		t.Fatal(err)
	}
	legacy, err := r.Commit("legacy config")
	if err != nil {
		t.Fatal(err)
	}
	r.SetSyncable(func(p string) bool { return !vault.Skip(p) })
	w := do(t, h, "GET", "/v1/snapshot", nil, true)
	var snap protocol.SnapshotResponse
	if w.Code != http.StatusOK {
		t.Fatalf("snapshot: %d %s", w.Code, w.Body)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	if _, ok := snap.Files[config]; ok {
		t.Error("snapshot advertises live config")
	}
	if _, ok := snap.Files["note.md"]; !ok {
		t.Error("snapshot lost note")
	}
	w = do(t, h, "GET", "/v1/changes?since=", nil, true)
	var changes protocol.ChangesResponse
	if w.Code != http.StatusOK {
		t.Fatalf("changes: %d %s", w.Code, w.Body)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &changes); err != nil {
		t.Fatal(err)
	}
	if len(changes.Entries) != 1 || changes.Entries[0].Path != "note.md" {
		t.Errorf("changes advertise legacy config: %+v", changes)
	}
	// Low-level history and its content transport stay readable.
	if got, err := r.ReadAt(legacy, config); err != nil || string(got) != string(old) {
		t.Fatalf("history lost: %q %v", got, err)
	}
	hash, err := repo.HashContent(old)
	if err != nil {
		t.Fatal(err)
	}
	w = do(t, h, "GET", "/v1/content/"+hash, nil, true)
	if w.Code != http.StatusOK || string(w.Body.Bytes()) != string(old) {
		t.Fatalf("historical content: %d %s", w.Code, w.Body)
	}
	// A deletion already recorded by an older server must also be filtered.
	r.SetSyncable(func(string) bool { return true })
	if err := v.Remove(config); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit("legacy deletion"); err != nil {
		t.Fatal(err)
	}
	r.SetSyncable(func(p string) bool { return !vault.Skip(p) })
	w = do(t, h, "GET", "/v1/changes?since="+legacy, nil, true)
	if err := json.Unmarshal(w.Body.Bytes(), &changes); err != nil {
		t.Fatal(err)
	}
	if len(changes.Entries) != 0 {
		t.Errorf("legacy deletion advertised: %+v", changes)
	}
}
