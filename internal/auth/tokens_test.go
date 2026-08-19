package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
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

func writeTokens(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tokens.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func hashOf(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func TestLookupFindsAPrincipalByTheHashOfItsToken(t *testing.T) {
	path := writeTokens(t, `{"v":2,"tokens":{"`+hashOf("secret-a")+`":
		{"label":"mac","vaults":["personal"],"scopes":["read","write"]}}}`)
	set, err := Load(path, "")
	if err != nil {
		t.Fatal(err)
	}
	p, ok := set.Lookup("secret-a")
	if !ok {
		t.Fatal("a known token was rejected")
	}
	if p.Label != "mac" || !p.Opens("personal") || !p.Can(ScopeWrite) {
		t.Errorf("wrong principal: %+v", p)
	}
	if _, ok := set.Lookup("secret-b"); ok {
		t.Error("an unknown token was accepted")
	}
	if _, ok := set.Lookup(""); ok {
		t.Error("an empty token was accepted; that is the 'Bearer ' bypass")
	}
}

// The file must never contain a value that could be replayed as a credential.
func TestTheFileHoldsNoPlaintextToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.json")
	set := New()
	token, err := set.Mint(Principal{Label: "agent", Vaults: []string{"personal"},
		Scopes: []string{ScopeRead}})
	if err != nil {
		t.Fatal(err)
	}
	if err := set.Save(path); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), token) {
		t.Fatal("the minted token appears verbatim in the file")
	}
	if !strings.Contains(string(body), hashOf(token)) {
		t.Error("the file does not contain the token's hash either; nothing was stored")
	}
}

func TestAMintedTokenLooksUpImmediately(t *testing.T) {
	set := New()
	token, err := set.Mint(Principal{Vaults: []string{"personal"}, Scopes: []string{ScopeRead}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, TokenPrefix) {
		t.Errorf("token %q does not carry the %q prefix that makes it greppable in a leak", token, TokenPrefix)
	}
	if _, ok := set.Lookup(token); !ok {
		t.Error("a token minted into the set does not resolve")
	}
}

func TestExpiredTokensAreRejectedAtLookup(t *testing.T) {
	path := writeTokens(t, `{"v":2,"tokens":{"`+hashOf("old")+`":
		{"vaults":["personal"],"scopes":["read"],"expiresAt":1000}}}`)
	set, err := Load(path, "")
	if err != nil {
		t.Fatal(err)
	}
	set.now = func() time.Time { return time.Unix(2000, 0) }
	if _, ok := set.Lookup("old"); ok {
		t.Error("an expired token was accepted")
	}
	set.now = func() time.Time { return time.Unix(500, 0) }
	if _, ok := set.Lookup("old"); !ok {
		t.Error("a token inside its window was rejected")
	}
}

// v1 was plaintext. There is no migration: re-hashing would require reading
// secrets the new format deliberately cannot hold.
func TestAVersionOneFileIsARefusalToStart(t *testing.T) {
	path := writeTokens(t, `{"tok-personal": {"label":"mac","vaults":["personal"]}}`)
	_, err := Load(path, "")
	if err == nil {
		t.Fatal("a v1 file was accepted; those tokens are plaintext")
	}
	if !strings.Contains(err.Error(), "token add") {
		t.Errorf("the error must tell the operator how to recover, got: %v", err)
	}
	// A v1 file has no "v" key, so it decodes as 0. Reporting "v0" would send
	// the reader looking for a version that never shipped.
	if strings.Contains(err.Error(), "v0") {
		t.Errorf("the error names a version that never existed: %v", err)
	}
}

func TestRevokeRemovesATokenFromTheSet(t *testing.T) {
	set := New()
	token, _ := set.Mint(Principal{Vaults: []string{"personal"}, Scopes: []string{ScopeRead}})
	if !set.Revoke(hashOf(token)) {
		t.Fatal("revoking a present token reported nothing removed")
	}
	if _, ok := set.Lookup(token); ok {
		t.Error("a revoked token still resolves")
	}
	if set.Revoke("0000") {
		t.Error("revoking an absent hash reported a removal")
	}
}

func TestNoTokensAtAllIsAnError(t *testing.T) {
	if _, err := Load("", ""); err == nil {
		t.Fatal("a server with no tokens would serve an open vault")
	}
}

func TestATokenWithNoVaultsIsAnError(t *testing.T) {
	path := writeTokens(t, `{"v":2,"tokens":{"`+hashOf("t")+`":{"vaults":[],"scopes":["read"]}}}`)
	if _, err := Load(path, ""); err == nil {
		t.Error("a token that opens nothing is a configuration mistake, not a valid entry")
	}
}

func TestATokenWithNoScopesIsAnError(t *testing.T) {
	path := writeTokens(t, `{"v":2,"tokens":{"`+hashOf("t")+`":{"vaults":["personal"],"scopes":[]}}}`)
	if _, err := Load(path, ""); err == nil {
		t.Error("a token that can do nothing is a mistake, and reads as a lockout at request time")
	}
}

func TestBootstrapTokenOpensEverythingAndSaysSo(t *testing.T) {
	set, err := Load("", "bootstrap-secret")
	if err != nil {
		t.Fatal(err)
	}
	if !set.IsBootstrap() {
		t.Error("the caller must be able to tell it is running unscoped, so it can warn")
	}
	p, ok := set.Lookup("bootstrap-secret")
	if !ok {
		t.Fatal("the bootstrap token was rejected")
	}
	if !p.Opens("personal") || !p.Opens("work") {
		t.Error("the bootstrap token must open every vault")
	}
	if !p.Can(ScopeRead) || !p.Can(ScopeWrite) {
		t.Error("the bootstrap token must be able to sync a vault, which needs read and write")
	}
	if p.Can(ScopeDelete) || p.CanCreateVaults {
		t.Error("the bootstrap token must not delete or create; those need a minted token")
	}
}

func TestVisibleReturnsOnlyWhatTheTokenOpens(t *testing.T) {
	path := writeTokens(t, `{"v":2,"tokens":{"`+hashOf("t")+`":
		{"vaults":["work"],"scopes":["read"]}}}`)
	set, _ := Load(path, "")
	p, _ := set.Lookup("t")
	got := p.Visible([]string{"personal", "work", "archive"})
	if len(got) != 1 || got[0] != "work" {
		t.Errorf("Visible = %v, want [work]", got)
	}
}

func TestWildcardOpensEverythingButCannotCreate(t *testing.T) {
	path := writeTokens(t, `{"v":2,"tokens":{"`+hashOf("t")+`":
		{"vaults":["*"],"scopes":["read"]}}}`)
	set, _ := Load(path, "")
	p, _ := set.Lookup("t")
	if !p.Opens("anything") {
		t.Error(`"*" should open every vault`)
	}
	if p.CanCreateVaults {
		t.Error(`"*" must not imply the creation capability`)
	}
}
