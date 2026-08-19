package auth

import (
	"testing"
	"time"
)

func TestCanReportsScopeMembership(t *testing.T) {
	p := Principal{Scopes: []string{ScopeRead, ScopeWrite}}
	if !p.Can(ScopeRead) {
		t.Error("read was granted and Can says otherwise")
	}
	if p.Can(ScopeDelete) {
		t.Error("delete was never granted; Can must not invent it")
	}
}

// Wildcards exist for vaults, deliberately not for scopes. A token that opens
// every vault is a convenience; a token that can do everything is the thing
// this package exists to stop being the default.
func TestScopesHaveNoWildcard(t *testing.T) {
	p := Principal{Scopes: []string{"*"}}
	if p.Can(ScopeWrite) {
		t.Error(`"*" must not grant a scope`)
	}
}

func TestExpiredIsFalseWhenNoExpiryIsSet(t *testing.T) {
	p := Principal{}
	if p.Expired(time.Unix(2000000000, 0)) {
		t.Error("a token with no expiry must never expire")
	}
}

func TestExpiredIsTrueAfterTheDeadline(t *testing.T) {
	p := Principal{ExpiresAt: 1000}
	if p.Expired(time.Unix(999, 0)) {
		t.Error("expired one second early")
	}
	if !p.Expired(time.Unix(1001, 0)) {
		t.Error("did not expire after the deadline")
	}
}

func TestValidScopeAcceptsOnlyTheCatalog(t *testing.T) {
	for _, s := range []string{ScopeRead, ScopeWrite, ScopeDelete} {
		if !ValidScope(s) {
			t.Errorf("%q is in the catalog but was rejected", s)
		}
	}
	for _, s := range []string{"", "admin", "*", "Read"} {
		if ValidScope(s) {
			t.Errorf("%q is not in the catalog but was accepted", s)
		}
	}
}
