// Package auth is who may open which vault.
//
// One mechanism yields both the authorisation check and the vault picker:
// GET /v1/vaults returns exactly what the presented token opens, so there is no
// separate admin concept and no second list to keep in step. This is what keeps
// isolation when an agent token leaks -- the property a single shared token
// silently destroys.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
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

// FormatVersion is the tokens-file schema.
//
// v1 was a bare object mapping a PLAINTEXT token to its principal. There is no
// migration path to v2 and deliberately so: converting would mean reading the
// secrets that v2 exists to stop storing. A v1 file is a startup error naming
// the recovery.
const FormatVersion = 2

// TokenPrefix makes a leaked token greppable -- in a log, a paste, a git diff.
// It is not a namespace and carries no meaning to the server.
const TokenPrefix = "arch_"

// file is the on-disk shape. Keys of Tokens are the lowercase hex sha256 of the
// token, so the file holds nothing replayable.
type file struct {
	V      int                  `json:"v"`
	Tokens map[string]Principal `json:"tokens"`
}

// Set is the live token table. Safe for concurrent use: the HTTP handlers read
// it on every request while the watcher replaces it wholesale.
type Set struct {
	mu        sync.RWMutex
	byHash    map[string]Principal
	bootstrap bool
	// now is a seam for testing expiry. Nil means time.Now.
	now func() time.Time
}

// New returns an empty set, for minting into.
func New() *Set { return &Set{byHash: map[string]Principal{}} }

// IsBootstrap reports that this set came from a single ARCHIVIST_TOKEN rather
// than a tokens file, so one token opens every vault. The caller warns.
func (s *Set) IsBootstrap() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.bootstrap
}

// HashToken is the storage key for a token. Exported because the CLI needs to
// name an entry for revocation without ever holding the secret.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Lookup resolves a presented token to its principal.
//
// This hashes and then does a map lookup, where v1 compared every entry in
// constant time. That is not a regression: the map is keyed by a sha256 digest,
// so recovering a token from lookup timing would require inverting the hash
// rather than guessing a string one byte at a time. The constant-time scan
// existed because the old table held the secrets themselves.
func (s *Set) Lookup(token string) (Principal, bool) {
	if token == "" {
		return Principal{}, false
	}
	s.mu.RLock()
	p, ok := s.byHash[HashToken(token)]
	now := s.now
	s.mu.RUnlock()
	if !ok {
		return Principal{}, false
	}
	if now == nil {
		now = time.Now
	}
	if p.Expired(now()) {
		return Principal{}, false
	}
	return p, true
}

// Mint generates a new token, records the principal against its hash, and
// returns the secret. The secret is returned exactly once and is never
// recoverable from the set afterwards.
func (s *Set) Mint(p Principal) (string, error) {
	if len(p.Vaults) == 0 {
		return "", errors.New("auth: a token must open at least one vault")
	}
	if len(p.Scopes) == 0 {
		return "", errors.New("auth: a token must hold at least one scope")
	}
	for _, sc := range p.Scopes {
		if !ValidScope(sc) {
			return "", fmt.Errorf("auth: %q is not a scope (read, write, delete)", sc)
		}
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("auth: generating a token: %w", err)
	}
	token := TokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	if p.CreatedAt == 0 {
		p.CreatedAt = time.Now().Unix()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byHash == nil {
		s.byHash = map[string]Principal{}
	}
	s.byHash[HashToken(token)] = p
	return token, nil
}

// Revoke removes one entry by its hash, reporting whether anything was removed.
func (s *Set) Revoke(hash string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.byHash[hash]
	delete(s.byHash, hash)
	return ok
}

// Entries returns a copy of the table, hash-keyed, for listing.
func (s *Set) Entries() map[string]Principal {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]Principal, len(s.byHash))
	for h, p := range s.byHash {
		out[h] = p
	}
	return out
}

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
		// Read and write, so a single-vault deployment syncs. Not delete and not
		// creation: those are worth the one command it takes to mint a token.
		return &Set{
			byHash: map[string]Principal{
				HashToken(fallback): {
					Label:  "bootstrap",
					Vaults: []string{"*"},
					Scopes: []string{ScopeRead, ScopeWrite},
				},
			},
			bootstrap: true,
		}, nil
	}

	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("auth: reading %s: %w", path, err)
	}
	var f file
	if err := json.Unmarshal(body, &f); err != nil {
		return nil, fmt.Errorf("auth: parsing %s: %w", path, err)
	}
	if f.V != FormatVersion {
		return nil, fmt.Errorf(
			"auth: %s is format v%d, this server needs v%d. v1 stored tokens in "+
				"plaintext and cannot be converted -- re-mint each one with "+
				"`archivist-server token add` and update the devices that hold them",
			path, f.V, FormatVersion)
	}
	if len(f.Tokens) == 0 {
		return nil, fmt.Errorf("auth: %s defines no tokens", path)
	}
	for hash, p := range f.Tokens {
		if hash == "" {
			return nil, fmt.Errorf("auth: %s has an empty token hash", path)
		}
		if len(p.Vaults) == 0 {
			return nil, fmt.Errorf("auth: token %q opens no vaults", p.Label)
		}
		if len(p.Scopes) == 0 {
			return nil, fmt.Errorf("auth: token %q holds no scopes", p.Label)
		}
		for _, sc := range p.Scopes {
			if !ValidScope(sc) {
				return nil, fmt.Errorf("auth: token %q has unknown scope %q", p.Label, sc)
			}
		}
	}
	return &Set{byHash: f.Tokens}, nil
}

// Save writes the table atomically: a temp file in the same directory, then a
// rename. A reader therefore sees either the whole old file or the whole new
// one, never a half-written one -- which is what lets the server watch this
// path without a lock.
func (s *Set) Save(path string) error {
	s.mu.RLock()
	f := file{V: FormatVersion, Tokens: make(map[string]Principal, len(s.byHash))}
	for h, p := range s.byHash {
		f.Tokens[h] = p
	}
	s.mu.RUnlock()

	body, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("auth: encoding tokens: %w", err)
	}
	body = append(body, '\n')

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tokens-*.json")
	if err != nil {
		return fmt.Errorf("auth: creating a temp file in %s: %w", dir, err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("auth: securing the temp file: %w", err)
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("auth: writing the temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("auth: closing the temp file: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("auth: replacing %s: %w", path, err)
	}
	return nil
}

// Replace swaps the whole table in one operation. The watcher calls this after
// a successful re-read, so a request either sees the old table or the new one.
func (s *Set) Replace(other *Set) {
	other.mu.RLock()
	next := make(map[string]Principal, len(other.byHash))
	for h, p := range other.byHash {
		next[h] = p
	}
	boot := other.bootstrap
	other.mu.RUnlock()

	s.mu.Lock()
	s.byHash, s.bootstrap = next, boot
	s.mu.Unlock()
}

// NewSetForTest builds a set from PLAINTEXT tokens, hashing each on the way in
// so callers keep writing the readable value they present as a bearer.
//
// Only for tests. Production sets come from Load or Mint, and neither ever sees
// a token it did not generate.
func NewSetForTest(byToken map[string]Principal) *Set {
	byHash := make(map[string]Principal, len(byToken))
	for tok, p := range byToken {
		byHash[HashToken(tok)] = p
	}
	return &Set{byHash: byHash}
}
