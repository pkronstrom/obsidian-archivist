package stepup

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrCoolingDown is a key inside its backoff window. Distinct from a wrong code
// so the caller can say when to try again rather than leaving a human retrying
// into a wall.
type ErrCoolingDown struct{ Retry time.Duration }

func (e ErrCoolingDown) Error() string {
	return fmt.Sprintf("too many failed codes; try again in %s", e.Retry.Round(time.Second))
}

// ErrBadCode is a code that did not verify, or one already spent.
var ErrBadCode = errors.New("that code is not valid")

// cooldowns is the ladder, entered on the third consecutive failure. A table
// rather than a formula with a cap bolted on: the shape is easier to read, and
// the numbers are the ones actually wanted rather than whatever a formula
// happens to produce.
var cooldowns = []time.Duration{
	30 * time.Second,
	60 * time.Second,
	90 * time.Second,
	180 * time.Second,
	360 * time.Second,
	720 * time.Second,
}

// freeAttempts is how many failures precede the ladder. The third failure is the
// one that triggers the first cooldown, which covers a fat-fingered human and a
// code typed as it expired without letting an overnight guess run.
const freeAttempts = 3

// evictAfter is how long past its cooldown a failure record is kept. Well beyond
// the longest rung, so eviction never resets a ladder somebody is still climbing.
const evictAfter = 2 * time.Hour

type failure struct {
	count int
	until time.Time
}

// Verifier turns a raw TOTP check into an authorisation decision: single use,
// and bounded guessing.
//
// Two pieces of state, deliberately keyed differently. Spent steps are keyed by
// TOKEN alone: one secret guards several capabilities, so a code seen while
// unlocking one must not be spendable on another. Failures are keyed by token
// AND vault, so fumbling one vault cannot lock a caller out of a second, and no
// caller can lock out another by failing on purpose.
type Verifier struct {
	now func() time.Time
	// floor is the highest step that could already have been spent when this
	// verifier was built. Every token starts refusing steps at or below it,
	// because a code spent just before a restart would otherwise be accepted
	// again by the empty map -- single use silently resetting on every deploy.
	//
	// It is construction step PLUS the skew, not the construction step alone.
	// Skew means the previous process would have accepted a code from one step
	// in the FUTURE, which an authenticator running slightly fast will produce;
	// a floor at the current step leaves exactly that code replayable.
	//
	// The lockout this creates is bounded by SkewSteps+1 steps, during which
	// every grant is gone anyway because they died with the process.
	floor int64

	mu       sync.Mutex
	usedStep map[string]int64   // token hash -> last accepted step
	failures map[string]failure // token hash + vault -> backoff state
}

func NewVerifier(now func() time.Time) *Verifier {
	if now == nil {
		now = time.Now
	}
	return &Verifier{
		now:      now,
		floor:    now().Unix()/int64(Step/time.Second) + SkewSteps,
		usedStep: map[string]int64{},
		failures: map[string]failure{},
	}
}

// Check verifies one code. A nil error means the caller may proceed. It does NOT
// create a grant: a one-shot authorisation must not.
func (v *Verifier) Check(tokenHash, vault, secret, presented string) error {
	now := v.now()
	key := tokenHash + "\x00" + vault

	v.mu.Lock()
	defer v.mu.Unlock()

	if f, ok := v.failures[key]; ok && now.Before(f.until) {
		return ErrCoolingDown{Retry: f.until.Sub(now)}
	}

	step, ok := Verify(secret, presented, now)
	if ok {
		last := v.usedStep[tokenHash]
		if last < v.floor {
			last = v.floor
		}
		if step <= last {
			// Correct, but already spent -- or from a window that closed before
			// this process started. Counted as a failure: replay is exactly what
			// the backoff should discourage.
			ok = false
		}
	}
	if !ok {
		v.noteFailure(key, now)
		return ErrBadCode
	}

	v.usedStep[tokenHash] = step
	delete(v.failures, key)
	v.evictLocked(now)
	return nil
}

// evictLocked drops state that can no longer affect a decision.
//
// Both maps are keyed by token hash, and tokens are minted and revoked over the
// life of a process, so without this every generation accumulates forever. Not
// a remote exhaustion vector -- only a token the server already accepted reaches
// Check -- but an unbounded map in a long-lived process is a leak either way.
//
// A used step outside the skew window can never be presented again, and a
// failure record whose cooldown has long passed is indistinguishable from no
// record at all.
func (v *Verifier) evictLocked(now time.Time) {
	cutoff := now.Unix()/int64(Step/time.Second) - (SkewSteps + 1)
	for h, step := range v.usedStep {
		if step < cutoff {
			delete(v.usedStep, h)
		}
	}
	stale := now.Add(-evictAfter)
	for k, f := range v.failures {
		if !f.until.IsZero() && f.until.Before(stale) {
			delete(v.failures, k)
		}
	}
}

func (v *Verifier) noteFailure(key string, now time.Time) {
	f := v.failures[key]
	f.count++
	if f.count >= freeAttempts {
		rung := f.count - freeAttempts
		if rung >= len(cooldowns) {
			rung = len(cooldowns) - 1
		}
		f.until = now.Add(cooldowns[rung])
	}
	v.failures[key] = f
}

// TrackedTokens reports how many tokens hold spent-step state, so a test can
// assert that eviction happens rather than trusting that it does.
func (v *Verifier) TrackedTokens() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.usedStep)
}
