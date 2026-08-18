package guard

import (
	"errors"
	"testing"
	"time"
)

// clock returns a now-function whose value the test controls.
func clock(t0 *time.Time) func() time.Time {
	return func() time.Time { return *t0 }
}

func plentyFree() FreeFunc { return func() (int64, error) { return 1 << 40, nil } }

func testLimits() Limits {
	return Limits{
		Writes:   3,
		Window:   5 * time.Minute,
		Cooldown: 15 * time.Minute,
	}
}

func TestUnderWriteThresholdAllows(t *testing.T) {
	now := time.Unix(1000, 0)
	g := New(testLimits(), clock(&now), plentyFree())

	for i := 0; i < 3; i++ {
		if v := g.Admit("a.md", 10); !v.OK() {
			t.Fatalf("write %d refused: %s", i, v.Code)
		}
		now = now.Add(time.Second)
	}
}

func TestOverWriteThresholdQuarantines(t *testing.T) {
	now := time.Unix(1000, 0)
	g := New(testLimits(), clock(&now), plentyFree())

	for i := 0; i < 4; i++ {
		g.Admit("a.md", 10)
		now = now.Add(time.Second)
	}
	v := g.Admit("a.md", 10)
	if v.OK() {
		t.Fatal("5th write to a hot path was allowed")
	}
	if v.Code != CodeQuarantined {
		t.Fatalf("code = %q, want %q", v.Code, CodeQuarantined)
	}
	if v.Retry <= 0 {
		t.Fatal("Retry should carry the remaining cooldown")
	}
}

func TestQuarantineIsPerPath(t *testing.T) {
	now := time.Unix(1000, 0)
	g := New(testLimits(), clock(&now), plentyFree())

	for i := 0; i < 5; i++ {
		g.Admit("hot.md", 10)
		now = now.Add(time.Second)
	}
	if v := g.Admit("cold.md", 10); !v.OK() {
		t.Fatalf("an unrelated path was blocked: %s", v.Code)
	}
}

func TestWritesOutsideWindowDoNotCount(t *testing.T) {
	now := time.Unix(1000, 0)
	g := New(testLimits(), clock(&now), plentyFree())

	for i := 0; i < 3; i++ {
		g.Admit("a.md", 10)
		now = now.Add(4 * time.Minute) // each write ages the previous ones out
	}
	if v := g.Admit("a.md", 10); !v.OK() {
		t.Fatalf("slow writes tripped the counter: %s", v.Code)
	}
}

func TestZeroWritesDisablesTheCounter(t *testing.T) {
	now := time.Unix(1000, 0)
	l := testLimits()
	l.Writes = 0
	g := New(l, clock(&now), plentyFree())

	for i := 0; i < 100; i++ {
		if v := g.Admit("a.md", 10); !v.OK() {
			t.Fatalf("disabled counter still refused: %s", v.Code)
		}
	}
}

// ---- byte counters ---------------------------------------------------------

func byteLimits() Limits {
	return Limits{
		Writes:     1000, // high enough not to interfere
		PathBytes:  100,
		TotalBytes: 500,
		Window:     5 * time.Minute,
		Cooldown:   15 * time.Minute,
	}
}

// A single write must never trip the per-path byte counter, however large.
// Otherwise one legitimate 120 MB attachment quarantines its own path on the
// first upload.
func TestSingleLargeWriteDoesNotTripPathBytes(t *testing.T) {
	now := time.Unix(1000, 0)
	// Vault-wide accounting is disabled so this isolates the per-path rule.
	// In production the global ceiling is 2 GB and a single attachment cannot
	// approach it; here the test limits are tiny and it would fire first.
	l := byteLimits()
	l.TotalBytes = 0
	g := New(l, clock(&now), plentyFree())

	if v := g.Admit("big.pdf", 10_000); !v.OK() {
		t.Fatalf("one large write was refused: %s", v.Code)
	}
	now = now.Add(time.Second)
	if v := g.Admit("other.md", 1); !v.OK() {
		t.Fatalf("an unrelated path was blocked: %s", v.Code)
	}
}

func TestRepeatedWritesTripPathBytes(t *testing.T) {
	now := time.Unix(1000, 0)
	g := New(byteLimits(), clock(&now), plentyFree())

	g.Admit("big.pdf", 60) // 60 total, one write: under the 2-write rule
	now = now.Add(time.Second)
	g.Admit("big.pdf", 60) // 120 total across two writes: trips
	now = now.Add(time.Second)

	v := g.Admit("big.pdf", 1)
	if v.OK() {
		t.Fatal("repeated large writes to one path were allowed")
	}
	if v.Code != CodeQuarantined {
		t.Fatalf("code = %q, want %q", v.Code, CodeQuarantined)
	}
}

