package watcher

import (
	"testing"
	"time"
)

func TestPressureWidensDebounceButKeepsTheCeiling(t *testing.T) {
	w := &Watcher{debounce: time.Second, maxDelay: 10 * time.Second}

	if got := w.effectiveDebounce(); got != time.Second {
		t.Fatalf("without pressure: %v, want 1s", got)
	}

	w.SetPressure(func() bool { return true }, 60*time.Second)

	got := w.effectiveDebounce()
	if got <= time.Second {
		t.Fatalf("under pressure the debounce did not widen: %v", got)
	}
	if got > 60*time.Second {
		t.Fatalf("debounce %v exceeded the ceiling 60s", got)
	}
}

// The bug recorded in New: an uncapped debounce under a steady write stream
// produced zero commits. Widening must never reintroduce it.
func TestWidenedDebounceStillHasAMaxDelay(t *testing.T) {
	w := &Watcher{debounce: time.Second, maxDelay: 10 * time.Second}
	w.SetPressure(func() bool { return true }, 60*time.Second)

	max := w.effectiveMaxDelay()
	if max <= 0 {
		t.Fatal("maxDelay was disabled under pressure")
	}
	if max < w.effectiveDebounce() {
		t.Fatalf("maxDelay %v is below the debounce %v, so it would fire on every event",
			max, w.effectiveDebounce())
	}
	if max > 20*time.Minute {
		t.Fatalf("maxDelay grew without bound: %v", max)
	}
}

// A nil probe means no guard is installed, so nothing defers.
func TestNilPressureLeavesCadenceAlone(t *testing.T) {
	w := &Watcher{debounce: time.Second, maxDelay: 10 * time.Second}

	if got := w.effectiveDebounce(); got != time.Second {
		t.Fatalf("effectiveDebounce = %v, want 1s", got)
	}
	if got := w.effectiveMaxDelay(); got != 10*time.Second {
		t.Fatalf("effectiveMaxDelay = %v, want 10s", got)
	}
}

// Pressure that clears must restore the normal cadence.
func TestCadenceReturnsWhenPressureClears(t *testing.T) {
	tripped := true
	w := &Watcher{debounce: time.Second, maxDelay: 10 * time.Second}
	w.SetPressure(func() bool { return tripped }, 60*time.Second)

	if w.effectiveDebounce() == time.Second {
		t.Fatal("debounce did not widen while tripped")
	}
	tripped = false
	if got := w.effectiveDebounce(); got != time.Second {
		t.Fatalf("debounce stayed widened after pressure cleared: %v", got)
	}
}
