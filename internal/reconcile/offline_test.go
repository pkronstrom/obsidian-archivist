package reconcile

import (
	"path/filepath"
	"testing"

	"github.com/pkronstrom/vaultsync/internal/repo"
	"github.com/pkronstrom/vaultsync/internal/vault"
)

// iOS can kill the app between "push sent" and "response recorded". On the next
// launch the client still holds the OLD base, recomputes the same diff, and
// pushes it again. That replay must be a no-op -- not a conflict, not a
// duplicate file.
func TestReplayingAnAlreadyAppliedPushIsANoOp(t *testing.T) {
	base := t.TempDir()
	work := filepath.Join(base, "vault")
	v, _ := vault.New(work)
	defer v.Close()
	r, _ := repo.Open(work, filepath.Join(base, "git"))
	rc := New(v, r)

	h0, _ := r.WriteBlob([]byte("original\n"))
	b0, _, _ := rc.Push("", "phone", []Change{{Path: "a.md", Op: "put", Hash: h0}})

	// The phone edits offline and pushes; the response is lost to a suspend.
	h1, _ := r.WriteBlob([]byte("edited offline\n"))
	afterFirst, res1, err := rc.Push(b0, "phone", []Change{{Path: "a.md", Op: "put", Hash: h1}})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("first push:  status=%s head=%s", res1[0].Status, afterFirst[:8])

	// Relaunch: same stale base, same recomputed change set.
	afterReplay, res2, err := rc.Push(b0, "phone", []Change{{Path: "a.md", Op: "put", Hash: h1}})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("replayed:    status=%s head=%s", res2[0].Status, afterReplay[:8])

	if res2[0].Status == StatusConflict {
		t.Errorf("replay produced a conflict: %+v", res2[0])
	}
	if res2[0].ConflictPath != "" {
		t.Errorf("replay created a conflict file: %s", res2[0].ConflictPath)
	}
	if afterReplay != afterFirst {
		t.Errorf("replay created a new commit %s (want unchanged %s)", afterReplay[:8], afterFirst[:8])
	}
	got, _ := v.Read("a.md")
	if string(got) != "edited offline\n" {
		t.Errorf("content = %q", got)
	}
	snap, _ := r.Snapshot(afterReplay)
	if len(snap) != 1 {
		t.Errorf("tree has %d files, want 1: %+v", len(snap), snap)
	}
}

// A long offline period on one device while another device also edits: the
// classic case. Different files must not conflict.
func TestTwoDevicesOfflineOnDifferentFiles(t *testing.T) {
	base := t.TempDir()
	work := filepath.Join(base, "vault")
	v, _ := vault.New(work)
	defer v.Close()
	r, _ := repo.Open(work, filepath.Join(base, "git"))
	rc := New(v, r)

	ha, _ := r.WriteBlob([]byte("a\n"))
	hb, _ := r.WriteBlob([]byte("b\n"))
	shared, _, _ := rc.Push("", "seed", []Change{
		{Path: "a.md", Op: "put", Hash: ha},
		{Path: "b.md", Op: "put", Hash: hb},
	})

	// Mac edits a.md and syncs.
	ha2, _ := r.WriteBlob([]byte("a from mac\n"))
	rc.Push(shared, "mac", []Change{{Path: "a.md", Op: "put", Hash: ha2}})

	// Phone was offline the whole time; it edited b.md and pushes from the old base.
	hb2, _ := r.WriteBlob([]byte("b from phone\n"))
	_, res, err := rc.Push(shared, "phone", []Change{{Path: "b.md", Op: "put", Hash: hb2}})
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Status != StatusApplied {
		t.Errorf("status = %s, want applied (different files never conflict)", res[0].Status)
	}
	a, _ := v.Read("a.md")
	b, _ := v.Read("b.md")
	if string(a) != "a from mac\n" || string(b) != "b from phone\n" {
		t.Errorf("a=%q b=%q, want both edits preserved", a, b)
	}
}
