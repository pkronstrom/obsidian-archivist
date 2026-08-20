package stepup_test

import (
	"testing"
	"time"

	"github.com/pkronstrom/obsidian-archivist/internal/stepup"
)

func TestAGrantExpiresAbsolutely(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1000, 0)}
	g := stepup.NewGrants(time.Minute, clock.Now)
	defer g.Close()
	g.Grant("tokenhash", "work")

	clock.advance(59 * time.Second)
	if !g.Held("tokenhash", "work") {
		t.Fatal("the grant lapsed inside its TTL")
	}
	clock.advance(2 * time.Second)
	if g.Held("tokenhash", "work") {
		t.Fatal("the grant outlived its TTL")
	}
}

func TestActivityDoesNotExtendAGrant(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1000, 0)}
	g := stepup.NewGrants(time.Minute, clock.Now)
	defer g.Close()
	g.Grant("tokenhash", "work")
	for i := 0; i < 5; i++ {
		clock.advance(20 * time.Second)
		g.Held("tokenhash", "work")
	}
	if g.Held("tokenhash", "work") {
		t.Fatal("checking the grant renewed it")
	}
}

func TestGrantsAreIsolatedByTokenAndVault(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1000, 0)}
	g := stepup.NewGrants(time.Minute, clock.Now)
	defer g.Close()
	g.Grant("alice", "work")
	if g.Held("alice", "personal") {
		t.Error("a grant for work covered personal")
	}
	if g.Held("bob", "work") {
		t.Error("alice's grant covered bob")
	}
}

// A stream must stop at the deadline, not at the next sweep. Real time here
// deliberately: this asserts the timer fires on its own, with nobody asking.
func TestExpiryClosesTheWatchChannelOnItsOwn(t *testing.T) {
	g := stepup.NewGrants(50*time.Millisecond, nil)
	defer g.Close()
	g.Grant("tokenhash", "work")
	lapsed := g.Watch("tokenhash", "work")

	select {
	case <-lapsed:
		t.Fatal("the channel closed while the grant was live")
	case <-time.After(10 * time.Millisecond):
	}

	select {
	case <-lapsed:
	case <-time.After(2 * time.Second):
		t.Fatal("the channel did not close at the deadline; expiry waits for a sweep")
	}
}

func TestWatchOnAnUngrantedKeyIsAlreadyClosed(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1000, 0)}
	g := stepup.NewGrants(time.Minute, clock.Now)
	defer g.Close()
	select {
	case <-g.Watch("nobody", "work"):
	case <-time.After(time.Second):
		t.Fatal("watching a key with no grant blocked instead of closing")
	}
}

// Re-granting must not close the previous watcher twice, and must not leave the
// old timer able to expire the new grant.
func TestRegrantingIsSafe(t *testing.T) {
	g := stepup.NewGrants(time.Minute, nil)
	defer g.Close()
	g.Grant("tokenhash", "work")
	first := g.Watch("tokenhash", "work")
	g.Grant("tokenhash", "work")

	select {
	case <-first:
	case <-time.After(time.Second):
		t.Fatal("the superseded watcher was never released")
	}
	if !g.Held("tokenhash", "work") {
		t.Fatal("re-granting dropped the new grant")
	}
}

func TestCloseReleasesEveryWatcher(t *testing.T) {
	g := stepup.NewGrants(time.Hour, nil)
	g.Grant("a", "work")
	g.Grant("b", "work")
	wa, wb := g.Watch("a", "work"), g.Watch("b", "work")
	g.Close()
	for i, ch := range []<-chan struct{}{wa, wb} {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatalf("watcher %d survived Close", i)
		}
	}
}

// Held-then-Watch is a TOCTOU window: a re-grant between the two calls hands
// back the successor's channel, so a request admitted under the first grant
// never learns that grant was replaced.
func TestHeldWatchIsAtomicAcrossARegrant(t *testing.T) {
	g := stepup.NewGrants(time.Minute, nil)
	defer g.Close()
	g.Grant("tokenhash", "work")

	lapsed, held := g.HeldWatch("tokenhash", "work")
	if !held {
		t.Fatal("the grant was not held")
	}
	g.Grant("tokenhash", "work") // supersede it

	select {
	case <-lapsed:
	case <-time.After(time.Second):
		t.Fatal("the channel handed to the admitted request outlived the grant it belonged to")
	}
}

func TestHeldWatchOnAnUngrantedKeyIsClosedAndNotHeld(t *testing.T) {
	g := stepup.NewGrants(time.Minute, nil)
	defer g.Close()
	lapsed, held := g.HeldWatch("nobody", "work")
	if held {
		t.Error("an ungranted key reported held")
	}
	select {
	case <-lapsed:
	case <-time.After(time.Second):
		t.Fatal("the channel for an ungranted key was not already closed")
	}
}
