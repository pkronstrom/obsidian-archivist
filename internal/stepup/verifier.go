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
	// floor is the step at construction. Every token starts refusing steps at or
	// below it, because a code spent just before a restart would otherwise be
	// accepted again by the empty map -- single use silently resetting on every
	// deploy. The lockout this creates is bounded by one step.
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
		floor:    now().Unix() / int64(Step/time.Second),
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
	return nil
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
