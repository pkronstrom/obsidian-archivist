// Package auth is who may open which vault.
//
// One mechanism yields both the authorisation check and the vault picker:
// GET /v1/vaults returns exactly what the presented token opens, so there is no
// separate admin concept and no second list to keep in step. This is what keeps
// isolation when an agent token leaks -- the property a single shared token
// silently destroys.
package auth

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// Scope catalog. Three verbs, deliberately no wildcard: a token that opens
// every vault is a convenience worth having, a token that can do everything is
// exactly the default this package exists to replace.
//
// There is no separate export or history scope. Both are folded into read for
// now; export is the first thing to split if an agent token ever leaks, because
// it hands over the entire vault history in one call.
const (
	ScopeRead   = "read"
	ScopeWrite  = "write"
	ScopeDelete = "delete"
)

// ValidScope reports whether s is in the catalog. Minting rejects anything
// else, so a typo in a scope name fails at mint time rather than silently
// granting nothing at request time.
func ValidScope(s string) bool {
	switch s {
	case ScopeRead, ScopeWrite, ScopeDelete:
		return true
	}
	return false
}

// Principal is what one token may do.
type Principal struct {
	// Label names the holder in logs and in `token list`. Never a secret.
	Label string `json:"label,omitempty"`
	// Vaults this token opens. "*" means every vault.
	Vaults []string `json:"vaults"`
	// Scopes this token holds, from the catalog above. No wildcard.
	Scopes []string `json:"scopes"`
	// CanCreateVaults is off unless asked for, and is never granted to a relay
	// or agent token. It is a capability over the SERVER rather than over a
	// vault, which is why it is a boolean here and not a scope.
	CanCreateVaults bool `json:"canCreateVaults,omitempty"`
	// CreatedAt is unix seconds, for `token list`.
	CreatedAt int64 `json:"createdAt,omitempty"`
	// ExpiresAt is unix seconds; 0 means never. Checked on every lookup, so an
	// expired token stops working without anyone editing the file.
	ExpiresAt int64 `json:"expiresAt,omitempty"`
}

func (p Principal) Opens(vault string) bool {
	for _, v := range p.Vaults {
		if v == "*" || v == vault {
			return true
		}
	}
	return false
}

// Can reports whether this token holds a scope. Exact match only.
func (p Principal) Can(scope string) bool {
	for _, s := range p.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// Expired reports whether this token is past its deadline. A zero ExpiresAt
// never expires.
func (p Principal) Expired(now time.Time) bool {
	return p.ExpiresAt != 0 && now.Unix() > p.ExpiresAt
}

// Visible filters a list of vault names to those this token opens, preserving
// order. This is the picker AND the authorisation check, from one fact.
func (p Principal) Visible(all []string) []string {
	out := []string{}
	for _, n := range all {
		if p.Opens(n) {
			out = append(out, n)
		}
	}
	return out
}

type Set struct {
	byToken   map[string]Principal
	bootstrap bool
}

// IsBootstrap reports that this set came from a single ARCHIVIST_TOKEN rather
// than a tokens file, so every token opens every vault. The caller warns.
func (s *Set) IsBootstrap() bool { return s.bootstrap }

// Load reads the tokens file, falling back to a single bootstrap token.
//
// path may be empty, in which case fallback must not be: a server with neither
// would compare against "Bearer " and accept any request -- authentication that
// looks like authentication and is not.
func Load(path, fallback string) (*Set, error) {
	if path == "" {
		if fallback == "" {
			return nil, errors.New(
				"auth: no credentials. Set ARCHIVIST_TOKENS to a tokens file, or " +
					"ARCHIVIST_TOKEN for a single unscoped token")
		}
		return &Set{
			byToken:   map[string]Principal{fallback: {Label: "bootstrap", Vaults: []string{"*"}}},
			bootstrap: true,
		}, nil
	}

	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("auth: reading %s: %w", path, err)
	}
	var raw map[string]Principal
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("auth: parsing %s: %w", path, err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("auth: %s defines no tokens", path)
	}
	for tok, p := range raw {
		if tok == "" {
			return nil, fmt.Errorf("auth: %s has an empty token key", path)
		}
		if len(p.Vaults) == 0 {
			return nil, fmt.Errorf("auth: token %q opens no vaults", p.Label)
		}
	}
	return &Set{byToken: raw}, nil
}

// Lookup finds the principal for a presented token, comparing in constant time.
//
// Every entry is compared even after a match, so the number of comparisons does
// not depend on which token was presented or whether one matched at all. The
// table is a handful of entries, so the cost is nothing and the timing signal
// is gone.
func (s *Set) Lookup(token string) (Principal, bool) {
	var found Principal
	ok := false
	for candidate, p := range s.byToken {
		if subtle.ConstantTimeCompare([]byte(candidate), []byte(token)) == 1 && token != "" {
			found, ok = p, true
		}
	}
	return found, ok
}

// NewSetForTest builds a token set in memory. Only for tests and for callers
// that already hold parsed principals.
func NewSetForTest(byToken map[string]Principal) *Set { return &Set{byToken: byToken} }
