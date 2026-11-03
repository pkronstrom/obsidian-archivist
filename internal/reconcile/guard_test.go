package reconcile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pkronstrom/obsidian-archivist/internal/guard"
	"github.com/pkronstrom/obsidian-archivist/protocol"
)

func tightGuard(writes int, now *time.Time) *guard.Guard {
	return guard.New(guard.Limits{
		Writes:   writes,
		Window:   5 * time.Minute,
		Cooldown: 15 * time.Minute,
	}, func() time.Time { return *now }, nil)
}

func TestPushRefusesQuarantinedPath(t *testing.T) {
	rc, _, r := newRec(t)
	now := time.Unix(1000, 0)
	rc.SetGuard(tightGuard(2, &now))

	// Writes=2 allows two writes; the third crosses the threshold and is
	// refused. Content must differ each time or the commit is a no-op and
	// nothing is counted.
	for i := 0; i < 2; i++ {
		if _, _, err := rc.Push("", "mac", []Change{put(t, r, "a.md", "v"+string(rune('a'+i)))}); err != nil {
			t.Fatalf("push %d: %v", i, err)
		}
		now = now.Add(time.Second)
	}

	_, results, err := rc.Push("", "mac", []Change{put(t, r, "a.md", "third")})
	if err != nil {
		t.Fatalf("a quarantine refused the whole push: %v", err)
	}
	if len(results) != 1 || results[0].Status != StatusRefused {
		t.Fatalf("results = %+v, want a.md refused", results)
	}
}

func TestPushToOtherPathsStillWorksWhileOneIsQuarantined(t *testing.T) {
	rc, _, r := newRec(t)
	now := time.Unix(1000, 0)
	rc.SetGuard(tightGuard(2, &now))

	for i := 0; i < 4; i++ {
		rc.Push("", "mac", []Change{put(t, r, "hot.md", "v"+string(rune('a'+i)))})
		now = now.Add(time.Second)
	}
	if _, _, err := rc.Push("", "mac", []Change{put(t, r, "cold.md", "v")}); err != nil {
		t.Fatalf("an unrelated path was blocked: %v", err)
	}
}

// A quarantined path is refused alone. The rest of its batch lands: a client
// pushes every pending change at once, so refusing the batch let one stuck
// attachment block every other edit from that device -- seen live, 2026-11-03.
func TestQuarantinedPathIsRefusedAloneInABatch(t *testing.T) {
	rc, v, r := newRec(t)
	now := time.Unix(1000, 0)
	rc.SetGuard(tightGuard(2, &now))

	for i := 0; i < 4; i++ {
		rc.Push("", "mac", []Change{put(t, r, "hot.md", "v"+string(rune('a'+i)))})
		now = now.Add(time.Second)
	}

	_, results, err := rc.Push("", "mac", []Change{
		put(t, r, "cold.md", "lands"),
		put(t, r, "hot.md", "blocked"),
	})
	if err != nil {
		t.Fatalf("the batch was refused whole: %v", err)
	}
	status := map[string]string{}
	for _, res := range results {
		status[res.Path] = res.Status
	}
	if status["cold.md"] != StatusApplied || status["hot.md"] != StatusRefused {
		t.Fatalf("statuses = %v, want cold.md applied and hot.md refused", status)
	}
	if b, _ := os.ReadFile(filepath.Join(v.Dir(), "hot.md")); string(b) == "blocked" {
		t.Fatal("the quarantined path was written")
	}
	if b, _ := os.ReadFile(filepath.Join(v.Dir(), "cold.md")); string(b) != "lands" {
		t.Fatalf("cold.md = %q, want it applied", b)
	}
}

// The vault-wide throttle is not about one path, so it still refuses the
// whole push and leaves the tree untouched.
func TestThrottledPushAppliesNothing(t *testing.T) {
	rc, v, r := newRec(t)
	now := time.Unix(1000, 0)
	rc.SetGuard(guard.New(guard.Limits{TotalBytes: 3, Window: 5 * time.Minute}, func() time.Time { return now }, nil))

	_, _, err := rc.Push("", "mac", []Change{
		put(t, r, "a.md", "aa"),
		put(t, r, "b.md", "bb"),
	})
	var pe *protocol.Error
	if !errors.As(err, &pe) || pe.Code != protocol.CodeThrottled {
		t.Fatalf("err = %v, want a throttle", err)
	}
	if _, statErr := os.Stat(filepath.Join(v.Dir(), "a.md")); statErr == nil {
		t.Fatal("a throttled push wrote part of its batch")
	}
}

// Scan is the local path. It must consult the guard, and it must never fail:
// the bytes are already on disk. A test that only exercised Push would have
// passed against the draft that left this path unguarded.
func TestScanRecordsWritesInTheGuard(t *testing.T) {
	rc, v, _ := newRec(t)
	now := time.Unix(1000, 0)
	g := tightGuard(1, &now)
	rc.SetGuard(g)

	// Content must differ each round. Rewriting identical bytes produces no
	// commit, so there is nothing for the guard to count -- which is the
	// "only changed content counts" rule working, not a missed write.
	for i := 0; i < 3; i++ {
		body := []byte("edit " + string(rune('a'+i)))
		if err := os.WriteFile(filepath.Join(v.Dir(), "local.md"), body, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := rc.Scan("local edit"); err != nil {
			t.Fatalf("Scan must never fail on the local path: %v", err)
		}
		now = now.Add(time.Second)
	}

	if !g.Pressure() {
		t.Fatal("local writes did not register with the guard")
	}
}

// Even with every threshold tripped, Scan keeps committing. Refusing here
// would diverge the vault from its history without reclaiming a byte.
func TestScanNeverRefusesUnderPressure(t *testing.T) {
	rc, v, _ := newRec(t)
	now := time.Unix(1000, 0)
	rc.SetGuard(tightGuard(1, &now))

	for i := 0; i < 5; i++ {
		if err := os.WriteFile(filepath.Join(v.Dir(), "loop.md"), []byte(string(rune('a'+i))), 0o644); err != nil {
			t.Fatal(err)
		}
		head, err := rc.Scan("local edit")
		if err != nil {
			t.Fatalf("Scan %d failed: %v", i, err)
		}
		if head == "" {
			t.Fatalf("Scan %d produced no commit", i)
		}
		now = now.Add(time.Second)
	}
}

// A nil guard disables everything, which is what the CLI and existing tests
// rely on.
func TestNilGuardAllowsEverything(t *testing.T) {
	rc, _, r := newRec(t)
	for i := 0; i < 50; i++ {
		if _, _, err := rc.Push("", "mac", []Change{put(t, r, "a.md", "v"+string(rune('a'+i)))}); err != nil {
			t.Fatalf("push %d with no guard: %v", i, err)
		}
	}
}
