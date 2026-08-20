package stepup_test

import (
	"errors"
	"testing"
	"time"

	"github.com/pkronstrom/obsidian-archivist/internal/stepup"
)

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time          { return c.now }
func (c *fakeClock) advance(d time.Duration) { c.now = c.now.Add(d) }

func TestAUsedCodeIsRefused(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1111111109, 0)}
	v := stepup.NewVerifier(clock.Now)
	// A fresh verifier refuses every step up to its construction step plus the
	// skew, so that a code spent just before a restart -- including one an
	// authenticator running fast produced -- cannot be replayed just after.
	// Move past that window before spending anything.
	clock.advance((stepup.SkewSteps + 1) * stepup.Step)
	code, err := stepup.Code(rfcSecret, clock.now)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Check("tokenhash", "work", rfcSecret, code); err != nil {
		t.Fatalf("first use: %v", err)
	}
	if err := v.Check("tokenhash", "work", rfcSecret, code); err == nil {
		t.Fatal("the same code was accepted twice")
	}
}

// One secret guards several capabilities, so a code seen while unlocking one
// vault must not be spendable on another.
func TestReplayIsRefusedAcrossVaults(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1111111109, 0)}
	v := stepup.NewVerifier(clock.Now)
	// A fresh verifier refuses every step up to its construction step plus the
	// skew, so that a code spent just before a restart -- including one an
	// authenticator running fast produced -- cannot be replayed just after.
	// Move past that window before spending anything.
	clock.advance((stepup.SkewSteps + 1) * stepup.Step)
	code, _ := stepup.Code(rfcSecret, clock.now)
	if err := v.Check("tokenhash", "work", rfcSecret, code); err != nil {
		t.Fatal(err)
	}
	if err := v.Check("tokenhash", "personal", rfcSecret, code); err == nil {
		t.Fatal("a used code was accepted for a second vault")
	}
}

// A restart must not resurrect a spent code. Grants dying with the process is
// intended; single-use quietly resetting is not.
func TestARestartDoesNotResurrectASpentCode(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1111111109, 0)}
	before := stepup.NewVerifier(clock.Now)
	clock.advance((stepup.SkewSteps + 1) * stepup.Step)
	code, _ := stepup.Code(rfcSecret, clock.now)
	if err := before.Check("tokenhash", "work", rfcSecret, code); err != nil {
		t.Fatal(err)
	}

	// Same instant, brand new verifier: the process restarted.
	after := stepup.NewVerifier(clock.Now)
	if err := after.Check("tokenhash", "work", rfcSecret, code); err == nil {
		t.Fatal("a code spent before the restart was accepted after it")
	}

	// The floor lifts as the window moves on, so the lockout is bounded by one
	// step rather than being permanent.
	clock.advance(2 * stepup.Step)
	fresh, _ := stepup.Code(rfcSecret, clock.now)
	if err := after.Check("tokenhash", "work", rfcSecret, fresh); err != nil {
		t.Fatalf("a code from a later step was refused after the restart: %v", err)
	}
}

func TestBackoffAfterThreeFailures(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1111111109, 0)}
	v := stepup.NewVerifier(clock.Now)
	// A fresh verifier refuses every step up to its construction step plus the
	// skew, so that a code spent just before a restart -- including one an
	// authenticator running fast produced -- cannot be replayed just after.
	// Move past that window before spending anything.
	clock.advance((stepup.SkewSteps + 1) * stepup.Step)
	for i := 0; i < 3; i++ {
		if err := v.Check("tokenhash", "work", rfcSecret, "000000"); err == nil {
			t.Fatalf("attempt %d: a wrong code was accepted", i)
		}
	}
	good, _ := stepup.Code(rfcSecret, clock.now)
	var cooling stepup.ErrCoolingDown
	if err := v.Check("tokenhash", "work", rfcSecret, good); !errors.As(err, &cooling) {
		t.Fatalf("a correct code was not refused by the cooldown; err = %v", err)
	}

	clock.advance(31 * time.Second)
	good, _ = stepup.Code(rfcSecret, clock.now)
	if err := v.Check("tokenhash", "work", rfcSecret, good); err != nil {
		t.Fatalf("after the first cooldown elapsed: %v", err)
	}
}

func TestBackoffIsPerTokenAndVault(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1111111109, 0)}
	v := stepup.NewVerifier(clock.Now)
	// A fresh verifier refuses every step up to its construction step plus the
	// skew, so that a code spent just before a restart -- including one an
	// authenticator running fast produced -- cannot be replayed just after.
	// Move past that window before spending anything.
	clock.advance((stepup.SkewSteps + 1) * stepup.Step)
	for i := 0; i < 3; i++ {
		v.Check("tokenhash", "work", rfcSecret, "000000")
	}
	good, _ := stepup.Code(rfcSecret, clock.now)
	if err := v.Check("tokenhash", "personal", rfcSecret, good); err != nil {
		t.Fatalf("personal was locked out by failures against work: %v", err)
	}
}

func TestCooldownLadderLengthens(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1111111109, 0)}
	v := stepup.NewVerifier(clock.Now)
	// A fresh verifier refuses every step up to its construction step plus the
	// skew, so that a code spent just before a restart -- including one an
	// authenticator running fast produced -- cannot be replayed just after.
	// Move past that window before spending anything.
	clock.advance((stepup.SkewSteps + 1) * stepup.Step)
	for i := 0; i < 3; i++ {
		v.Check("t", "work", rfcSecret, "000000")
	}
	clock.advance(31 * time.Second) // clears the 30s rung
	v.Check("t", "work", rfcSecret, "000000")

	clock.advance(31 * time.Second) // not enough for the 60s rung
	good, _ := stepup.Code(rfcSecret, clock.now)
	if err := v.Check("t", "work", rfcSecret, good); err == nil {
		t.Fatal("31s cleared the 60s rung; the ladder is not lengthening")
	}
	clock.advance(61 * time.Second)
	good, _ = stepup.Code(rfcSecret, clock.now)
	if err := v.Check("t", "work", rfcSecret, good); err != nil {
		t.Fatalf("after the 60s rung elapsed: %v", err)
	}
}

// Skew means the previous process would have accepted a code one step in the
// FUTURE -- an authenticator running slightly fast produces exactly that. A
// floor at the construction step alone leaves that code replayable across a
// restart, which is the same hole the floor exists to close.
func TestARestartDoesNotResurrectASkewedCode(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1111111109, 0)}
	before := stepup.NewVerifier(clock.Now)
	clock.advance((stepup.SkewSteps + 1) * stepup.Step)

	// A code from one step ahead: valid now, because skew accepts it.
	ahead, _ := stepup.Code(rfcSecret, clock.now.Add(stepup.Step))
	if err := before.Check("tokenhash", "work", rfcSecret, ahead); err != nil {
		t.Fatalf("a code one step ahead was refused before the restart: %v", err)
	}

	after := stepup.NewVerifier(clock.Now)
	if err := after.Check("tokenhash", "work", rfcSecret, ahead); err == nil {
		t.Fatal("a skewed code spent before the restart was accepted after it")
	}
}
