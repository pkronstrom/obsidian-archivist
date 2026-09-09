package reconcile

import (
	"github.com/pkronstrom/obsidian-archivist/internal/repo"
	"github.com/pkronstrom/obsidian-archivist/internal/vault"
	"github.com/pkronstrom/obsidian-archivist/protocol"
	"testing"
)

func newRecWithPolicy(t *testing.T) (*Reconciler, *vault.Vault, *repo.Repo) {
	t.Helper()
	rc, v, r := newRec(t)
	r.SetSyncable(func(p string) bool { return !vault.Skip(p) })
	return rc, v, r
}

func TestLegacyConfigOperationsRefusedAndHistoryPreserved(t *testing.T) {
	for _, op := range []string{"put", "del", "move-from", "move-to"} {
		t.Run(op, func(t *testing.T) {
			rc, v, r := newRec(t)
			config := ".obsidian/appearance.json"
			old := `{"theme":"old"}`
			if err := v.Write(config, []byte(old)); err != nil {
				t.Fatal(err)
			}
			if err := v.Write("source.md", []byte("source")); err != nil {
				t.Fatal(err)
			}
			base, err := rc.Scan("legacy config history")
			if err != nil {
				t.Fatal(err)
			}
			r.SetSyncable(func(p string) bool { return !vault.Skip(p) })
			ch := put(t, r, config, "replacement")
			switch op {
			case "del":
				ch = Change{Path: config, Op: protocol.OpDel}
			case "move-from":
				ch = Change{Path: "leaked.md", From: config, Op: protocol.OpMove}
			case "move-to":
				ch = Change{Path: ".obsidian/hotkeys.json", From: "source.md", Op: protocol.OpMove}
			}
			head, results, err := rc.Push(base, "legacy-client", []Change{ch, put(t, r, "note.md", "notes continue")})
			if err != nil {
				t.Fatal(err)
			}
			if results[0].Status != StatusRefused || results[1].Status != StatusApplied {
				t.Fatalf("results = %+v", results)
			}
			for _, rev := range []string{base, head} {
				got, err := r.ReadAt(rev, config)
				if err != nil || string(got) != old {
					t.Fatalf("history %s = %q, %v", rev, got, err)
				}
			}
			got, err := v.Read(config)
			if err != nil || string(got) != old {
				t.Fatalf("disk config = %q, %v", got, err)
			}
			if got, err := v.Read("source.md"); err != nil || string(got) != "source" {
				t.Fatalf("move source changed: %q %v", got, err)
			}
			if _, err := v.Read(".obsidian/hotkeys.json"); err == nil {
				t.Fatal("move destination written")
			}
			if _, err := v.Read("leaked.md"); err == nil {
				t.Fatal("config source leaked")
			}
			if err := v.Write(config, []byte("local config edit")); err != nil {
				t.Fatal(err)
			}
			after, err := rc.Scan("local config ignored")
			if err != nil || after != head {
				t.Fatalf("local config created commit: %s %v", after, err)
			}
		})
	}
}

const inventoryPath = ".archivist/plugin-inventory/01234567-89ab-cdef-0123-456789abcdef.json"

func TestInventoryCommitAndWholeFileLastWriterWins(t *testing.T) {
	rc, v, r := newRecWithPolicy(t)
	base, results, err := rc.Push("", "mac", []Change{put(t, r, inventoryPath, `{
 "name": "old",
 "plugins": []
}
`)})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Status != StatusApplied {
		t.Fatalf("results = %+v", results)
	}
	snap, err := r.Snapshot(base)
	if err != nil || snap[inventoryPath].Hash == "" {
		t.Fatalf("inventory absent from commit: %+v %v", snap, err)
	}
	ours := `{
 "name": "server",
 "plugins": []
}
`
	prior, _, err := rc.Push(base, "mac", []Change{put(t, r, inventoryPath, ours)})
	if err != nil {
		t.Fatal(err)
	}
	theirs := `{
 "name": "old",
 "plugins": ["plugin"]
}
`
	head, results, err := rc.Push(base, "restored-mac", []Change{put(t, r, inventoryPath, theirs)})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Status != StatusApplied {
		t.Fatalf("inventory merged or conflicted: %+v", results)
	}
	if got, err := v.Read(inventoryPath); err != nil || string(got) != theirs {
		t.Fatalf("inventory combined devices: %q %v", got, err)
	}
	if got, err := r.ReadAt(prior, inventoryPath); err != nil || string(got) != ours {
		t.Fatalf("prior lost: %q %v", got, err)
	}
	next, _, err := rc.Push(head, "mac", []Change{put(t, r, inventoryPath, theirs)})
	if err != nil || next != head {
		t.Fatalf("unchanged inventory committed: %s %v", next, err)
	}
}

// A malformed prior upload must not trap an installation in an unavailable
// state when it subsequently publishes a valid inventory from a stale base.
func TestInventoryCanReplaceMalformedBinaryContent(t *testing.T) {
	rc, _, r := newRecWithPolicy(t)
	base, _, err := rc.Push("", "mac", []Change{put(t, r, inventoryPath, "{}")})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := rc.Push(base, "mac", []Change{put(t, r, inventoryPath, "bad\x00inventory")}); err != nil {
		t.Fatal(err)
	}
	head, results, err := rc.Push(base, "mac", []Change{put(t, r, inventoryPath, `{"plugins":[]}`)})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Status != StatusApplied {
		t.Fatalf("repair refused by merge resolution: %+v", results)
	}
	if got, err := r.ReadAt(head, inventoryPath); err != nil || string(got) != `{"plugins":[]}` {
		t.Fatalf("repair: %q %v", got, err)
	}
}