func TestTotalBytesThrottles(t *testing.T) {
	now := time.Unix(1000, 0)
	g := New(byteLimits(), clock(&now), plentyFree())

	// Spread across distinct paths so no per-path counter fires.
	g.Admit("a.md", 200)
	now = now.Add(time.Second)
	g.Admit("b.md", 200)
	now = now.Add(time.Second)
	g.Admit("c.md", 200) // 600 > 500: trips the vault-wide counter
	now = now.Add(time.Second)

	v := g.Admit("d.md", 1)
	if v.OK() {
		t.Fatal("vault-wide byte ceiling did not engage")
	}
	if v.Code != CodeThrottled {
		t.Fatalf("code = %q, want %q", v.Code, CodeThrottled)
	}
}

func TestZeroByteLimitsDisableThem(t *testing.T) {
	now := time.Unix(1000, 0)
	l := byteLimits()
	l.PathBytes, l.TotalBytes = 0, 0
	g := New(l, clock(&now), plentyFree())

	for i := 0; i < 20; i++ {
		if v := g.Admit("a.md", 1_000_000); !v.OK() {
			t.Fatalf("disabled byte counters still refused: %s", v.Code)
		}
		now = now.Add(time.Second)
	}
}

// ---- cooldown and pressure -------------------------------------------------

func TestQuarantineClearsAfterCooldown(t *testing.T) {
	now := time.Unix(1000, 0)
	g := New(testLimits(), clock(&now), plentyFree())

	for i := 0; i < 5; i++ {
		g.Admit("a.md", 10)
		now = now.Add(time.Second)
	}
	if v := g.Admit("a.md", 10); v.OK() {
		t.Fatal("expected the path to be quarantined")
	}

	now = now.Add(16 * time.Minute) // past the 15m cooldown
	if v := g.Admit("a.md", 10); !v.OK() {
		t.Fatalf("quarantine did not expire: %s", v.Code)
	}
}

// The local write path cannot refuse, so it asks whether anything is tripped
// and slows its commit cadence instead.
func TestPressureReportsTrippedState(t *testing.T) {
	now := time.Unix(1000, 0)
	g := New(testLimits(), clock(&now), plentyFree())

	if g.Pressure() {
		t.Fatal("Pressure is set on a fresh guard")
	}
	for i := 0; i < 5; i++ {
		g.Admit("a.md", 10)
		now = now.Add(time.Second)
	}
	if !g.Pressure() {
		t.Fatal("Pressure is not set while a path is quarantined")
	}

	now = now.Add(16 * time.Minute)
	if g.Pressure() {
		t.Fatal("Pressure stayed set after the cooldown expired")
	}
}

// ---- disk floor ------------------------------------------------------------

func TestDiskFloorRefusesBelowThreshold(t *testing.T) {
	now := time.Unix(1000, 0)
	l := testLimits()
	l.MinFreeBytes = 20 << 30
	g := New(l, clock(&now), func() (int64, error) { return 19 << 30, nil })

	v := g.Admit("a.md", 10)
	if v.OK() {
		t.Fatal("a write was allowed below the disk floor")
	}
	if v.Code != CodeDiskLow {
		t.Fatalf("code = %q, want %q", v.Code, CodeDiskLow)
	}
}

func TestDiskFloorAllowsAboveThreshold(t *testing.T) {
	now := time.Unix(1000, 0)
	l := testLimits()
	l.MinFreeBytes = 20 << 30
	g := New(l, clock(&now), func() (int64, error) { return 21 << 30, nil })

	if v := g.Admit("a.md", 10); !v.OK() {
		t.Fatalf("a write above the floor was refused: %s", v.Code)
	}
}

// A broken probe must not become a broken vault: if free space cannot be
// read, writes proceed. Failing closed here would take the vault down over a
// statfs error on a disk that is fine.
func TestDiskProbeFailureAllowsWrites(t *testing.T) {
	now := time.Unix(1000, 0)
	l := testLimits()
	l.MinFreeBytes = 20 << 30
	g := New(l, clock(&now), func() (int64, error) { return 0, errors.New("statfs failed") })

	if v := g.Admit("a.md", 10); !v.OK() {
		t.Fatalf("a failed probe blocked writes: %s", v.Code)
	}
}

func TestZeroFloorDisablesTheCheck(t *testing.T) {
	now := time.Unix(1000, 0)
	l := testLimits()
	l.MinFreeBytes = 0
	g := New(l, clock(&now), func() (int64, error) { return 1, nil })

	if v := g.Admit("a.md", 10); !v.OK() {
		t.Fatalf("disabled floor still refused: %s", v.Code)
	}
}
