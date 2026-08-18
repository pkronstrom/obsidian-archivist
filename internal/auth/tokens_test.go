package auth

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTokens(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tokens.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTokenOpensOnlyItsOwnVaults(t *testing.T) {
	path := writeTokens(t, `{
	  "tok-personal": {"label": "mac",   "vaults": ["personal"]},
	  "tok-both":     {"label": "admin", "vaults": ["personal", "work"]}
	}`)
	set, err := Load(path, "")
	if err != nil {
		t.Fatal(err)
	}

	p, ok := set.Lookup("tok-personal")
	if !ok {
		t.Fatal("a known token was rejected")
	}
	if !p.Opens("personal") {
		t.Error("tok-personal cannot open personal")
	}
	if p.Opens("work") {
		t.Error("tok-personal opens work: the scope is not enforced")
	}

	b, _ := set.Lookup("tok-both")
	if !b.Opens("personal") || !b.Opens("work") {
		t.Error("tok-both should open both")
	}
}

func TestUnknownTokenIsRejected(t *testing.T) {
	set, err := Load(writeTokens(t, `{"good": {"vaults": ["personal"]}}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := set.Lookup("bad"); ok {
		t.Error("an unknown token was accepted")
	}
	if _, ok := set.Lookup(""); ok {
		t.Error("an empty token was accepted; that is the 'Bearer ' bypass")
	}
}

func TestVisibleReturnsOnlyWhatTheTokenOpens(t *testing.T) {
	set, _ := Load(writeTokens(t, `{"tok": {"vaults": ["work"]}}`), "")
	p, _ := set.Lookup("tok")
	got := p.Visible([]string{"personal", "work", "archive"})
	if len(got) != 1 || got[0] != "work" {
		t.Errorf("Visible = %v, want [work]", got)
	}
}

// Creation is a capability, off by default, and never granted implicitly.
func TestCreateCapabilityDefaultsOff(t *testing.T) {
	set, _ := Load(writeTokens(t, `{
	  "plain":   {"vaults": ["personal"]},
	  "creator": {"vaults": ["personal"], "canCreateVaults": true}
	}`), "")

	plain, _ := set.Lookup("plain")
	if plain.CanCreateVaults {
		t.Error("creation must be off unless it is asked for")
	}
	creator, _ := set.Lookup("creator")
	if !creator.CanCreateVaults {
		t.Error("an explicit capability was dropped")
	}
}

// A wildcard exists so the single-user case does not need a line per vault, but
// it never grants creation.
func TestWildcardOpensEverythingButCannotCreate(t *testing.T) {
	set, _ := Load(writeTokens(t, `{"tok": {"vaults": ["*"]}}`), "")
	p, _ := set.Lookup("tok")
	if !p.Opens("anything") {
		t.Error(`"*" should open every vault`)
	}
	if p.CanCreateVaults {
		t.Error(`"*" must not imply the creation capability`)
	}
}

// With no tokens file, ARCHIVIST_TOKEN keeps a single-vault deployment working.
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
		t.Error("the bootstrap token must open every vault; scoping needs the tokens file")
	}
	if p.CanCreateVaults {
		t.Error("the bootstrap token must not be able to create vaults")
	}
}

func TestNoTokensAtAllIsAnError(t *testing.T) {
	if _, err := Load("", ""); err == nil {
		t.Fatal("a server with no tokens would serve an open vault")
	}
}

func TestATokenWithNoVaultsIsAnError(t *testing.T) {
	if _, err := Load(writeTokens(t, `{"tok": {"vaults": []}}`), ""); err == nil {
		t.Error("a token that opens nothing is a configuration mistake, not a valid entry")
	}
}
